import type { UIActionResult } from "@mcp-ui/client";

// InteractRequest is the POST /interact body for one widget-proposed action.
// Session-scoped: NO artifactId (mirrors SessionView.tsx's compose-box
// onSend — see interact/handlers.go step 8's empty-ArtifactID branch, which
// mints the Via from viewurn.TypeSession on precisely that branch).
export interface InteractRequest {
  kind: "mcp_ui_action";
  payload: Record<string, unknown>;
}

// interactRequestFromUIAction flattens one @mcp-ui/client UIActionResult —
// `{type, payload:{...variant fields...}}` — into the FLAT wire shape
// pkg/channels/interact/mcp_ui_action.go's mcpUiActionPayload decodes:
// `{type, toolName, params, prompt, url, intent, message}` all at the top
// level. The two shapes differ (the client library nests every variant's
// fields one level down under `.payload`; the server's wire contract does
// not), so this is a pure shape translation, mirroring
// artifactview/ui/annotationSend.ts's interactRequestFromBundle. It is NOT a
// safety boundary: every field here is widget/MCP-server-authored and
// therefore untrusted — the server (clampAction + buildActionEnvelope)
// truncates and untrusted-delimits it before it ever reaches the LLM; this
// function's only job is getting the field names in the place the server
// expects them.
export function interactRequestFromUIAction(result: UIActionResult): InteractRequest {
  const variantFields = (result as { payload?: Record<string, unknown> }).payload ?? {};
  return {
    kind: "mcp_ui_action",
    payload: { type: result.type, ...variantFields },
  };
}

// --- appToolCall response -> CallToolResult mapping ------------------------
//
// Task 3's other half of the round-trip: SessionView.tsx POSTs
// /session/{ns}/{name}/app-tool-call and gets back an AppToolCallResponse
// (pkg/channels/channelevents/app_tool_call.go's wire shape, mirrored below), which
// this file maps to the CallToolResultLike that gets replied down to
// mcpuihost/McpUiHost.tsx's pending sendAppToolCall Promise. This function is
// pure (no fetch, no postMessage) so it's directly unit-testable; the
// listener in SessionView.tsx owns the fetch + reply plumbing around it.

// CallToolResultLike mirrors mcpuihost/McpUiHost.tsx's identically-named
// type and the plan's correlation protocol block verbatim — MUST stay in
// sync (duplicated, not imported, because this bundle and the mcpui-host
// bundle are built as two SEPARATE sandbox-isolated entry points; see
// index.tsx's doc comment for why they don't share a module graph).
export interface CallToolResultLike {
  content: Array<{ type: "text"; text: string }>;
  isError: boolean;
}

// AppToolCallResponse mirrors pkg/channels/channelevents/app_tool_call.go's
// AppToolCallResponse wire shape (JSON tags all-lower-case: status, result,
// isError, message). `result` is untyped on purpose: the runner encodes it
// via json.Marshal(tool.Result.Content) (a Go string), so after
// `Response.json()` decodes the whole body it is normally already a plain
// JS string — but this type doesn't assume that, since it's the server's
// json.RawMessage and this is the one place decoding it happens.
export interface AppToolCallResponse {
  status: string;
  result?: unknown;
  isError?: boolean;
  message?: string;
}

// decodeAppToolResult renders an "ok" AppToolCallResponse.result as widget
// text. r.json() has already parsed the outer response body, so `result` is
// normally a plain JS string already; the JSON.parse branch below only
// matters for a double-encoded string (defensive — never assume the
// runner's encoding can't change). Never throws: an object/array falls
// through to JSON.stringify, and any stringify failure (a cyclic value —
// impossible from a JSON-decoded body, but this must never throw) falls
// back to String(...).
function decodeAppToolResult(result: unknown): string {
  if (result === undefined || result === null) return "";
  if (typeof result === "string") {
    try {
      const parsed: unknown = JSON.parse(result);
      if (typeof parsed === "string") return parsed;
    } catch {
      // Not further JSON-encoded — the raw string IS the text.
    }
    return result;
  }
  try {
    return JSON.stringify(result);
  } catch {
    return String(result);
  }
}

// appToolResponseToCallResult maps every AppToolCallResponse.Status the
// server can send to the in-widget CallToolResultLike SessionView.tsx
// replies down with. `not_found` is deliberately NOT handled here — that
// status means "fall back to /interact", a decision the LISTENER makes
// (it needs to issue a second fetch), not this pure mapper.
export function appToolResponseToCallResult(resp: AppToolCallResponse): CallToolResultLike {
  switch (resp.status) {
    case "ok":
      return { content: [{ type: "text", text: decodeAppToolResult(resp.result) }], isError: resp.isError === true };
    case "requires_approval":
      return {
        content: [{ type: "text", text: resp.message || "Approval requested — pending; ask in chat for the result." }],
        isError: true,
      };
    case "denied":
      return { content: [{ type: "text", text: resp.message || "Not authorized to run this action." }], isError: true };
    case "rate_limited":
      return { content: [{ type: "text", text: resp.message || "Rate limit reached; try again shortly." }], isError: true };
    case "error":
    default:
      return { content: [{ type: "text", text: resp.message || "The action could not be completed." }], isError: true };
  }
}
