// A session-keyed MCP server for tests that need to answer one question:
// does this caller reuse ONE MCP session across tool calls, or open a fresh
// session per call?
package testing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewSelectionServer starts a real go-sdk MCP server whose "select"/"selected"
// tools store and read a value keyed by the MCP session id (req.Session.ID()) —
// the same shape as the dedicated-mcp sidecar's per-session PermissionSystem
// selection. A value set by one call is visible to a later call ONLY if both
// calls ride the same MCP session, so calling select then selected is a direct
// behavioral probe for session reuse: "adobe" back means one shared session,
// IsError ("no selection for this session") means a fresh session per call.
//
// sessionCount reports how many DISTINCT sessions the server has served, so a
// test can also assert reuse structurally rather than only by its side effect.
//
// The caller owns the server: srv.Close (typically via t.Cleanup).
func NewSelectionServer() (srv *httptest.Server, sessionCount func() int) {
	s := mcp.NewServer(&mcp.Implementation{Name: "selection-mcp", Version: "1.0.0"}, nil)
	objectSchema := map[string]any{"type": "object"}

	var mu sync.Mutex
	selected := map[string]string{} // sessionID -> selected value
	seen := map[string]bool{}       // distinct sessionIDs observed

	note := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		seen[id] = true
	}

	s.AddTool(&mcp.Tool{
		Name:        "select",
		Description: "selects a value for this MCP session",
		InputSchema: objectSchema,
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.Session.ID()
		note(id)
		var args map[string]any
		_ = json.Unmarshal(req.Params.Arguments, &args)
		v, _ := args["value"].(string)
		mu.Lock()
		selected[id] = v
		mu.Unlock()
		return TextResult("selected "+v, false), nil
	})

	s.AddTool(&mcp.Tool{
		Name:        "selected",
		Description: "returns the value selected for this MCP session",
		InputSchema: objectSchema,
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.Session.ID()
		note(id)
		mu.Lock()
		v := selected[id]
		mu.Unlock()
		if v == "" {
			return TextResult("no selection for this session", true), nil
		}
		return TextResult(v, false), nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	srv = httptest.NewServer(handler)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(seen)
	}
}
