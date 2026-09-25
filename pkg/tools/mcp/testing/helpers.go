package testing

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolNames collects the names from a tools slice (via the given accessor) into
// a set, so tests can assert order-independent membership without repeating the
// extraction loop. The go-sdk client does not guarantee tools/list ordering, so
// assertions must be "contains", never positional.
//
// Generic over the tool type (probe.Tool, a CLI result's tool, ...) so it stays
// in this leaf test-helper package without importing — and cycling with — the
// packages under test.
func ToolNames[T any](tools []T, name func(T) string) map[string]bool {
	s := make(map[string]bool, len(tools))
	for _, t := range tools {
		s[name(t)] = true
	}
	return s
}

// ToolHandler is one tool's tools/call implementation.
type ToolHandler = func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error)

// CallServerOpts configures NewCallServer.
type CallServerOpts struct {
	// Tools maps tool name → its call handler.
	Tools map[string]ToolHandler
	// RequireHeaderName/Value, when set, 401s any request lacking that header
	// value BEFORE the go-sdk session — for auth and reauth-retry tests. On a
	// reauth test the first (stale-token) request 401s, the client re-auths, and
	// the retried request carries the matching value.
	RequireHeaderName, RequireHeaderValue string
	// OnRequest, when set, is invoked with each request's Authorization header
	// (for tests asserting which credentials the server saw). Called for EVERY
	// request in the session (initialize, notifications, tools/call, delete), so
	// assert membership, not exact counts.
	OnRequest func(authorization string)
}

// NewCallServer stands up a REAL go-sdk Streamable-HTTP server scripting the
// tools/call path (unlike New, which focuses on tools/list). The caller must
// Close it (t.Cleanup(srv.Close)).
func NewCallServer(o CallServerOpts) *httptest.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-mcp", Version: "1.0.0"}, nil)
	for name, h := range o.Tools {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, h)
	}
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.OnRequest != nil {
			o.OnRequest(r.Header.Get("Authorization"))
		}
		if o.RequireHeaderName != "" && r.Header.Get(o.RequireHeaderName) != o.RequireHeaderValue {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sdk.ServeHTTP(w, r)
	}))
}

// StaticTool returns a ToolHandler that always yields result.
func StaticTool(result *mcp.CallToolResult) ToolHandler {
	return func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return result, nil }
}

// TextResult builds a single-text-block tools/call result.
func TextResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isError}
}
