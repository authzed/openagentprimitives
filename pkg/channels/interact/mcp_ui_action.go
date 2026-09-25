package interact

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
)

func init() { Register(mcpUiActionKind{}) }

const (
	// maxActionFieldChars bounds every widget/MCP-server-authored string field
	// before it reaches the LLM. Defense-in-depth truncation, not a DoS
	// control — the request body is already 64 KB-capped upstream.
	maxActionFieldChars = 2000
	// maxActionParamsBytes caps the marshaled size of Params — arbitrary,
	// attacker-controlled JSON — before it is embedded (as opaque text, never
	// parsed or executed) in the envelope.
	maxActionParamsBytes = 2000
)

// validActionTypes is the fixed allowlist of @mcp-ui/client UIActionResult
// variants this kind accepts. Anything else is rejected in Submit before any
// NATS request — an unrecognized type is not a routable widget action, never
// a partial best-effort guess.
var validActionTypes = map[string]bool{
	"tool":   true,
	"prompt": true,
	"link":   true,
	"intent": true,
	"notify": true,
}

// mcpUiActionKind routes one MCP-UI widget action (@mcp-ui/client's
// UIActionResult) into the session as a single untrusted-delimited user turn
// (see action_envelope.go). It defers the raw echo: the source channel is
// soft-muted during widget interaction, and mirroring the raw
// <untrusted-widget-action nonce=N> delimiter markup verbatim would be noise.
type mcpUiActionKind struct{}

// mcpUiActionPayload is the wire shape of one @mcp-ui/client UIActionResult,
// decoded server-side from the raw /interact submission. Every field is
// widget/MCP-server-authored and therefore untrusted.
type mcpUiActionPayload struct {
	// Type is which UIActionResult variant this is; anything outside
	// validActionTypes is refused before any NATS request.
	Type string `json:"type"`
	// ToolName is the tool a "tool" action asks for, by the widget's name for
	// it. Advisory: nothing here invokes it.
	ToolName string `json:"toolName"`
	// Params is the "tool" action's arguments — arbitrary attacker-controlled
	// JSON, NEVER parsed or executed here, only clamped and rendered as opaque
	// text inside the untrusted envelope.
	Params json.RawMessage `json:"params"`
	// Prompt is the text a "prompt" action wants put to the agent.
	Prompt string `json:"prompt"`
	// URL is the destination a "link" action points at; rendered as text, never
	// fetched.
	URL string `json:"url"`
	// Intent is the named intent an "intent" action raises.
	Intent string `json:"intent"`
	// Message is the body of a "notify" action.
	Message string `json:"message"`
}

func (mcpUiActionKind) Name() string { return "mcp_ui_action" }

// Permission is "interact" (agentsession#interact) — REQUIRED: the /interact
// endpoint hard-rejects any Kind whose Permission() is not exactly this
// value.
func (mcpUiActionKind) Permission() string { return "interact" }

// ViaSub places widget actions at the session-view /widget sub-URN, so the
// turn's Via (urn:ap:view:session:<ns>/<name>/widget) verifiably marks it as
// a widget action — structural, signed provenance, not sniffed from text.
func (mcpUiActionKind) ViaSub() string { return viewurn.SubWidget }

func (mcpUiActionKind) Submit(ctx context.Context, deps Deps, ns, name, subject, via string, raw json.RawMessage) (Result, error) {
	var pl mcpUiActionPayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return Result{}, fmt.Errorf("interact: mcp_ui_action: decode payload: %w", err)
	}
	if !validActionTypes[pl.Type] {
		// Refused before any NATS request: an unrecognized type is not a
		// routable @mcp-ui/client action.
		return Result{}, fmt.Errorf("interact: mcp_ui_action: unknown action type %q", pl.Type)
	}
	// Truncate every untrusted, widget-authored field before it reaches the LLM.
	clampAction(&pl)

	text, err := buildActionEnvelope(pl)
	if err != nil {
		return Result{}, err
	}

	// The canonical subject is "user:<base64url(email)>"; decoding recovers the
	// original email, from which channelsd's HandleViewMessage re-derives the
	// SAME canonical id, so this round-trips.
	email := identity.DecodeForDisplay(subject)
	ext := channelkinds.ExternalIdentity{Kind: "idp", Email: identity.Email(email), ExternalID: identity.RawExternalID(email)}

	// deferEcho=true: the source channel is soft-muted during widget
	// interaction, and echoing the raw delimiter markup there is noise.
	dec, err := channelkinds.RequestViewMessage(channelkinds.Deps{NATSRequest: deps.NATSRequest}, ns, name, text, via, ext, true, "")
	if err != nil {
		return Result{Outcome: dec.Outcome.String(), Notice: dec.Notice.ToWire()}, fmt.Errorf("interact: mcp_ui_action: %w", err)
	}
	return Result{Outcome: dec.Outcome.String(), Notice: dec.Notice.ToWire()}, nil
}

// clampAction truncates every untrusted, widget-authored string field before it
// reaches the LLM, and caps Params to a bounded byte length. Params is never
// parsed or executed — it is opaque text in the envelope — so truncating its
// raw bytes, which may leave the JSON malformed, is fine.
func clampAction(pl *mcpUiActionPayload) {
	clamp := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	pl.ToolName = clamp(pl.ToolName, maxActionFieldChars)
	pl.Prompt = clamp(pl.Prompt, maxActionFieldChars)
	pl.URL = clamp(pl.URL, maxActionFieldChars)
	pl.Intent = clamp(pl.Intent, maxActionFieldChars)
	pl.Message = clamp(pl.Message, maxActionFieldChars)
	if len(pl.Params) > maxActionParamsBytes {
		truncated := make(json.RawMessage, maxActionParamsBytes)
		copy(truncated, pl.Params[:maxActionParamsBytes])
		pl.Params = truncated
	}
}
