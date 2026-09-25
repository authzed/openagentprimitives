// Package llm defines provider-neutral types and the Provider interface.
// Per-provider impls live in subpackages and translate to/from these types.
// The Cacheable bit is the portable cache-control hint; provider impls map it
// to a native mechanism or no-op.
package llm

import (
	"context"
	"encoding/json"
)

// Provider is the unified LLM interface used by both the agent loop
// (chat + tool_use) and the toolspec authoring pipeline (the free functions in
// pkg/tools/toolspec/llm/promptlib build on Send).
type Provider interface {
	// Name is a stable provider identifier ("anthropic", "fake") for logging.
	// Distinct from Request.Model, which names a model id within a provider.
	Name() string

	// SupportedFromEnv reports whether the env vars this provider needs are set,
	// so a caller can pick a provider at startup from whichever keys the user
	// has. Must be cheap — no network.
	SupportedFromEnv() bool

	// Pricing returns per-token pricing for the named model. ok=false when the
	// provider has no price for that model id (the caller must NOT fabricate a
	// number). Model-specific cost knowledge lives in the provider so consumers
	// never branch on provider name.
	Pricing(model string) (ModelPricing, bool)

	// Capabilities answers what the model CAN do; cheap, no network. An unknown
	// model id yields an empty CapabilitySet (Has always returns false) rather
	// than an error — capability data is advisory, not load-bearing the way
	// pricing is, so callers branch on Has without a second return value.
	Capabilities(model string) CapabilitySet

	// NativeInputMIMEs reports the MIME types this model accepts as native
	// message content blocks — an image/document block in a message, NOT a
	// Files-API upload into a code-execution container (that is CapNativeFileIn).
	// An unknown model id yields an empty set rather than an error; the caller
	// falls back to the extracted-text path, so this is advisory like
	// Capabilities.
	NativeInputMIMEs(model string) MIMESet

	// Send drives one chat-completion turn.
	Send(ctx context.Context, req Request) (Response, error)
}

// Request is one chat-completion turn, provider-neutral.
type Request struct {
	// Model is the provider-scoped model id. Required — the caller resolves it;
	// no default is substituted here or in the provider adapters.
	Model string
	// System is the system prompt in order, split into blocks so each can carry
	// its own Cacheable hint.
	System []SystemBlock
	// Messages is the conversation, oldest first.
	Messages []Message
	// Tools is the tool list offered to the model for this turn.
	Tools []ToolDef
	// MaxTokens caps the tokens the model may generate this turn.
	MaxTokens int

	// UserID is a provider-neutral, opaque request tag (no PII). The anthropic
	// adapter maps it to metadata.user_id; other providers map to their own
	// per-request attribution field. Empty = unset.
	UserID string

	// OnEvent, if non-nil, receives StreamEvents synchronously during a
	// streaming generation. Provider implementations call this on the
	// SDK's event-loop goroutine — callbacks should be cheap or buffer
	// to a worker. Nil = no live forwarding (final Response is still
	// returned identically).
	OnEvent func(StreamEvent)

	// ProviderRouting expresses OpenRouter dynamic-routing preferences;
	// ignored by non-OpenRouter providers.
	ProviderRouting *OpenRouterRouting
}

// SystemBlock is one segment of the system prompt.
type SystemBlock struct {
	Text string
	// Cacheable asks the provider to place a prompt-cache breakpoint after this
	// block; providers without a cache mechanism ignore it.
	Cacheable bool
}

// ToolDef is one tool as offered to the model.
type ToolDef struct {
	// Name is the LLM-facing tool name.
	Name string
	// Description is PROMPT TEXT the model reads to decide when to call the tool.
	Description string
	// InputSchema is the JSON Schema for the arguments; its nested "description"
	// strings are prompt text too.
	InputSchema json.RawMessage
	// Cacheable asks the provider to place a prompt-cache breakpoint after this
	// tool def.
	Cacheable bool

	// ServerType, when non-empty, marks this as an Anthropic *server* tool (e.g.
	// "web_search_20250305") that the LLM API runs itself rather than the runner
	// dispatching it. Description and InputSchema are then ignored — the API
	// knows the shape from the type identifier alone. Empty means a custom
	// client tool.
	ServerType string
}

// Message is one conversation turn.
type Message struct {
	Role    string // "user" | "assistant"
	Content []ContentBlock
}

// ContentBlock is one block within a Message; Type selects which fields are set.
type ContentBlock struct {
	Type       string // "text" | "tool_use" | "tool_result" | "container_upload" | "document" | "image"
	Text       string
	ToolUse    *ToolUseBlock
	ToolResult *ToolResultBlock
	// Cacheable asks the provider to place a prompt-cache breakpoint after this
	// block.
	Cacheable bool

	// FileID is set on a container_upload block — a provider file id (from
	// the Files API) preloaded into the code-execution container. Bridged
	// in via the files modality's Tier-2 path.
	FileID string

	// MIME and Data carry a native "document" or "image" block: the file's
	// content type and its raw bytes, base64-encoded by the provider adapter.
	// Only ever set by the runner's attachment hydration pass, and only when
	// the resolved model declared this MIME in NativeInputMIMEs.
	MIME string
	Data []byte

	// Attachment is the reference form of an inbound attachment: handles and
	// metadata, no bytes. It flows from memory through message assembly and is
	// resolved by the hydration pass into either a native block above or
	// nothing. It MUST NOT survive into a provider adapter — every adapter
	// hard-errors on an unknown block type.
	Attachment *AttachmentRef
}

