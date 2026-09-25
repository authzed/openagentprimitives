package runner

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
)

// replay reconstructs the messages array from prior turns. system_note and any
// non-user/assistant roles are skipped. nextIndex is the next free Index;
// hadInitialPrompt is true iff a user turn at Index 0 was present (the
// cold-start initial prompt has been appended).
//
// Two passes make the reconstruction sendable rather than merely faithful:
// repairOrphanToolUses answers any tool_use an ungraceful restart left dangling
// (a provider rejects the request outright otherwise), and appendRefusalNudge
// tells a retry why the prior attempt didn't land. Refused assistant turns stay
// in the append-only transcript for audit but are excluded from msgs (see
// memory.Turn.Refused) while still advancing nextIndex. Neither pass's blocks
// are ever appended to the transcript. sessionRef names the session for the
// repair's log line.
func replay(prior []memory.Turn, sessionRef string) (msgs []llm.Message, nextIndex int, hadInitialPrompt bool) {
	trailingRefused := false
	for _, t := range prior {
		if t.Index >= nextIndex {
			nextIndex = t.Index + 1
		}
		if t.Index == 0 && t.Role == "user" {
			hadInitialPrompt = true
		}
		if t.Role == "assistant" && t.Refused {
			// Kept in the append-only transcript for audit; excluded from replay
			// so the retry re-generates this turn without the content that tripped
			// the provider. Remember it was the most recent event.
			trailingRefused = true
			continue
		}
		if t.Role == "user" || t.Role == "assistant" {
			trailingRefused = false
			blocks := contentBlocksFromMemory(t.Content)
			if t.Role == "user" && t.Via != "" {
				if desc := viewurn.Describe(t.Via); desc != "" {
					blocks = append([]llm.ContentBlock{{Type: "text", Text: "(via " + desc + ")"}}, blocks...)
				}
			}
			msgs = append(msgs, llm.Message{Role: t.Role, Content: blocks})
		}
	}
	// Repair before the refusal nudge: the nudge appends to the last user
	// message when there is one, so running the repair first lets a
	// synthesized answer and the nudge share a single trailing user turn
	// instead of the nudge landing on a still-unanswered tool_use.
	repairOrphanToolUses(&msgs, ComputeDeliveredToolUseIDs(prior), sessionRef)
	if trailingRefused {
		appendRefusalNudge(&msgs)
	}
	return msgs, nextIndex, hadInitialPrompt
}

// Text carried by the tool_result blocks repairOrphanToolUses synthesizes.
// Both are addressed to the model, and the distinction between them is
// load-bearing: the unknown-outcome wording must not let the model assume the
// call succeeded, and the delivered wording must stop it re-sending a reply
// the user has already read.
const (
	orphanToolResultUnknownText = "[runner-warning] No result was recorded for this tool call: the agent runtime restarted while the call was in flight. The tool may or may not have run — do NOT assume it succeeded. If the step is still needed, call the tool again; if repeating it would not be safe, tell the user what was in progress and ask how to proceed."

	orphanToolResultDeliveredText = "delivered — this reply reached the user before the agent runtime restarted, and only the tool's result was lost. Do NOT send it again."
)

