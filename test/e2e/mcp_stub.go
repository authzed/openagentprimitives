//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// ToolCallRecord captures one MCP tool invocation for after-the-fact
// assertions.
type ToolCallRecord struct {
	Name    string
	Args    map[string]any
	Headers http.Header // HTTP headers on the request that carried this call
}

// MCPStub is an httptest-backed MCP server. Tests register per-tool
// handlers via OnTool; the stub speaks the JSON-RPC 2.0 MCP wire
// protocol so the runner's real MCP client exercises the full
// request/response shape.
//
// NOT for production. Always paired with `t.Cleanup(m.Close)` — the
// harness registers the cleanup automatically when MCPStub is built
// via NewMCPStub.
type MCPStub struct {
	t             TB
	server        *httptest.Server
	mu            sync.Mutex
	tools         map[string]func(args map[string]any) any
	toolsWithMeta map[string]func(args map[string]any) (any, map[string]any)
	errors        map[string]toolError
	httpFail      map[string]httpFailure // per-tool injected HTTP-level failures
	calls         []ToolCallRecord
	toolList      []map[string]any // optional override for tools/list response
}

type toolError struct {
	Code    int
	Message string
}

// httpFailure is an injected HTTP-level failure for one tool: the status to
// answer tools/call with instead of a JSON-RPC response, and how many more
// requests it applies to. A NEGATIVE remaining means "until cleared" — see
// FailHTTP for why a count is not always the right knob.
type httpFailure struct {
	status    int
	remaining int
}

// NewMCPStub spins up the httptest.Server. Registers a t.Cleanup
// that calls Close — callers don't need to defer Close themselves.
func NewMCPStub(t TB) *MCPStub {
	m := &MCPStub{
		t:             t,
		tools:         map[string]func(args map[string]any) any{},
		toolsWithMeta: map[string]func(args map[string]any) (any, map[string]any){},
		errors:        map[string]toolError{},
		httpFail:      map[string]httpFailure{},
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	if c, ok := t.(interface{ Cleanup(func()) }); ok {
		c.Cleanup(m.Close)
	}
	return m
}

// URL returns the httptest endpoint, suitable for substituting into
// MCPServer.spec.server.url.
func (m *MCPStub) URL() string { return m.server.URL }

// Close shuts down the httptest server. Safe to call multiple times.
func (m *MCPStub) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server != nil {
		m.server.Close()
		m.server = nil
	}
}

// OnTool registers a handler returning a tool_result payload. The
// stub wraps the payload as JSON in the MCP response envelope.
func (m *MCPStub) OnTool(name string, handler func(args map[string]any) any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools[name] = handler
}

// OnToolWithMeta is like OnTool but also lets the test attach
// _meta.annotations to the tools/call response envelope (SEP-1913).
// The handler returns (resultPayload, metaAnnotations). A nil
// metaAnnotations means no _meta block is emitted.
func (m *MCPStub) OnToolWithMeta(name string, handler func(args map[string]any) (any, map[string]any)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolsWithMeta[name] = handler
}

// OnToolError registers a handler that returns a JSON-RPC error
// instead of a successful result. Wins over OnTool for the same name.
func (m *MCPStub) OnToolError(name string, code int, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors[name] = toolError{Code: code, Message: msg}
}

// FailTransport makes the next n tools/call requests for `name` return
// HTTP 500 (a transport-class failure, distinct from OnToolError's
// JSON-RPC app error). The runner's MCP dispatcher surfaces these as
// "mcp: HTTP 500: ...". Used to exercise toolguard's origin-level
// circuit breaker for sidecar health (the former sidecar degrade tracker
// was removed; circuit-breaking now goes through toolguard).
// The call is still recorded in Calls() before the 500 is written.
func (m *MCPStub) FailTransport(name string, n int) {
	m.FailHTTP(name, http.StatusInternalServerError, n)
}

