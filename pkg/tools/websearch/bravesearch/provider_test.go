package bravesearch_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch/bravesearch"
)

// findTool returns the named client tool from p.
func findTool(t *testing.T, p websearch.Provider, name string) agenttool.Tool {
	t.Helper()
	for _, ct := range p.ClientTools() {
		if ct.Name() == name {
			return ct
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func newTestProvider(t *testing.T, httpClient *http.Client, apiKey string) websearch.Provider {
	t.Helper()
	p, err := bravesearch.Backend{}.New(websearch.Deps{HTTPClient: httpClient, APIKey: apiKey})
	require.NoError(t, err, "Backend.New")
	return p
}

func TestEffectiveBaseURL(t *testing.T) {
	t.Run("no override: DefaultBaseURL", func(t *testing.T) {
		assert.Equal(t, bravesearch.DefaultBaseURL, bravesearch.EffectiveBaseURL())
	})
	t.Run("BRAVE_SEARCH_BASE_URL set: overrides", func(t *testing.T) {
		t.Setenv("BRAVE_SEARCH_BASE_URL", "https://proxy.example/search")
		assert.Equal(t, "https://proxy.example/search", bravesearch.EffectiveBaseURL())
	})
}

func TestProvider_ClientTools(t *testing.T) {
	p := newTestProvider(t, http.DefaultClient, "k")
	tools := p.ClientTools()
	require.Len(t, tools, 2, "ClientTools count (web_search + web_fetch)")
	names := map[string]agenttool.Kind{}
	for _, ct := range tools {
		names[ct.Name()] = ct.Kind()
	}
	assert.Equal(t, agenttool.KindMeta, names["web_search"], "web_search Kind")
	assert.Equal(t, agenttool.KindMeta, names["web_fetch"], "web_fetch Kind")
}

func TestProvider_SearchHappyPath(t *testing.T) {
	var sawToken, sawQuery, sawCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Subscription-Token")
		sawQuery = r.URL.Query().Get("q")
		sawCount = r.URL.Query().Get("count")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"web": {
				"results": [
					{"title": "First", "url": "https://example.com/1", "description": "snippet 1"},
					{"title": "Second", "url": "https://example.com/2", "description": "snippet 2"}
				]
			}
		}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BRAVE_SEARCH_BASE_URL", srv.URL)

	p := newTestProvider(t, srv.Client(), "secret-token")
	tool := findTool(t, p, "web_search")

	res, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"golang stdlib","max_results":5}`), nil)
	require.NoError(t, err, "Execute")
	require.False(t, res.IsError, "unexpected IsError=true: %s", res.Content)

	assert.Equal(t, "secret-token", sawToken, "X-Subscription-Token header")
	assert.Equal(t, "golang stdlib", sawQuery, "q query param")
	assert.Equal(t, "5", sawCount, "count query param")

	require.True(t, strings.HasPrefix(res.Content, "results: "), "Content missing 'results: ' prefix: %s", res.Content)
	payload := strings.TrimPrefix(res.Content, "results: ")
	var got []websearch.SearchResult
	require.NoError(t, json.Unmarshal([]byte(payload), &got), "unmarshal payload=%q", payload)
	require.Len(t, got, 2, "result count")
	assert.Equal(t, websearch.SearchResult{Title: "First", URL: "https://example.com/1", Snippet: "snippet 1"}, got[0])
	assert.Equal(t, websearch.SearchResult{Title: "Second", URL: "https://example.com/2", Snippet: "snippet 2"}, got[1])
}

// TestProvider_SearchCountClamps pins that an out-of-range max_results
// clamps to the API's documented cap rather than being sent verbatim.
func TestProvider_SearchCountClamps(t *testing.T) {
	cases := []struct {
		name      string
		maxResult int
		wantCount string
	}{
		{name: "zero: clamps to 20", maxResult: 0, wantCount: "20"},
		{name: "negative: clamps to 20", maxResult: -1, wantCount: "20"},
		{name: "over cap: clamps to 20", maxResult: 500, wantCount: "20"},
		{name: "in range: passed through", maxResult: 3, wantCount: "3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sawCount string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawCount = r.URL.Query().Get("count")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("BRAVE_SEARCH_BASE_URL", srv.URL)

			p := newTestProvider(t, srv.Client(), "k")
			tool := findTool(t, p, "web_search")
			args, _ := json.Marshal(map[string]any{"query": "q", "max_results": tc.maxResult})
			res, err := tool.Execute(context.Background(), args, nil)
			require.NoError(t, err, "Execute")
			require.False(t, res.IsError, "unexpected IsError=true: %s", res.Content)
			assert.Equal(t, tc.wantCount, sawCount)
		})
	}
}

func TestProvider_SearchUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad token"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BRAVE_SEARCH_BASE_URL", srv.URL)

	p := newTestProvider(t, srv.Client(), "k")
	tool := findTool(t, p, "web_search")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q"}`), nil)
	require.NoError(t, err, "Execute returned go error (should be IsError result)")
	assert.True(t, res.IsError, "expected IsError=true")
	assert.Contains(t, res.Content, "401", "Content should mention upstream status")
}

