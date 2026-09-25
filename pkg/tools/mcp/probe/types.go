package probe

import "encoding/json"

// ServerInfo is the implementation identity a server self-reports in the
// initialize handshake. Audit metadata only: it is asserted by the server.
type ServerInfo struct {
	// Name is the implementation name the server calls itself.
	Name string `json:"name"`
	// Version is the implementation version the server reports.
	Version string `json:"version"`
}

// Tool is one entry in a tools/list response. InputSchema is preserved as
// raw JSON so callers can introspect its shape without committing to a
// JSON Schema parser.
type Tool struct {
	// Name is the tool's identifier, as tools/call must address it.
	Name string `json:"name"`
	// Description is the server's own prose. It reaches the model verbatim
	// unless a spec overrides it, so it is untrusted prompt text.
	Description string `json:"description,omitempty"`
	// InputSchema is the tool's declared argument schema. Empty when the
	// server advertises none.
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// OutputSchema is the tool's declared result schema (MCP
	// 2025-06-18 revision). Empty when the server does not advertise
	// one. Like InputSchema, preserved as raw JSON.
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	// Annotations are the server's self-asserted hints about the tool.
	Annotations Annotations `json:"annotations,omitempty"`
}

// Annotations mirrors the MCP protocol's tool annotation hints. Empty
// fields mean the server did not assert that hint.
type Annotations struct {
	// DestructiveHint: the server says the call may make changes that cannot
	// simply be undone.
	DestructiveHint bool `json:"destructiveHint,omitempty"`
	// ReadOnlyHint: the server says the call changes nothing.
	ReadOnlyHint bool `json:"readOnlyHint,omitempty"`
	// IdempotentHint: the server says repeating the call has no extra effect.
	IdempotentHint bool `json:"idempotentHint,omitempty"`
	// OpenWorldHint: the server says the call reaches systems beyond itself.
	OpenWorldHint bool `json:"openWorldHint,omitempty"`
	// Title is a human-readable label some servers attach; passed
	// through for completeness but not used by the validator.
	Title string `json:"title,omitempty"`

	// SEP-1913 trust + action-security annotations. See
	// https://github.com/modelcontextprotocol/modelcontextprotocol/pull/1913
	//
	// The structured InputMetadata / ReturnMetadata are kept as raw
	// JSON because the SEP's nested DataClass shape is still in
	// flux — RawMessage round-trips any future shape losslessly.
	// MaliciousActivityHint: the server declares it MAY flag malicious
	// activity — a capability, not an accusation.
	MaliciousActivityHint bool `json:"maliciousActivityHint,omitempty"`
	// Attribution is who the server credits the tool's behavior to.
	Attribution []string `json:"attribution,omitempty"`
	// InputMetadata describes what the call does (outcomes, destination).
	InputMetadata json.RawMessage `json:"inputMetadata,omitempty"`
	// ReturnMetadata describes where the result comes from (source).
	ReturnMetadata json.RawMessage `json:"returnMetadata,omitempty"`

	// Visibility is MCP Apps' _meta.ui.visibility: which surfaces the tool
	// is exposed to, "model" and/or "app". Recovered from the tool's _meta
	// the same way SEP-1913's fields are, one level deeper (nested under
	// "ui"). Empty/absent means the server did not assert it — this field
	// mirrors the WIRE, so it stays nil rather than being defaulted to a
	// non-nil slice; do not default it here.
	//
	// MCP Apps' own protocol treats an unset value as ambiguous (readable as
	// "both"), but no reader in this repo does: this value is copied
	// verbatim into mcpspec.Tool.Visibility, whose one consumer,
	// pkg/agent/tool/mcp/synthesize.go's exclusive app/model split, treats
	// empty/absent the same as ["model"] — model-visible only, never
	// browser-callable. A caller reading THIS field directly (bypassing that
	// split) must not assume "both"; it must go through synthesize.go's
	// routing to get this repo's actual interpretation.
	Visibility []string `json:"visibility,omitempty"`
}
