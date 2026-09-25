package probe_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// newEchoSDKServerWithKillSwitch stands up a real go-sdk echo server wrapped in a
// middleware that fails the NEXT POST once when armed — a stand-in for a dropped
// or expired MCP session (the sidecar restarted, the server GC'd an idle
// session). arm() primes exactly one failure; the reconnect path must recover.
func newEchoSDKServerWithKillSwitch(t *testing.T) (srv *httptest.Server, arm func()) {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "echo-mcp", Version: "1.0.0"}, nil)
	s.AddTool(&mcp.Tool{Name: "echo", Description: "echoes its message", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &args)
			msg, _ := args["message"].(string)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + msg}}}, nil
		})
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)

	var failNext atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && failNext.CompareAndSwap(true, false) {
			// A 404 to a request carrying an Mcp-Session-Id is the server's
			// "that session is gone" signal — the go-sdk client surfaces it as a
			// transport error, which is exactly what a real drop looks like.
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		inner.ServeHTTP(w, r)
	})
	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, func() { failNext.Store(true) }
}

// TestSessionCache_ReconnectsAfterSessionDrops proves a persistent session is not
// a footgun: when the cached session drops mid-AgentSession, the next CallTool
// invalidates it, reopens a fresh session, and retries — transparently — instead
// of failing every call forever against a dead session.
func TestSessionCache_ReconnectsAfterSessionDrops(t *testing.T) {
	srv, arm := newEchoSDKServerWithKillSwitch(t)
	ctx := context.Background()

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	out, err := cache.CallTool(ctx, srv.URL, http.DefaultClient, "echo",
		map[string]any{"message": "a"}, probe.Credential{})
	require.NoError(t, err, "first call opens the session")
	require.Len(t, out.Content, 1)
	assert.Equal(t, "echo: a", out.Content[0].Text)

	// The next POST fails as if the session dropped; CallTool must recover.
	arm()
	out, err = cache.CallTool(ctx, srv.URL, http.DefaultClient, "echo",
		map[string]any{"message": "b"}, probe.Credential{})
	require.NoError(t, err, "a dropped session is transparently reconnected + retried")
	require.Len(t, out.Content, 1)
	assert.Equal(t, "echo: b", out.Content[0].Text)
}

// TestSessionCache_ConcurrentCallsShareOneSession proves the cache is safe for the
// runner's parallel tool dispatch: N goroutines racing on the first call for a URL
// open exactly ONE session (not N), and all calls succeed. Run under -race.
func TestSessionCache_ConcurrentCallsShareOneSession(t *testing.T) {
	srv, sessionCount := newStatefulSDKServer(t)
	ctx := context.Background()

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = cache.CallTool(ctx, srv.URL, http.DefaultClient, "select",
				map[string]any{"value": "adobe"}, probe.Credential{})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "concurrent call %d", i)
	}
	assert.Equal(t, 1, sessionCount(), "N concurrent first-calls open exactly one session")
}
