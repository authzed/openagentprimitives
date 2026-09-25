package probe_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// newSDKServer stands up a REAL MCP go-sdk Streamable-HTTP server (the exact SDK
// the dedicated-mcp sidecar is built on) behind httptest. It requires the full
// session handshake (initialize → Mcp-Session-Id → notifications/initialized),
// so these tests prove our client speaks the spec — a stateless POST is rejected
// with "invalid during session initialization".
func newSDKServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-mcp", Version: "1.0.0"}, nil)
	objectSchema := map[string]any{"type": "object"}

	s.AddTool(&mcp.Tool{Name: "echo", Description: "echoes its message", InputSchema: objectSchema},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &args)
			msg, _ := args["message"].(string)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + msg}}}, nil
		})
	s.AddTool(&mcp.Tool{Name: "boom", Description: "always errors", InputSchema: objectSchema},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "kaboom"}}, IsError: true}, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestSDKServer_SessionHandshake_ListInitializeCall drives ListTools,
// Initialize, and CallTool (success + tool-error) against the go-sdk server, all
// over the session protocol our client now performs.
func TestSDKServer_SessionHandshake_ListInitializeCall(t *testing.T) {
	srv := newSDKServer(t)
	ctx := context.Background()
	// Loopback httptest server → pass a plain client; the SSRF-guarded default
	// would (correctly) refuse 127.0.0.1.
	c := &probe.Client{HTTP: http.DefaultClient, URL: srv.URL}

	tools, err := c.ListTools(ctx, "", "")
	require.NoError(t, err, "ListTools against a real go-sdk session server")
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	assert.True(t, names["echo"], "echo tool listed; got %v", names)
	assert.True(t, names["boom"], "boom tool listed; got %v", names)

	info, err := c.Initialize(ctx, "", "")
	require.NoError(t, err, "Initialize handshake")
	assert.Equal(t, "fake-mcp", info.Name, "server identity from initialize")

	out, err := probe.CallTool(ctx, srv.URL, http.DefaultClient, "echo",
		map[string]any{"message": "hi"}, "", "", nil)
	require.NoError(t, err, "CallTool echo")
	assert.False(t, out.IsError)
	require.Len(t, out.Content, 1)
	assert.Equal(t, "echo: hi", out.Content[0].Text, "args flow through the session")

	bad, err := probe.CallTool(ctx, srv.URL, http.DefaultClient, "boom",
		map[string]any{}, "", "", nil)
	require.NoError(t, err, "CallTool boom (transport ok, tool reports error)")
	assert.True(t, bad.IsError, "a tool-reported error surfaces as IsError")
}
