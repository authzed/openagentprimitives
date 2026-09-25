// Package memory is the agent-memory framework: the Memory interface
// (Put/Query/Search/SendSignal), the Kind registry, the Local facade over a
// pluggable Backend, generic Query and scope semantics, signal dispatch, and
// the provider-neutral transcript types that flow over the HTTP API. A
// session's transcript is the turn Kind (kinds/turn) over that same Backend;
// the Backend implementations live in inmem/, sqlite/, postgres/ and shadow/.
package memory

import (
	"errors"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// NamespacedName identifies an AgentSession. Mirrors k8s types.NamespacedName
// but kept import-free so this package can be used from non-k8s code.
type NamespacedName struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Turn is one entry in a session's transcript. user/assistant roles feed back
// into the LLM messages array on resume; system_note is operator/runner
// metadata, filtered out at replay time.
type Turn struct {
	// Index is the turn's 0-based position in the transcript; (Index, Role) is
	// the identity turn.Appender guards against conflicting re-writes.
	Index int `json:"index"`
	// Role is "user" | "assistant" | "system_note".
	Role string `json:"role"`
	// Content holds the provider-neutral blocks, persisted verbatim.
	Content []ContentBlock `json:"content"`
	// CreatedAt is when the turn was recorded.
	CreatedAt time.Time `json:"createdAt"`
	// Usage is token accounting; nil on anything but an assistant turn.
	Usage *Usage `json:"usage,omitempty"`
	// Author is the canonical subject (e.g. "user:YWxpY2U") of the human who
	// authored this turn, including the index-0 initial prompt, which carries
	// the session starter's subject. Empty for agent/runner-authored turns
	// (tool results), system notes, and kubectl/bento-driven sessions with no
	// human starter. In multiplayer sessions it is the actual sender, not the
	// session initiator.
	Author identity.Subject `json:"author,omitempty"`
	// Via names the browser/TUI surface that injected this turn, as a view URN
	// (urn:ap:view:artifact:artifact-3f2a1b8c/artrev-9d4e0117). Empty for turns
	// arriving through the Channel's own listener. Server-minted, never
	// client-supplied. omitempty is load-bearing: the provenance digest omits
	// empty fields, so an unset Via must not perturb it.
	Via string `json:"via,omitempty"`
	// Refused marks an assistant turn whose provider response carried
	// stop_reason=refusal. The turn stays in the append-only transcript for
	// audit, but replay excludes it from the reconstructed message history so a
	// retry regenerates it cleanly. omitempty for the same digest reason as Via.
	Refused bool `json:"refused,omitempty"`
	// Model is the uniform display id of the model that actually served the
	// turn, "<provider>/<served-model>". Empty on user turns.
	Model string `json:"model,omitempty"`
}

// ContentBlock mirrors the LLM provider-neutral block shape so the runner
// can persist its native shape and reload it without translation.
type ContentBlock struct {
	// Type selects which of the fields below is populated: "text", "tool_use",
	// "tool_result" or "attachment".
	Type string `json:"type"`
	// Text is the block's prose; set only when Type == "text".
	Text string `json:"text,omitempty"`
	// ToolUse is the model's tool invocation; set only when Type == "tool_use".
	ToolUse *ToolUseBlock `json:"toolUse,omitempty"`
	// ToolResult is the tool's answer; set only when Type == "tool_result".
	ToolResult *ToolResultBlock `json:"toolResult,omitempty"`
	// Attachment carries a stored inbound file's handles and metadata, set only
	// when Type == "attachment" — one block per inbound attachment whose bytes
	// reached the store, whether or not extraction also succeeded (see
	// AttachmentBlock.TextRef).
	Attachment *AttachmentBlock `json:"attachment,omitempty"`
}

type ToolUseBlock struct {
	// ID correlates this call with the ToolResultBlock answering it.
	ID string `json:"id"`
	// Name is the tool as the agent loop knows it, not the upstream MCP name.
	Name string `json:"name"`
	// Input is raw JSON arguments, opaque to the store.
	Input []byte `json:"input"`
}

type ToolResultBlock struct {
	// ToolUseID is the ToolUseBlock.ID this result answers.
	ToolUseID string `json:"toolUseId"`
	// Content is the tool's output as the model will see it.
	Content string `json:"content"`
	// IsError marks a failed call; the content is then the error text.
	IsError bool `json:"isError,omitempty"`
	// RenderedLive records that the live surfaces already showed Content to the
	// user, so a resumed transcript must show it too — the live == reload
	// invariant. Set by the runner from tool.Result.RenderedLive, which only a
	// streaming/interactive toolkit's composed result carries; ordinary tool
	// output (a memory query's rows, a kubectl dump) leaves it false and stays
	// hidden. Read by turn.VisibleTimeline. Content is still the ENVELOPED form
	// the model saw — a renderer unwraps it (pkg/agent/toolenvelope).
	//
	// omitempty is load-bearing for the same reason as Turn.Via: the provenance
	// digest omits empty fields, so an unset flag must not perturb it, and
	// entries written before this existed stay verifiable byte-for-byte.
	RenderedLive bool `json:"renderedLive,omitempty"`
}

// AttachmentBlock records a file a user attached to an inbound message that the
// pipeline actually fetched and, when a backend claimed its MIME, extracted.
// Ref/TextRef are artifactstore handles, not bytes — the same by-reference shape
// respond_to_user's `attached` field and fetch_artifact use. Filename is
// untrusted, listener-sanitized input: never treat it as more than a label.
type AttachmentBlock struct {
	// Filename is the sanitized, user-supplied name.
	Filename string `json:"filename"`
	// MIME is the kind-reported content type — a hint, not a verified fact.
	MIME string `json:"mime"`
	// SizeBytes is the kind-reported size. May be 0 when unreported.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Ref is the artifactstore handle for the original bytes.
	Ref string `json:"ref"`
	// TextRef is the artifactstore handle for the extracted text; EMPTY when no
	// extractor exists for this MIME (images, today). Its presence is
	// load-bearing: with TextRef empty the runner keeps a native block attached
	// for the whole session, since nothing else represents the file; with it set
	// the block is kept only on the arrival turn, because the text carries it
	// after.
	TextRef string `json:"textRef,omitempty"`
	// ArchiveRef is the handle of the ARCHIVE this member came out of; empty
	// for a file the user attached directly.
	//
	// Load-bearing at hydration: an archive member is excluded from the
	// persistent native-block window unless pinned, because its archive's
	// index already named it and stated its readability. Without this field a
	// bundle of 187 log files would fill that window with four arbitrary
	// members and name them in an unreadable-attachments note the agent was
	// never told about.
	ArchiveRef string `json:"archiveRef,omitempty"`

	// Pages is the extractor-reported page/slide count; 0 when the format has no
	// real notion of pages.
	Pages int `json:"pages,omitempty"`
}

// Usage is per-turn token accounting as the provider reported it.
type Usage struct {
	// InputTokens billed for the prompt, cache reads excluded.
	InputTokens int64 `json:"inputTokens"`
	// OutputTokens billed for the completion.
	OutputTokens int64 `json:"outputTokens"`
	// CacheCreationTokens written into the prompt cache this turn.
	CacheCreationTokens int64 `json:"cacheCreationTokens,omitempty"`
	// CacheReadTokens served from the prompt cache instead of being billed as input.
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
}

// ErrIndexConflict signals that a turn Append's (index, role) collides with a
// previously-stored turn whose payload differs. Surfaced by turn.Appender.
var ErrIndexConflict = errors.New("memory: index conflict")
