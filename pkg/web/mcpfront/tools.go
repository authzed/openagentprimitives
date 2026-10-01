package mcpfront

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolErr/jsonResult/decodeArgs are local copies of the same three helpers
// pkg/tools/workshopmcp/server.go:225-293 defines for its own MCP server —
// the shared shape every leaf MCP server in this repo uses for a
// single-text-block JSON result, duplicated per package rather than factored
// out (see pkg/web/webui/chat, pkg/web/webui/agentui, pkg/web/admind for the
// same convention with writeJSON).

// toolErr builds a structured IsError CallToolResult carrying
// {"error": "<formatted message>"}.
func toolErr(format string, a ...any) *mcp.CallToolResult {
	b, err := json.Marshal(map[string]any{"error": fmt.Sprintf(format, a...)})
	if err != nil {
		b = fmt.Appendf(nil, "%q", format)
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

// jsonResult marshals body into a single-text-block SUCCESSFUL CallToolResult.
// body is always a plain struct/slice/map of JSON-safe values at every call
// site in this file, so json.Marshal cannot fail in practice — but per
// AGENTS.md's never-silently-drop-an-error rule, a hypothetical failure still
// surfaces as a structured tool error rather than an empty or malformed result.
func jsonResult(body any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return toolErr("marshaling result: %v", err), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

// decodeArgs decodes req's raw MCP tool-call arguments into out. A nil/empty
// Arguments (a tool called with no args at all) leaves out at its zero value
// rather than erroring, mirroring encoding/json's own treatment of a missing
// field.
func decodeArgs(req *mcp.CallToolRequest, out any) error {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return nil
	}
	return json.Unmarshal(req.Params.Arguments, out)
}

// readOnly is the Annotations every /mcp tool in this file carries: this
// surface is read-role only (Task 10 — a write/interact role is out of
// Phase-1 scope), and ReadOnlyHint tells a connecting client that up front.
var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true}

// unauthenticatedErr is returned by a tool handler when actingFrom finds no
// acting identity on the request context. Unreachable in production — every
// /mcp request passes through newBearerMiddleware first, which either attaches
// one or never reaches a tool handler at all — but a handler must still fail
// closed rather than panic if ever called directly (e.g. from a future
// in-process test harness that forgets the middleware).
func unauthenticatedErr() *mcp.CallToolResult {
	return toolErr("unauthenticated: no acting identity on request context")
}

// registerTools registers the /mcp server's read-role tool set against d.
// Every handler: resolve the acting identity the bearer middleware attached
// to the request context, decode args, run the op, and translate its result
// (or its denial) into a CallToolResult. The ops themselves (pkg/web/mcpfront
// ops.go) own every authorization decision; nothing here second-guesses them.
func registerTools(srv *mcp.Server, d Deps) {
	srv.AddTool(&mcp.Tool{
		Name: "list_sessions",
		Description: "List agent sessions this token's owner may read, optionally filtered by " +
			"agent (agentclass name) and/or state (\"running\" or \"ended\"). Each row reports its " +
			"channel origin (kind, key, and any kind-specific routing metadata) — combine the agent " +
			"filter with the channel fields to answer questions like \"find reviewbot's session for " +
			"PR #123\" by filtering to agent=reviewbot and matching the channel key/metadata yourself, " +
			"rather than needing a dedicated per-channel tool. Only sessions whose agentclass this " +
			"token's scope covers are returned; everything else is silently absent, not denied.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent": map[string]any{"type": "string", "description": "agentclass name filter"},
				"state": map[string]any{"type": "string", "enum": []any{"running", "ended"}, "description": "filter by lifecycle bucket; omit for all"},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxListSessionsLimit, "default": defaultListSessionsLimit, "description": "max rows to return"},
			},
		},
		Annotations: readOnly,
	}, handleListSessions(d))

	srv.AddTool(&mcp.Tool{
		Name:        "get_session",
		Description: "Get one session's summary (class, phase, start time, channel origin) by namespace and name. Use list_sessions first to find the (namespace, name) pair.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": map[string]any{"type": "string"},
				"name":      map[string]any{"type": "string"},
			},
			"required": []any{"namespace", "name"},
		},
		Annotations: readOnly,
	}, handleGetSession(d))

	srv.AddTool(&mcp.Tool{
		Name: "get_transcript",
		Description: "Read a session's conversation transcript (messages and plan cards, in order), " +
			"paginated with offset/limit. total is the full entry count regardless of the page " +
			"requested, so a caller can tell whether more remains.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": map[string]any{"type": "string"},
				"name":      map[string]any{"type": "string"},
				"offset":    map[string]any{"type": "integer", "minimum": 0, "default": 0},
				"limit":     map[string]any{"type": "integer", "minimum": 1, "default": defaultTranscriptLimit, "description": "max entries to return"},
			},
			"required": []any{"namespace", "name"},
		},
		Annotations: readOnly,
	}, handleGetTranscript(d))

	srv.AddTool(&mcp.Tool{
		Name: "search_memory",
		Description: "Search a session's (or, with no namespace/name given, every session this " +
			"token's owner may read) stored memory by free text. Combine with list_sessions/" +
			"get_session when you need to narrow to one agent or channel first. Each hit names the " +
			"session it came from, so a cross-session search stays attributable. When no search " +
			"provider is configured, searchUnavailable is true and hits is empty — that is not the " +
			"same as \"nothing matched\".",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":     map[string]any{"type": "string", "description": "free-text query"},
				"namespace": map[string]any{"type": "string", "description": "with name, restrict to one session"},
				"name":      map[string]any{"type": "string"},
				"limit":     map[string]any{"type": "integer", "minimum": 1, "default": 20},
			},
			"required": []any{"query"},
		},
		Annotations: readOnly,
	}, handleSearchMemory(d))

	srv.AddTool(&mcp.Tool{
		Name:        "list_artifacts",
		Description: "List the artifacts (generated files/renders) a session has produced, by namespace and name. Use get_artifact to fetch one's content.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": map[string]any{"type": "string"},
				"name":      map[string]any{"type": "string"},
			},
			"required": []any{"namespace", "name"},
		},
		Annotations: readOnly,
	}, handleListArtifacts(d))

	srv.AddTool(&mcp.Tool{
		Name: "get_artifact",
		Description: "Fetch one artifact's content by namespace, name, and artifactId (from " +
			"list_artifacts). content is populated only when the artifact's bytes are valid UTF-8 " +
			"text; a binary artifact (e.g. an image) reports empty content. Content over 256KiB is " +
			"truncated, with contentTruncated set to true.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace":  map[string]any{"type": "string"},
				"name":       map[string]any{"type": "string"},
				"artifactId": map[string]any{"type": "string"},
			},
			"required": []any{"namespace", "name", "artifactId"},
		},
		Annotations: readOnly,
	}, handleGetArtifact(d))
}

