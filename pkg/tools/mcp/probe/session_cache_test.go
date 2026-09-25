package probe_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// newStatefulSDKServer stands up a REAL go-sdk MCP server whose select/selected
// tools store and read a value keyed by the MCP session id (req.Session.ID()) —
// exactly like dedicated-mcp's per-session PermissionSystem selection store. A
// value set by one call is visible to a later call ONLY if the two share the
// same MCP session, so it is a direct proxy for the question that motivates the
// SessionCache: does the client reuse one session per AgentSession, or open a
// fresh one per tool call? It also counts distinct sessions (initialize
// handshakes) so a test can assert reuse structurally.
func newStatefulSDKServer(t *testing.T) (srv *httptest.Server, sessionCount func() int) {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "stateful-mcp", Version: "1.0.0"}, nil)
	objectSchema := map[string]any{"type": "object"}

	var mu sync.Mutex
	selected := map[string]string{} // sessionID -> value
	seen := map[string]bool{}       // distinct sessionIDs observed

	note := func(id string) {
		mu.Lock()
		seen[id] = true
		mu.Unlock()
	}

	s.AddTool(&mcp.Tool{Name: "select", Description: "selects a value for this session", InputSchema: objectSchema},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.Session.ID()
			note(id)
			var args map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &args)
			v, _ := args["value"].(string)
			mu.Lock()
			selected[id] = v
			mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "selected " + v}}}, nil
		})
	s.AddTool(&mcp.Tool{Name: "selected", Description: "returns the value selected for this session", InputSchema: objectSchema},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.Session.ID()
			note(id)
			mu.Lock()
			v := selected[id]
			mu.Unlock()
			if v == "" {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no selection for this session"}}, IsError: true}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: v}}}, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(seen)
	}
}

// TestSessionCache_ReusesSessionSoServerStatePersists is the regression test for
// the dedicated-mcp "no PermissionSystem selected" bug: the runner opened a fresh
// MCP session per tool call (probe.CallTool → openSession → Close), so a
// selection made on one call was invisible to the next because the server keys it
// by session id. A SessionCache reuses ONE session per (AgentSession, server), so
// server-side per-session state survives across tool calls.
func TestSessionCache_ReusesSessionSoServerStatePersists(t *testing.T) {
	srv, sessionCount := newStatefulSDKServer(t)
	ctx := context.Background()

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	_, err := cache.CallTool(ctx, srv.URL, http.DefaultClient, "select",
		map[string]any{"value": "adobe"}, probe.Credential{})
	require.NoError(t, err, "select through the cache")

	out, err := cache.CallTool(ctx, srv.URL, http.DefaultClient, "selected",
		map[string]any{}, probe.Credential{})
	require.NoError(t, err, "selected through the cache")
	require.False(t, out.IsError, "selection must persist across calls on the same cached session")
	require.Len(t, out.Content, 1)
	assert.Equal(t, "adobe", out.Content[0].Text, "the value selected on the first call is visible on the second")

	assert.Equal(t, 1, sessionCount(), "both calls share exactly one MCP session (no fresh session per call)")
}
