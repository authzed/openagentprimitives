package probe

import (
	"context"
	"errors"
	"net/http"
	"testing"

	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_Initialize_HappyPath(t *testing.T) {
	c, _ := newClientForBehavior(t, mcptest.Behavior{
		Tools:      []mcptest.Tool{{Name: "search_issues", Description: "find them"}},
		ServerInfo: &mcptest.ServerInfo{Name: "myserver", Version: "1.2.3"},
	})
	info, err := c.Initialize(context.Background(), "Authorization", "Bearer tok")
	require.NoError(t, err, "Initialize must succeed")
	assert.Equal(t, "myserver", info.Name)
	assert.Equal(t, "1.2.3", info.Version)
}

// TestClient_Initialize_ServerInfoOmitted verifies that a server that omits
// serverInfo from its initialize response yields zero ServerInfo with nil error.
func TestClient_Initialize_ServerInfoOmitted(t *testing.T) {
	c, _ := newClientForBehavior(t, mcptest.Behavior{
		Tools: []mcptest.Tool{{Name: "tool"}},
		// ServerInfo nil → fake server omits serverInfo from result.
	})
	info, err := c.Initialize(context.Background(), "", "")
	require.NoError(t, err, "Initialize: omitted serverInfo must return nil error")
	assert.Equal(t, ServerInfo{}, info, "zero ServerInfo when server omits it")
}

// TestClient_Initialize_JSONRPCError verifies JSON-RPC error responses are
// surfaced as errors by Initialize (same wrapping as ListTools).
func TestClient_Initialize_JSONRPCError(t *testing.T) {
	c, _ := newClientForBehavior(t, mcptest.Behavior{
		JSONRPCError: "method not found",
	})
	_, err := c.Initialize(context.Background(), "", "")
	require.Error(t, err, "Initialize must return error on JSON-RPC error")
	// go-sdk surfaces the server-sent JSON-RPC error message verbatim.
	assert.Contains(t, err.Error(), "method not found")
}

// TestClient_Initialize_HTTP500 verifies non-200 HTTP responses are surfaced
// as *HTTPError by Initialize (same as ListTools).
func TestClient_Initialize_HTTP500(t *testing.T) {
	c, _ := newClientForBehavior(t, mcptest.Behavior{
		Status:  500,
		RawBody: "internal error",
	})
	_, err := c.Initialize(context.Background(), "", "")
	require.Error(t, err, "Initialize must return error on HTTP 500")
	var httpErr *HTTPError
	require.True(t, errors.As(err, &httpErr), "errors.As(*HTTPError) must succeed; err=%v (%T)", err, err)
	assert.Equal(t, 500, httpErr.StatusCode)
}

// TestClient_Initialize_ViaSSE verifies Initialize works when the server
// responds with a text/event-stream (SSE) body, same as ListTools.
func TestClient_Initialize_ViaSSE(t *testing.T) {
	// Build an SSE-wrapped initialize response with serverInfo.
	sseBody := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"serverInfo\":{\"name\":\"sse-server\",\"version\":\"0.1.0\"}}}\n\n"
	srv := mcptest.New(mcptest.Behavior{
		SSEResponse: sseBody,
	})
	t.Cleanup(srv.Close)
	c := &Client{HTTP: http.DefaultClient, URL: srv.URL}
	info, err := c.Initialize(context.Background(), "", "")
	require.NoError(t, err, "Initialize must succeed via SSE")
	assert.Equal(t, "sse-server", info.Name)
	assert.Equal(t, "0.1.0", info.Version)
}