func handleListSessions(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		act, ok := actingFrom(ctx)
		if !ok {
			return unauthenticatedErr(), nil
		}
		var in ListSessionsIn
		if err := decodeArgs(req, &in); err != nil {
			return toolErr("list_sessions: decode arguments: %v", err), nil
		}
		out, err := opListSessions(ctx, d, act, in)
		if err != nil {
			return toolErr("list_sessions: %v", err), nil
		}
		return jsonResult(out)
	}
}

func handleGetSession(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		act, ok := actingFrom(ctx)
		if !ok {
			return unauthenticatedErr(), nil
		}
		var in GetSessionIn
		if err := decodeArgs(req, &in); err != nil {
			return toolErr("get_session: decode arguments: %v", err), nil
		}
		out, err := opGetSession(ctx, d, act, in)
		if err != nil {
			return toolErr("get_session: %v", err), nil
		}
		return jsonResult(out)
	}
}

func handleGetTranscript(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		act, ok := actingFrom(ctx)
		if !ok {
			return unauthenticatedErr(), nil
		}
		var in GetTranscriptIn
		if err := decodeArgs(req, &in); err != nil {
			return toolErr("get_transcript: decode arguments: %v", err), nil
		}
		out, err := opGetTranscript(ctx, d, act, in)
		if err != nil {
			return toolErr("get_transcript: %v", err), nil
		}
		return jsonResult(out)
	}
}

func handleSearchMemory(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		act, ok := actingFrom(ctx)
		if !ok {
			return unauthenticatedErr(), nil
		}
		var in SearchMemoryIn
		if err := decodeArgs(req, &in); err != nil {
			return toolErr("search_memory: decode arguments: %v", err), nil
		}
		out, err := opSearchMemory(ctx, d, act, in)
		if err != nil {
			return toolErr("search_memory: %v", err), nil
		}
		return jsonResult(out)
	}
}

func handleListArtifacts(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		act, ok := actingFrom(ctx)
		if !ok {
			return unauthenticatedErr(), nil
		}
		var in ListArtifactsIn
		if err := decodeArgs(req, &in); err != nil {
			return toolErr("list_artifacts: decode arguments: %v", err), nil
		}
		out, err := opListArtifacts(ctx, d, act, in)
		if err != nil {
			return toolErr("list_artifacts: %v", err), nil
		}
		return jsonResult(out)
	}
}

func handleGetArtifact(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		act, ok := actingFrom(ctx)
		if !ok {
			return unauthenticatedErr(), nil
		}
		var in GetArtifactIn
		if err := decodeArgs(req, &in); err != nil {
			return toolErr("get_artifact: decode arguments: %v", err), nil
		}
		out, err := opGetArtifact(ctx, d, act, in)
		if err != nil {
			return toolErr("get_artifact: %v", err), nil
		}
		return jsonResult(out)
	}
}