// repairOrphanToolUses makes the reconstructed message list well-formed by
// answering every `tool_use` block that replay found no `tool_result` for.
//
// The loop appends the assistant turn carrying `tool_use` blocks to durable
// memory BEFORE dispatching them, and the paired `tool_result` user turn only
// once every dispatch has returned; an ungraceful death in between (OOM,
// eviction, node loss, a restart during a minutes-long approval Await) persists
// an assistant turn nothing ever answered. Untreated, that orphan is fatal and
// self-sustaining: providers reject a `tool_use` with no `tool_result`
// immediately after it, failProviderError's Retry replays the identical orphan,
// drainInbox appends new messages AFTER it, and channelsd keeps routing later
// messages to the dead ToolCall — leaving the user no exit but deleting the
// session.
//
// Placement matters twice over: the synthesized blocks LEAD the user message
// that answers them (a provider requires `tool_result` at the start of the
// turn), and they go INTO an immediately-following user message when one exists
// rather than a message of their own, so the repair never splits one user turn
// into two.
//
// The blocks are transient, never appended to memory: the transcript is
// append-only and signed per turn, so the runner must not fabricate a durable
// turn to paper over a crash. That also makes the repair idempotent — a later
// replay of the same transcript re-derives the same blocks.
func repairOrphanToolUses(msgs *[]llm.Message, delivered map[string]bool, sessionRef string) {
	var repaired []string
	out := *msgs
	for i := 0; i < len(out); i++ {
		if out[i].Role != "assistant" {
			continue
		}
		// Only the immediately-following user message counts as an answer —
		// that is the rule the providers enforce, and it is where the loop
		// writes the result turn.
		answered := map[string]bool{}
		nextIsUser := i+1 < len(out) && out[i+1].Role == "user"
		if nextIsUser {
			for _, b := range out[i+1].Content {
				if b.Type == "tool_result" && b.ToolResult != nil {
					answered[b.ToolResult.ToolUseID] = true
				}
			}
		}
		var synth []llm.ContentBlock
		for _, b := range out[i].Content {
			if b.Type != "tool_use" || b.ToolUse == nil || answered[b.ToolUse.ID] {
				continue
			}
			// A delivered tool_use is one respond_to_user published
			// successfully: the delivered system_note is written after the
			// publish and before the result turn, so the user HAS read it.
			// Reporting that as a failed step would make the model re-send and
			// the user see the message twice.
			text, isErr := orphanToolResultUnknownText, true
			if delivered[b.ToolUse.ID] {
				text, isErr = orphanToolResultDeliveredText, false
			}
			synth = append(synth, llm.ContentBlock{
				Type:       "tool_result",
				ToolResult: &llm.ToolResultBlock{ToolUseID: b.ToolUse.ID, Content: text, IsError: isErr},
			})
			repaired = append(repaired, b.ToolUse.ID)
		}
		if len(synth) == 0 {
			continue
		}
		if nextIsUser {
			out[i+1].Content = append(synth, out[i+1].Content...)
			continue
		}
		// Nothing follows this assistant turn, or an assistant turn does
		// (a refused turn between the two is dropped above): the answer needs
		// a user message of its own, inserted right after the orphan.
		out = append(out, llm.Message{})
		copy(out[i+2:], out[i+1:])
		out[i+1] = llm.Message{Role: "user", Content: synth}
		i++ // already answered; don't re-scan the message just inserted
	}
	*msgs = out // an inserted message may have reallocated the slice
	if len(repaired) == 0 {
		return
	}
	// A repair is a recovery an operator must be able to find after the fact:
	// it is the only trace that a crash cost this session a tool result.
	slog.Default().Info("replay: synthesized tool_result for tool_use blocks left unanswered by an ungraceful restart",
		"session", sessionRef, "count", len(repaired), "toolUseIDs", strings.Join(repaired, ","))
}

// appendRefusalNudge appends a one-line, transient advisory to the last user
// message so the model — on a retry after a refusal — knows the prior attempt
// was content-blocked and should be reframed/lightened. It is NOT persisted:
// it exists only in the reconstructed message array for this Send. If the last
// message is not a user turn (rare — an assistant turn normally precedes a
// refused one), a fresh user message carries it so the request still ends on a
// user turn.
func appendRefusalNudge(msgs *[]llm.Message) {
	const nudge = "[system] Your previous attempt at this turn was blocked by the content policy and was not delivered. Try again; if the response involved a very large inline payload such as an embedded base64 image, produce a smaller version (reference or omit the image)."
	appendTransientAdvisory(msgs, nudge)
}

// appendTransientAdvisory attaches a one-line `[system]` advisory to the last
// user message, or as a fresh user message when the array does not already end
// on a user turn (the request must end on one).
//
// "Transient" is the contract: the advisory exists only in the array
// reconstructed for THIS Send. Advisories describe the current turn's state — a
// content-policy retry, a sidecar that is down — and replaying one into a later
// turn would re-assert a condition that may since have healed.
func appendTransientAdvisory(msgs *[]llm.Message, text string) {
	if n := len(*msgs); n > 0 && (*msgs)[n-1].Role == "user" {
		(*msgs)[n-1].Content = append((*msgs)[n-1].Content, llm.ContentBlock{Type: "text", Text: text})
		return
	}
	*msgs = append(*msgs, llm.Message{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: text}}})
}

func contentBlocksToMemory(in []llm.ContentBlock) []memory.ContentBlock {
	out := make([]memory.ContentBlock, len(in))
	for i, b := range in {
		mb := memory.ContentBlock{Type: b.Type, Text: b.Text}
		if b.ToolUse != nil {
			mb.ToolUse = &memory.ToolUseBlock{
				ID: b.ToolUse.ID, Name: b.ToolUse.Name, Input: cloneJSON(b.ToolUse.Input),
			}
		}
		if b.ToolResult != nil {
			mb.ToolResult = &memory.ToolResultBlock{
				ToolUseID: b.ToolResult.ToolUseID, Content: b.ToolResult.Content, IsError: b.ToolResult.IsError,
			}
		}
		out[i] = mb
	}
	return out
}

// llmRenderableBlockTypes are the memory.ContentBlock.Type values every provider
// adapter's BuildParams/translate actually understands
// (pkg/agent/llm/anthropic/anthropic.go, pkg/agent/llm/openaicompat/translate.go
// — both hard-error on anything else rather than skipping it). An allowlist
// rather than a denylist so a FUTURE memory.ContentBlock variant fails safe
// (dropped, not handed to a provider that has never heard of it) until whoever
// adds it also teaches every adapter about it.
var llmRenderableBlockTypes = map[string]bool{
	"text":             true,
	"tool_use":         true,
	"tool_result":      true,
	"container_upload": true,
	// "attachment" flows through in REFERENCE form only (handles, no bytes)
	// and is resolved by hydrateAttachments before the provider call. It is
	// listed here because message assembly must carry it; it is never valid
	// at an adapter boundary, and the hydration pass guarantees it never
	// reaches one.
	"attachment": true,
}

