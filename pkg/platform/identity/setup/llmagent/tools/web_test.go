package tools_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent/tools"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
)

func TestFetchURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello body"))
	}))
	t.Cleanup(srv.Close)

	// FetchURLClient is the SSRF-guarded production client, which refuses
	// the loopback httptest server. Swap it for a plain client for the
	// duration of this test (FetchURLClient is the documented seam).
	old := tools.FetchURLClient
	tools.FetchURLClient = http.DefaultClient
	t.Cleanup(func() { tools.FetchURLClient = old })

	out, err := tools.FetchURLRun(context.Background(),
		json.RawMessage(`{"url":"`+srv.URL+`"}`))
	require.NoError(t, err, "FetchURLRun")
	assert.Contains(t, out, "hello body", "FetchURLRun output")
}

func TestFetchURLRejectsNonHTTP(t *testing.T) {
	_, err := tools.FetchURLRun(context.Background(),
		json.RawMessage(`{"url":"file:///etc/passwd"}`))
	require.Error(t, err, "expected error on file:// scheme")
}

func TestWebSearchHappy(t *testing.T) {
	old := tools.WebSearchClient
	tools.WebSearchClient = func(_ context.Context, query string, opts websearch.SearchOpts) ([]websearch.SearchResult, error) {
		return []websearch.SearchResult{
			{Title: "Result 1", URL: "https://example.com/1", Snippet: "first hit"},
			{Title: "Result 2", URL: "https://example.com/2", Snippet: "second hit"},
		}, nil
	}
	t.Cleanup(func() { tools.WebSearchClient = old })

	out, err := tools.WebSearchRun(context.Background(),
		json.RawMessage(`{"query":"github personal access token"}`))
	require.NoError(t, err, "WebSearchRun")
	assert.Contains(t, out, "example.com", "WebSearchRun output")
}

func TestWebSearchRejectsLongQuery(t *testing.T) {
	q := strings.Repeat("x", 201)
	_, err := tools.WebSearchRun(context.Background(),
		json.RawMessage(`{"query":"`+q+`"}`))
	require.Error(t, err, "expected error for query > 200 chars")
}

func TestWebSearchRequiresClient(t *testing.T) {
	old := tools.WebSearchClient
	tools.WebSearchClient = nil
	t.Cleanup(func() { tools.WebSearchClient = old })

	_, err := tools.WebSearchRun(context.Background(),
		json.RawMessage(`{"query":"something"}`))
	require.Error(t, err, "expected error when client is nil")
}
