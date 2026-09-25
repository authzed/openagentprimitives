package websearch_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
)

// findTool returns the named client tool from the FakeProvider.
func findTool(t *testing.T, p *websearch.FakeProvider, name string) agenttool.Tool {
	t.Helper()
	for _, ct := range p.ClientTools() {
		if ct.Name() == name {
			return ct
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func TestFakeProvider_BasicShape(t *testing.T) {
	p := websearch.NewFakeProvider()
	assert.Equal(t, "fake", p.Name(), "Name")
	tools := p.ClientTools()
	require.Len(t, tools, 2, "ClientTools count (web_search + web_fetch)")
	names := map[string]agenttool.Kind{}
	for _, ct := range tools {
		names[ct.Name()] = ct.Kind()
	}
	assert.Equal(t, agenttool.KindMeta, names["web_search"], "web_search Kind")
	assert.Equal(t, agenttool.KindMeta, names["web_fetch"], "web_fetch Kind")
}

func TestFakeProvider_SearchHappyPath(t *testing.T) {
	want := []websearch.SearchResult{
		{Title: "First", URL: "https://example.com/1", Snippet: "snippet 1"},
		{Title: "Second", URL: "https://example.com/2", Snippet: "snippet 2"},
	}
	p := websearch.NewFakeProvider()
	var sawQuery string
	var sawOpts websearch.SearchOpts
	p.SearchFn = func(_ context.Context, q string, opts websearch.SearchOpts) ([]websearch.SearchResult, error) {
		sawQuery = q
		sawOpts = opts
		return want, nil
	}

	tool := findTool(t, p, "web_search")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"golang stdlib","max_results":5}`), nil)
	require.NoError(t, err, "Execute")
	require.False(t, res.IsError, "unexpected IsError=true: %s", res.Content)
	assert.Equal(t, "golang stdlib", sawQuery, "SearchFn query")
	assert.Equal(t, 5, sawOpts.MaxResults, "SearchFn opts.MaxResults")

	// The returned content should be the marshaled results, prefixed.
	require.True(t, strings.HasPrefix(res.Content, "results: "), "Content missing 'results: ' prefix: %s", res.Content)
	payload := strings.TrimPrefix(res.Content, "results: ")
	var got []websearch.SearchResult
	require.NoError(t, json.Unmarshal([]byte(payload), &got), "unmarshal payload=%q", payload)
	require.Len(t, got, len(want), "result count")
	for i := range want {
		assert.Equal(t, want[i], got[i], "result[%d]", i)
	}
}

func TestFakeProvider_SearchError(t *testing.T) {
	p := websearch.NewFakeProvider()
	p.SearchFn = func(_ context.Context, _ string, _ websearch.SearchOpts) ([]websearch.SearchResult, error) {
		return nil, errors.New("backend exploded")
	}

	tool := findTool(t, p, "web_search")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"hi"}`), nil)
	require.NoError(t, err, "Execute returned go error (should be IsError result)")
	assert.True(t, res.IsError, "expected IsError=true")
	assert.Contains(t, res.Content, "backend exploded", "Content missing backend error")
}

func TestFakeProvider_SearchInvalidArgs(t *testing.T) {
	p := websearch.NewFakeProvider()
	p.SearchFn = func(_ context.Context, _ string, _ websearch.SearchOpts) ([]websearch.SearchResult, error) {
		t.Fatal("SearchFn should not be called when args fail to parse")
		return nil, nil
	}

	tool := findTool(t, p, "web_search")
	res, err := tool.Execute(context.Background(), json.RawMessage(`not-json`), nil)
	require.NoError(t, err, "Execute")
	assert.True(t, res.IsError, "expected IsError=true on bad JSON")
}

func TestFakeProvider_FetchHappyPath(t *testing.T) {
	want := &websearch.FetchResult{
		URL:         "https://example.com/page",
		ContentType: "text/html; charset=utf-8",
		Body:        "<html>hi</html>",
	}
	p := websearch.NewFakeProvider()
	var sawURL string
	p.FetchFn = func(_ context.Context, url string) (*websearch.FetchResult, error) {
		sawURL = url
		return want, nil
	}

	tool := findTool(t, p, "web_fetch")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/page"}`), nil)
	require.NoError(t, err, "Execute")
	require.False(t, res.IsError, "unexpected IsError=true: %s", res.Content)
	assert.Equal(t, "https://example.com/page", sawURL, "FetchFn url")

	require.True(t, strings.HasPrefix(res.Content, "result: "), "Content missing 'result: ' prefix: %s", res.Content)
	payload := strings.TrimPrefix(res.Content, "result: ")
	var got websearch.FetchResult
	require.NoError(t, json.Unmarshal([]byte(payload), &got), "unmarshal payload=%q", payload)
	assert.Equal(t, *want, got)
}

func TestFakeProvider_FetchError(t *testing.T) {
	p := websearch.NewFakeProvider()
	p.FetchFn = func(_ context.Context, _ string) (*websearch.FetchResult, error) {
		return nil, errors.New("dns failure")
	}

	tool := findTool(t, p, "web_fetch")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"https://x"}`), nil)
	require.NoError(t, err, "Execute returned go error")
	assert.True(t, res.IsError, "expected IsError=true")
	assert.Contains(t, res.Content, "dns failure", "Content missing backend error")
}

// TestFakeProvider_NoFunctionConfigured locks in the two
// "tool unconfigured" paths together: each tool with its respective
// FN nil should return IsError with a clear message.
func TestFakeProvider_NoFunctionConfigured(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		args     string
		wantMsg  string
	}{
		{
			name:     "web_search with nil SearchFn: IsError, mentions 'no SearchFn configured'",
			toolName: "web_search",
			args:     `{"query":"x"}`,
			wantMsg:  "no SearchFn configured",
		},
		{
			name:     "web_fetch with nil FetchFn: IsError, mentions 'no FetchFn configured'",
			toolName: "web_fetch",
			args:     `{"url":"https://x"}`,
			wantMsg:  "no FetchFn configured",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := websearch.NewFakeProvider()
			tool := findTool(t, p, tc.toolName)
			res, err := tool.Execute(context.Background(), json.RawMessage(tc.args), nil)
			require.NoError(t, err, "Execute returned go error")
			assert.True(t, res.IsError, "expected IsError=true")
			assert.Contains(t, res.Content, tc.wantMsg, "expected clear error message")
		})
	}
}

func TestFakeProvider_FetchNilResult(t *testing.T) {
	p := websearch.NewFakeProvider()
	p.FetchFn = func(_ context.Context, _ string) (*websearch.FetchResult, error) {
		return nil, nil
	}

	tool := findTool(t, p, "web_fetch")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"https://x"}`), nil)
	require.NoError(t, err, "Execute")
	assert.True(t, res.IsError, "expected IsError=true on nil result")
}

func TestFakeProvider_Schemas(t *testing.T) {
	p := websearch.NewFakeProvider()
	for _, ct := range p.ClientTools() {
		var schema map[string]any
		err := json.Unmarshal(ct.InputSchema(), &schema)
		if !assert.NoError(t, err, "%s: invalid InputSchema JSON", ct.Name()) {
			continue
		}
		assert.Equal(t, "object", schema["type"], "%s: schema type", ct.Name())
		assert.NotEmpty(t, ct.Description(), "%s: empty Description", ct.Name())
	}
}