func TestProvider_SearchInvalidArgs(t *testing.T) {
	p := newTestProvider(t, http.DefaultClient, "k")
	tool := findTool(t, p, "web_search")
	res, err := tool.Execute(context.Background(), json.RawMessage(`not-json`), nil)
	require.NoError(t, err, "Execute")
	assert.True(t, res.IsError, "expected IsError=true on bad JSON")
}

func TestProvider_FetchHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html>hi</html>"))
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(t, srv.Client(), "k")
	tool := findTool(t, p, "web_fetch")

	args, _ := json.Marshal(map[string]string{"url": srv.URL + "/page"})
	res, err := tool.Execute(context.Background(), args, nil)
	require.NoError(t, err, "Execute")
	require.False(t, res.IsError, "unexpected IsError=true: %s", res.Content)

	require.True(t, strings.HasPrefix(res.Content, "result: "), "Content missing 'result: ' prefix: %s", res.Content)
	payload := strings.TrimPrefix(res.Content, "result: ")
	var got websearch.FetchResult
	require.NoError(t, json.Unmarshal([]byte(payload), &got), "unmarshal payload=%q", payload)
	assert.Equal(t, srv.URL+"/page", got.URL)
	assert.Equal(t, "text/html; charset=utf-8", got.ContentType)
	assert.Equal(t, "<html>hi</html>", got.Body)
}

func TestProvider_FetchCapsBody(t *testing.T) {
	// One byte over the cap; the returned body must be truncated to it.
	huge := strings.Repeat("a", 300*1024+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(t, srv.Client(), "k")
	tool := findTool(t, p, "web_fetch")
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	res, err := tool.Execute(context.Background(), args, nil)
	require.NoError(t, err, "Execute")
	require.False(t, res.IsError, "unexpected IsError=true: %s", res.Content)

	payload := strings.TrimPrefix(res.Content, "result: ")
	var got websearch.FetchResult
	require.NoError(t, json.Unmarshal([]byte(payload), &got))
	assert.Len(t, got.Body, 300*1024, "body must be capped at maxFetchBytes")
}

func TestProvider_FetchUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(t, srv.Client(), "k")
	tool := findTool(t, p, "web_fetch")
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	res, err := tool.Execute(context.Background(), args, nil)
	require.NoError(t, err, "Execute returned go error")
	assert.True(t, res.IsError, "expected IsError=true")
	assert.Contains(t, res.Content, "404")
}

func TestProvider_FetchInvalidArgs(t *testing.T) {
	p := newTestProvider(t, http.DefaultClient, "k")
	tool := findTool(t, p, "web_fetch")
	res, err := tool.Execute(context.Background(), json.RawMessage(`not-json`), nil)
	require.NoError(t, err, "Execute")
	assert.True(t, res.IsError, "expected IsError=true on bad JSON")
}

func TestProvider_Schemas(t *testing.T) {
	p := newTestProvider(t, http.DefaultClient, "k")
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