// FailHTTP is FailTransport with the status chosen by the caller. It exists
// because the status is not always incidental: an upstream answering 401 is
// what the credential-update corroboration path classifies as an
// AUTHENTICATION failure (the provider catalog's authFailure.httpStatuses),
// while a 500 is deliberately NOT one — so a test about credentials cannot
// express its premise with FailTransport's hardcoded 500.
//
// n bounds how many tools/call requests the injection applies to. A NEGATIVE n
// means "until cleared", and that is usually what a test wants: ONE logical
// tool call can make several HTTP attempts (the MCP client re-resolves the
// credential and retries once on a 401), so a count is really a count of
// attempts, not of calls. Passing n == 0 clears any injection for `name`,
// which is how a test says "the upstream starts working now".
//
// The call is still recorded in Calls() before the status is written.
func (m *MCPStub) FailHTTP(name string, status, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n == 0 {
		delete(m.httpFail, name)
		return
	}
	m.httpFail[name] = httpFailure{status: status, remaining: n}
}

// Calls returns a copy of every tool invocation the stub observed.
func (m *MCPStub) Calls() []ToolCallRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ToolCallRecord, len(m.calls))
	copy(out, m.calls)
	return out
}

// AnnounceTools overrides the tool list returned by tools/list. If
// not called, tools/list synthesizes a minimal entry per registered
// handler. Use this to declare richer schemas / descriptions when a
// test exercises a tool's full MCP advertisement.
func (m *MCPStub) AnnounceTools(tools []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolList = tools
}

// handle is the http.HandlerFunc. Speaks JSON-RPC 2.0.
func (m *MCPStub) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	switch req.Method {
	case "initialize":
		writeJSONRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mcp-stub", "version": "0"},
		})
	case "tools/list":
		m.mu.Lock()
		tools := m.toolList
		if tools == nil {
			tools = []map[string]any{}
			for name := range m.tools {
				tools = append(tools, map[string]any{
					"name":        name,
					"description": "stub: " + name,
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			for name := range m.toolsWithMeta {
				tools = append(tools, map[string]any{
					"name":        name,
					"description": "stub: " + name,
					"inputSchema": map[string]any{"type": "object"},
				})
			}
		}
		m.mu.Unlock()
		writeJSONRPCResult(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		name, _ := req.Params["name"].(string)
		args, _ := req.Params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		m.mu.Lock()
		m.calls = append(m.calls, ToolCallRecord{Name: name, Args: args, Headers: r.Header.Clone()})
		// HTTP-level failure injection (FailTransport / FailHTTP): when an
		// injection is live for this tool, write its status and decrement
		// BEFORE the normal JSON-RPC handling. The status surfaces to the
		// runner as a transport-class error ("mcp: HTTP <status>") carrying the
		// code out-of-band on tool.Result.HTTPStatus. Keep this ahead of the
		// OnToolError / handler branches so an injected failure wins over a
		// registered app error / result. A negative remaining never decrements:
		// it stays live until the test clears it.
		if f, live := m.httpFail[name]; live && f.remaining != 0 {
			if f.remaining > 0 {
				f.remaining--
				m.httpFail[name] = f
			}
			m.mu.Unlock()
			http.Error(w, fmt.Sprintf("stub: injected HTTP %d for %s", f.status, name), f.status)
			return
		}
		if errSpec, ok := m.errors[name]; ok {
			m.mu.Unlock()
			writeJSONRPCError(w, req.ID, errSpec.Code, errSpec.Message)
			return
		}
		if metaHandler, ok := m.toolsWithMeta[name]; ok {
			m.mu.Unlock()
			payload, meta := metaHandler(args)
			jsonBlob, err := json.Marshal(payload)
			if err != nil {
				writeJSONRPCError(w, req.ID, -32603,
					"stub: marshal handler result for "+name+": "+err.Error())
				return
			}
			result := map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": string(jsonBlob)},
				},
			}
			if meta != nil {
				result["_meta"] = map[string]any{"annotations": meta}
			}
			writeJSONRPCResult(w, req.ID, result)
			return
		}
		handler, ok := m.tools[name]
		m.mu.Unlock()
		if !ok {
			writeJSONRPCError(w, req.ID, -32601, "tool not registered: "+name)
			return
		}
		result := handler(args)
		jsonBlob, err := json.Marshal(result)
		if err != nil {
			writeJSONRPCError(w, req.ID, -32603,
				"stub: marshal handler result for "+name+": "+err.Error())
			return
		}
		writeJSONRPCResult(w, req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(jsonBlob)},
			},
		})
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not supported: "+req.Method)
	}
}

func writeJSONRPCResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
}

func writeJSONRPCError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg},
	})
}