// emptyContentPlaceholderText stands in for a message that would otherwise end
// up with zero content blocks: every block in the turn was unrenderable
// (contentBlocksFromMemory), or the turn's only block was an attachment ref
// hydrateAttachments had to clear. An empty Content slice is its own provider
// error, distinct from "unknown content block type" — a message needs at least
// one block — so both sites fall back here. Keep both fallbacks: either one,
// alone, can be the one that empties the message.
const emptyContentPlaceholderText = "[content omitted: unsupported block type]"

// contentBlocksFromMemory converts a stored turn's content into the
// provider-neutral llm.ContentBlock shape, dropping any block type no provider
// adapter renders. "attachment" IS renderable here, but only in reference form
// (memory.AttachmentBlock's durable handles/metadata, no bytes); the runner's
// hydrateAttachments pass resolves it immediately before the provider call, so
// no attachment block ever reaches an adapter. Passing one straight through
// hard-errors the request with "unknown content block type" — the turn is
// unusable, not merely under-described.
//
// If every block turns out unrenderable, emptyContentPlaceholderText stands in:
// an empty Content slice is itself a provider error (a message needs at least
// one block).
func contentBlocksFromMemory(in []memory.ContentBlock) []llm.ContentBlock {
	out := make([]llm.ContentBlock, 0, len(in))
	for _, b := range in {
		if !llmRenderableBlockTypes[b.Type] {
			continue
		}
		mb := llm.ContentBlock{Type: b.Type, Text: b.Text}
		if b.ToolUse != nil {
			mb.ToolUse = &llm.ToolUseBlock{
				ID: b.ToolUse.ID, Name: b.ToolUse.Name, Input: json.RawMessage(b.ToolUse.Input),
			}
		}
		if b.ToolResult != nil {
			mb.ToolResult = &llm.ToolResultBlock{
				ToolUseID: b.ToolResult.ToolUseID, Content: b.ToolResult.Content, IsError: b.ToolResult.IsError,
			}
		}
		if b.Attachment != nil {
			mb.Attachment = &llm.AttachmentRef{
				Ref:        b.Attachment.Ref,
				TextRef:    b.Attachment.TextRef,
				MIME:       b.Attachment.MIME,
				Filename:   b.Attachment.Filename,
				Size:       b.Attachment.SizeBytes,
				Pages:      b.Attachment.Pages,
				ArchiveRef: b.Attachment.ArchiveRef,
			}
		}
		out = append(out, mb)
	}
	if len(out) == 0 && len(in) > 0 {
		out = append(out, llm.ContentBlock{Type: "text", Text: emptyContentPlaceholderText})
	}
	return out
}

func cloneJSON(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// ComputeDeliveredToolUseIDs scans memory for system_note turns whose JSON
// payload includes a "delivered" array. The runner appends one of these
// after each successful respond_to_user publish; on resume, the loop
// uses this set to skip re-dispatch of already-delivered tool_use IDs.
func ComputeDeliveredToolUseIDs(turns []memory.Turn) map[string]bool {
	out := map[string]bool{}
	for _, t := range turns {
		if t.Role != "system_note" {
			continue
		}
		for _, b := range t.Content {
			if b.Type != "text" || b.Text == "" {
				continue
			}
			var note struct {
				Delivered []string `json:"delivered"`
			}
			if err := json.Unmarshal([]byte(b.Text), &note); err != nil {
				continue
			}
			for _, id := range note.Delivered {
				out[id] = true
			}
		}
	}
	return out
}

// ReplayStateNotes scans turns for system_note entries whose JSON payload
// matches the framework wrapper {"kind":..., "v":..., "data":...} and dispatches
// each to the matching state.Store via state.DispatchSystemNote. Notes that
// don't match the wrapper (e.g. the "delivered" set) are ignored — those flow
// through ComputeDeliveredToolUseIDs unchanged.
//
// A note that DOES match a registered Kind but whose ReplayNote fails (corrupt
// or version-mismatched data) is logged at INFO: that state (e.g. the user's
// tracked plan) will be missing from the resumed session, and "resumed with no
// plan state" must be explicable from logs rather than vanishing silently.
func ReplayStateNotes(turns []memory.Turn, registry *state.Registry) {
	if registry == nil {
		return
	}
	for _, t := range turns {
		if t.Role != "system_note" {
			continue
		}
		for _, b := range t.Content {
			if b.Type != "text" || b.Text == "" {
				continue
			}
			if _, err := state.DispatchSystemNote(registry, []byte(b.Text)); err != nil {
				// Matched a registered Kind but failed to replay. Don't
				// abort the rest of the replay — other notes/kinds may
				// still rebuild correctly — but make the loss visible.
				slog.Default().Info("ReplayStateNotes: state note failed to replay; resuming without that Kind's state",
					"op", "replay_state_note",
					"err", err.Error())
			}
		}
	}
}
