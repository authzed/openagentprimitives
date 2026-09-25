// Package testing provides an httptest-backed fake MCP server for probe,
// dispatch, controller, and CLI tests.
//
// The happy path is served by a REAL go-sdk Streamable-HTTP server, because the
// probe client speaks the full session handshake (initialize → Mcp-Session-Id →
// notifications/initialized) and would reject a stateless JSON-RPC responder
// with "invalid during session initialization". Fault-injection behaviors
// (Status / RawBody / JSONRPCError / SSEResponse) short-circuit that session
// with a raw response, so tests can still exercise how the client surfaces
// HTTP-status, malformed and JSON-RPC errors.
//
// Because this package's name shadows the stdlib "testing" package, all
// consumers must import it with an alias:
//
//	import mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
package testing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Annotations mirrors tool annotation hints for the fake server.
type Annotations struct {
	DestructiveHint       bool            `json:"destructiveHint,omitempty"`
	ReadOnlyHint          bool            `json:"readOnlyHint,omitempty"`
	IdempotentHint        bool            `json:"idempotentHint,omitempty"`
	OpenWorldHint         bool            `json:"openWorldHint,omitempty"`
	Title                 string          `json:"title,omitempty"`
	MaliciousActivityHint bool            `json:"maliciousActivityHint,omitempty"`
	Attribution           []string        `json:"attribution,omitempty"`
	InputMetadata         json.RawMessage `json:"inputMetadata,omitempty"`
	ReturnMetadata        json.RawMessage `json:"returnMetadata,omitempty"`
}

// Tool is the MCP tools/list result shape.
type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  *Annotations    `json:"annotations,omitempty"`
}

// ServerInfo is the fake server's identity for the initialize handshake.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Behavior controls per-test responses.
type Behavior struct {
	// Tools returned on success. If a fault is set, Tools is ignored.
	Tools []Tool
	// ServerInfo, when non-nil, sets the go-sdk server's advertised identity.
	// When nil the server reports an empty implementation identity.
	ServerInfo *ServerInfo
	// Status overrides the HTTP status (default 200). A non-200 value faults the
	// session at the first (initialize) request.
	Status int
	// RawBody overrides the response body (used for malformed-JSON tests). Faults
	// the session.
	RawBody string
	// JSONRPCError, when non-empty, returns a JSON-RPC error envelope. Faults the
	// session.
	JSONRPCError string
	// SSEResponse, when non-empty, overrides the response body with the given raw
	// SSE bytes and sets Content-Type: text/event-stream. Highest-priority fault.
	SSEResponse string
	// RequireHeader, when set, 401s any request lacking that header value (checked
	// on the happy AND fault paths, before anything else).
	RequireHeaderName  string
	RequireHeaderValue string
}

// Server wraps an httptest.Server with mutable behavior.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	behavior Behavior
	sdk      http.Handler // real go-sdk Streamable-HTTP handler for happy sessions
	// LastHeader captures the last request's headers for test assertions.
	// NOTE: a go-sdk session makes several requests (initialize, notifications,
	// tools/list, and a DELETE on close), so this is the LAST of them — assert on
	// headers the client sets on every request (e.g. Authorization), not on a
	// per-method header like Content-Type. Safe to read after the request-making
	// call has fully returned. Concurrent in-flight requests are NOT safe — use
	// one Server per concurrent test.
	LastHeader http.Header
}