// AttachmentRef is the reference form of a stored inbound attachment, carried
// between memory and the runner's hydration pass. It holds no bytes: the pass
// fetches them only for attachments it will actually render natively, so a
// session full of unrenderable files costs no artifact reads.
//
// The turn an attachment arrived on is deliberately NOT a field: the hydration
// pass derives head-turn eligibility and newest-N ordering from the ref's
// position in the message slice it walks, which cannot drift from the
// conversation the way a stored index can.
type AttachmentRef struct {
	Ref      string // artifactstore handle for the original bytes
	TextRef  string // handle for extracted text; empty when no extractor exists
	MIME     string
	Filename string
	Size     int64
	// ArchiveRef is the handle of the archive this member came from; empty for
	// a directly-attached file. Hydration keys the native-window exclusion on
	// it — see AttachmentBlock.ArchiveRef.
	ArchiveRef string
	// Pages is the page count for paginated documents; zero when unknown.
	Pages int
}

// ToolUseBlock is the model's request to call one tool.
type ToolUseBlock struct {
	// ID correlates this call with its ToolResultBlock.ToolUseID.
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResultBlock is one tool's outcome fed back to the model.
type ToolResultBlock struct {
	ToolUseID string
	Content   string
	// IsError marks a tool-level failure; the model still reads Content.
	IsError bool
}

// Response is one completed chat-completion turn.
type Response struct {
	Content    []ContentBlock
	StopReason string // "end_turn" | "tool_use" | "max_tokens" | "stop_sequence"
	Usage      Usage

	// Model is the actually-served model; empty if the provider does not
	// report one.
	Model string
}

// Usage is the per-turn token accounting reported by the provider. The four
// buckets are disjoint: an input token counted as cache creation or cache read
// is not also counted in InputTokens.
type Usage struct {
	// InputTokens is prompt tokens billed at full input rate.
	InputTokens int64
	// OutputTokens is tokens the model generated this turn.
	OutputTokens int64
	// CacheCreationTokens is prompt tokens written to the provider's prompt cache.
	CacheCreationTokens int64
	// CacheReadTokens is prompt tokens served from the provider's prompt cache.
	CacheReadTokens int64

	// CostUSD is the provider-reported dollar cost; nil = not reported.
	CostUSD *float64
}

// ModelPricing is per-model token pricing, in USD per 1,000,000 tokens, split by
// the four usage buckets (Anthropic prices cache-write at 1.25x input and
// cache-read at 0.1x input; other providers differ — each owns its own table).
type ModelPricing struct {
	InputPerMTok         float64
	OutputPerMTok        float64
	CacheCreationPerMTok float64
	CacheReadPerMTok     float64
	Currency             string // ISO 4217, e.g. "USD"
}

// StreamEventType identifies the kind of streaming event flowing through
// Request.OnEvent. The value space is provider-neutral; per-provider
// adapters translate their native events into this enum.
type StreamEventType string

const (
	StreamEventTextDelta        StreamEventType = "text_delta"
	StreamEventToolUseStart     StreamEventType = "tool_use_start"
	StreamEventToolUseDeltaArgs StreamEventType = "tool_use_delta_args"
	StreamEventToolUseStop      StreamEventType = "tool_use_stop"
	StreamEventThinkingDelta    StreamEventType = "thinking_delta"
	StreamEventUsage            StreamEventType = "usage"
	StreamEventStop             StreamEventType = "stop"
)

// StreamEvent is the provider-neutral shape carried by Request.OnEvent. Type
// selects which payload fields are set; the rest stay zero.
//
// JSONFragment on tool_use_delta_args events is partial JSON — NOT valid until
// the matching tool_use_stop. Consumers wanting the full args should read them
// from the final llm.Response after Send returns; per-event JSON-fragment
// handling is rare and provider-leaky.
type StreamEvent struct {
	Type StreamEventType
	// BlockIndex is the event's position in the response's content-block list,
	// so deltas can be routed to the block they belong to.
	BlockIndex int

	// text_delta:
	Text string

	// thinking_delta:
	Thinking string

	// tool_use_*:
	ToolUseID    string
	ToolName     string
	JSONFragment string

	// usage (cumulative; arrives periodically):
	Usage *Usage

	// stop:
	StopReason string
}

// HasToolUses reports whether r.Content contains any tool_use blocks.
func (r Response) HasToolUses() bool {
	for _, b := range r.Content {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// ToolUses returns the tool_use blocks, in encounter order.
func (r Response) ToolUses() []ToolUseBlock {
	var out []ToolUseBlock
	for _, b := range r.Content {
		if b.Type == "tool_use" && b.ToolUse != nil {
			out = append(out, *b.ToolUse)
		}
	}
	return out
}
