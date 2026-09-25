package channelevents

import "encoding/json"

// AppToolCallRequest is the body of a KindAppToolCall envelope, published
// browser/webd→runner to synchronously invoke an MCP-UI app-visible tool
// (an mcp-ui widget's window.postMessage tool call, relayed by webd). The
// runner replies via msg.Respond with an AppToolCallResponse.
type AppToolCallRequest struct {
	// ToolName is the runner's AppTools registry key — the LLM-prefixed
	// <server>_<tool> name the widget invoked.
	ToolName string `json:"toolName"`
	// Args is the tool call's argument payload, passed through opaque.
	Args json.RawMessage `json:"args"`
	// Requester is the webd-verified canonical viewer subject, e.g.
	// "user:<...>". Carried as a plain string — this package does not
	// import pkg/platform/identity for request payloads (see ExternalIdentity's
	// doc comment on the kinds→channelevents layering rule).
	Requester string `json:"requester"`
	// RequestID is a caller-generated idempotency/correlation key.
	RequestID string `json:"requestID"`
	// WidgetOrigin is the MCPServer CR name of the server whose ui:// resource
	// produced the widget MAKING this call ("mcpserver/<name>", the same string
	// tool.OriginTool.Origin returns) — the CALLER's identity, never the
	// callee's. Empty means the call did not come from a widget at all (an
	// agent-declared UI binding), and the runner then applies no origin pin.
	//
	// It is set SERVER-SIDE, by webd, from the session's own
	// status.activeWidgets entry for the artifact the shell says the call came
	// from. A widget cannot write it: the browser sends an artifact id, and an
	// artifact id it does not own resolves to that other widget's origin, which
	// pins it harder rather than laxer.
	WidgetOrigin string `json:"widgetOrigin,omitempty"`
}

// AppToolCallResponse is the runner's synchronous reply (via msg.Respond)
// to a KindAppToolCall request.
type AppToolCallResponse struct {
	// Status is one of the AppToolCallStatus* values below.
	Status string `json:"status"`
	// Result is the tool result content, populated only when
	// Status == AppToolCallStatusOK.
	Result json.RawMessage `json:"result,omitempty"`
	// IsError reports that the tool itself returned an error result
	// (Status is still AppToolCallStatusOK — the call reached the tool
	// and completed; the tool's own outcome was an error).
	IsError bool `json:"isError,omitempty"`
	// Message is the reason for a non-ok status, for an OPERATOR: it is the
	// diagnostic channel, and it is NOT browser-safe. On the denial and error
	// arms the runner relays the containment pipeline's own hook reason
	// verbatim, and those reasons are written for logs and for the model —
	// they name SpiceDB permissions, resource types and IDs, CRD field names,
	// and the raw subject.
	Message string `json:"message,omitempty"`
	// ViewerMessage is copy the replying site authored ITSELF for a human
	// looking at a browser, and therefore vouches for as free of internal
	// identifiers. A site that relays a reason from anywhere else leaves it
	// empty.
	//
	// Empty is the ZERO value, and that is the whole point: a renderer must
	// read this field and substitute its own fixed copy when it is empty, so
	// a reply site added later that forgets to author viewer copy degrades to
	// a generic message instead of leaking whatever it happened to relay.
	// pkg/web/uibindings/tool's errorForStatus is the renderer-side half of that
	// contract.
	ViewerMessage string `json:"viewerMessage,omitempty"`
}

// AppToolCallStatus values for AppToolCallResponse.Status.
const (
	// AppToolCallStatusOK is a completed dispatch; Result carries the
	// tool's output (IsError distinguishes a tool-level error from success).
	AppToolCallStatusOK = "ok"
	// AppToolCallStatusRequiresApproval means the tool is not readonly and
	// was not run synchronously; the caller must fall back to the normal
	// approval flow.
	AppToolCallStatusRequiresApproval = "requires_approval"
	// AppToolCallStatusDenied means an authz/interact check rejected the call.
	AppToolCallStatusDenied = "denied"
	// AppToolCallStatusMisconfigured means a gate refused the call because the
	// AGENT'S OWN DEFINITION could not be evaluated — a CEL trust rule that
	// does not compile, or that errors against the arguments it is given.
	//
	// Separate from denied because it is a different fact with a different
	// audience. A denial is about the caller and is answered by granting
	// access; this is about the agent and is answered by editing its spec. No
	// caller, however privileged, gets past it. Folding the two together told
	// viewers they lacked access to data that nobody could load, and left the
	// operator who could have fixed it with nothing to see.
	AppToolCallStatusMisconfigured = "misconfigured"
	// AppToolCallStatusNotFound means ToolName is not in the session's
	// AppTools registry — not opted in, or unknown.
	AppToolCallStatusNotFound = "not_found"
	// AppToolCallStatusError is a transport/dispatch error unrelated to the
	// tool's own execution (e.g. the runner couldn't be reached in time).
	AppToolCallStatusError = "error"
	// AppToolCallStatusRateLimited means the call's MCPServer origin has
	// exceeded its mcpUiAppTools.maxCallsPerMin budget (AppToolRateLimiter,
	// pkg/agent/runner). Enforced only on the autonomous app-tool path —
	// NOT via toolguard, so the origin's circuit breaker and its LLM-visible
	// tools' guards are untouched.
	AppToolCallStatusRateLimited = "rate_limited"
)