// New starts a fake server with the given initial behavior.
func New(b Behavior) *Server {
	s := &Server{behavior: b}
	s.rebuildSDK()
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// SetBehavior updates the server's behavior atomically (rebuilding the go-sdk
// handler so a subsequent session reflects the new tools/identity).
func (s *Server) SetBehavior(b Behavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.behavior = b
	s.rebuildSDK()
}

// rebuildSDK constructs the go-sdk Streamable-HTTP handler for the current
// happy-path tools + identity. Caller holds s.mu (or is in New before publish).
func (s *Server) rebuildSDK() {
	impl := &mcp.Implementation{}
	if s.behavior.ServerInfo != nil {
		impl.Name, impl.Version = s.behavior.ServerInfo.Name, s.behavior.ServerInfo.Version
	}
	srv := mcp.NewServer(impl, nil)
	for _, tl := range s.behavior.Tools {
		srv.AddTool(sdkTool(tl), echoHandler)
	}
	s.sdk = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.LastHeader = r.Header.Clone()
	b := s.behavior
	sdk := s.sdk
	s.mu.Unlock()

	// Header-auth gate applies to both the happy and fault paths.
	if b.RequireHeaderName != "" {
		if got := r.Header.Get(b.RequireHeaderName); got != b.RequireHeaderValue {
			http.Error(w, fmt.Sprintf("expected %s = %q, got %q",
				b.RequireHeaderName, b.RequireHeaderValue, got), http.StatusUnauthorized)
			return
		}
	}

	// A configured fault short-circuits the go-sdk session with a raw response,
	// so the client's initialize hits the fault and surfaces the error.
	if fault := faultResponse(b); fault != nil {
		fault(w)
		return
	}

	// Happy path: the real go-sdk Streamable-HTTP session handler.
	sdk.ServeHTTP(w, r)
}

// faultResponse returns a raw-response writer when Behavior configures a fault,
// else nil (happy path). Priority: SSE > non-200 status > raw body > JSON-RPC
// error, mirroring the pre-go-sdk fake's precedence.
func faultResponse(b Behavior) func(http.ResponseWriter) {
	switch {
	case b.SSEResponse != "":
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(b.SSEResponse))
		}
	case b.Status != 0 && b.Status != http.StatusOK:
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(b.Status)
			_, _ = w.Write([]byte(b.RawBody))
		}
	case b.RawBody != "":
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(b.RawBody))
		}
	case b.JSONRPCError != "":
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]any{"code": -32000, "message": b.JSONRPCError},
			})
		}
	}
	return nil
}

// sdkTool converts a fake Tool to a go-sdk tool: standard hints go to the typed
// ToolAnnotations; SEP-1913 custom annotations (which ToolAnnotations can't
// carry) ride in the tool's _meta, matching probe.toolFromSDK's recovery.
func sdkTool(tl Tool) *mcp.Tool {
	t := &mcp.Tool{Name: tl.Name, Description: tl.Description, InputSchema: map[string]any{"type": "object"}}
	if len(tl.InputSchema) > 0 {
		var sch any
		if json.Unmarshal(tl.InputSchema, &sch) == nil {
			t.InputSchema = sch
		}
	}
	if len(tl.OutputSchema) > 0 {
		var sch any
		if json.Unmarshal(tl.OutputSchema, &sch) == nil {
			t.OutputSchema = sch
		}
	}
	if a := tl.Annotations; a != nil {
		dh, ow := a.DestructiveHint, a.OpenWorldHint
		t.Annotations = &mcp.ToolAnnotations{
			Title:           a.Title,
			ReadOnlyHint:    a.ReadOnlyHint,
			IdempotentHint:  a.IdempotentHint,
			DestructiveHint: &dh,
			OpenWorldHint:   &ow,
		}
		meta := mcp.Meta{}
		if a.MaliciousActivityHint {
			meta["maliciousActivityHint"] = true
		}
		if len(a.Attribution) > 0 {
			meta["attribution"] = a.Attribution
		}
		if len(a.InputMetadata) > 0 {
			meta["inputMetadata"] = json.RawMessage(a.InputMetadata)
		}
		if len(a.ReturnMetadata) > 0 {
			meta["returnMetadata"] = json.RawMessage(a.ReturnMetadata)
		}
		if len(meta) > 0 {
			t.Meta = meta
		}
	}
	return t
}

// echoHandler is a minimal tool handler; the shared fake is used mostly for
// tools/list, so a call returns a benign result. Call-shape tests use their own
// inline go-sdk servers.
func echoHandler(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
}
