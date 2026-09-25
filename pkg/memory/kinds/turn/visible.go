package turn

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Chat-timeline roles a memory turn maps to, and the internal turn roles this
// mapping reasons about.
const (
	// VisibleRoleUser is a human message; VisibleRoleAgent is an agent reply.
	VisibleRoleUser  = "user"
	VisibleRoleAgent = "agent"

	roleUser      = "user"
	roleAssistant = "assistant"
	// roleInbox is a raw inbound human message written by channelsd/chat before
	// the runner drains it; roleInboxDone is the marker the runner writes at a
	// drained inbox turn's index once it has promoted it to a roleUser turn.
	roleInbox     = "inbox"
	roleInboxDone = "inbox_done"

	// respondToUserToolName is the meta tool an agent uses to deliver its reply;
	// the reply text lives in the tool_use block's input JSON, not a text block.
	// Kept as a literal (not imported from pkg/agent/tool/meta) to avoid a
	// dependency from the memory layer up into the agent tool layer.
	respondToUserToolName = "respond_to_user"
)

// VisibleMessage is one user-facing message extracted from a session's memory
// log — the conversation as a human should see it, with the runner's internal
// bookkeeping (inbox duplication, tool-only turns, markers) resolved away.
type VisibleMessage struct {
	Role      string    // VisibleRoleUser | VisibleRoleAgent
	Text      string    // user-facing text (respond_to_user text extracted)
	CreatedAt time.Time // when the underlying turn was written
	Via       string    // optional surface origin (e.g., "urn:ap:view:artifact:...")
}

// VisibleMessages extracts the ordered user-facing MESSAGES (no plan cards):
// VisibleTimeline filtered to messages, so the extraction rules live in one
// place. See VisibleTimeline.
func VisibleMessages(turns []memory.Turn) []VisibleMessage {
	items := VisibleTimeline(turns)
	out := make([]VisibleMessage, 0, len(items))
	for _, it := range items {
		if it.Kind == "message" {
			out = append(out, *it.Message)
		}
	}
	return out
}

// TimelineItem is one ordered entry of the resumed conversation: a user/agent
// message or a plan-card snapshot. It is the superset VisibleMessages filters.
type TimelineItem struct {
	Kind    string // "message" | "plan"
	Message *VisibleMessage
	Plan    *PlanTimelineItem
}

// PlanTimelineItem is a plan card reconstructed from the "plans" system_notes:
// one per distinct plan name, positioned at first appearance, carrying the
// LATEST snapshot (opaque JSON — the memory layer does not import the plan
// types; a higher layer maps it to the channel card). Deleted is true when the
// name's latest op was "delete".
type PlanTimelineItem struct {
	PlanName     string
	SnapshotJSON []byte
	Deleted      bool
	CreatedAt    time.Time
}

// planNote is the generic (plans-free) view of a "plans" system_note's payload.
type planNote struct {
	// Kind discriminates system_note payloads; only "plans" is handled here.
	Kind string `json:"kind"`
	Data struct {
		// Op is the plan operation; "delete" tombstones the card.
		Op string `json:"op"`
		// Plan is the snapshot, kept opaque: no plan types are imported here.
		Plan json.RawMessage `json:"plan"`
		// Name is the plan name on a delete op; other ops carry it in Plan.
		Name string `json:"name"`
	} `json:"data"`
}

// VisibleTimeline extracts the ordered user-facing timeline — messages AND plan
// cards — from a session's raw memory turns; VisibleMessages is this, filtered.
// "plans" system_notes are grouped by name: one card at its first-appearance
// index, updated in place by later notes for the same name.
//
// The subtlety it encapsulates is the inbox lifecycle. A mid-session human
// message is first written as a roleInbox turn (channelsd/chat); the runner's
// drainInbox promotes it to a roleUser turn at a new index and writes a
// roleInboxDone marker at the inbox turn's index, but PRESERVES the raw
// roleInbox turn for audit (memory is append-only). A consumed inbox turn (one
// with a matching roleInboxDone) therefore duplicates its promoted roleUser
// turn and is dropped — rendering both showed every drained message twice —
// while an unconsumed one (not yet drained, the runner is parked) is kept, so
// a just-sent message still renders before it is promoted.
//
// Other rules: roleUser/roleInbox collapse to VisibleRoleUser and roleAssistant
// to VisibleRoleAgent; roleInboxDone and any other role contribute nothing; an
// agent reply's text is pulled from its respond_to_user tool_use block (see
// visibleText); a tool_result the live surfaces already showed the user renders
// as an agent message (see renderedLiveText); turns with no user-facing text (a
// tool-only assistant turn) are dropped. Input need not be sorted — turns are
// ordered by (index, role) here.
func VisibleTimeline(turns []memory.Turn) []TimelineItem {
	sorted := make([]memory.Turn, len(turns))
	copy(sorted, turns)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Index != sorted[j].Index {
			return sorted[i].Index < sorted[j].Index
		}
		return sorted[i].Role < sorted[j].Role
	})

	consumed := map[int]bool{}
	for _, t := range sorted {
		if t.Role == roleInboxDone {
			consumed[t.Index] = true
		}
	}

	out := make([]TimelineItem, 0, len(sorted))
	planPos := map[string]int{} // plan name -> index into out (first appearance)
	for _, t := range sorted {
		if t.Role == "system_note" {
			if pn, ok := parsePlanNote(t.Content); ok {
				applyPlanNote(&out, planPos, pn, t.CreatedAt)
			}
			continue
		}
		role, ok := visibleRole(t.Role)
		if !ok {
			continue
		}
		if t.Role == roleInbox && consumed[t.Index] {
			continue // its promoted roleUser turn is the canonical copy
		}
		if live := renderedLiveText(t.Content); live != "" {
			// A tool-result relay turn carrying output the live surfaces
			// already showed. It renders as an AGENT message, not a user one:
			// the runner wrote this turn under the "user" role because that is
			// where the providers require tool results to sit, but the human
			// authored none of it and live never attributed it to them.
			out = append(out, TimelineItem{Kind: "message", Message: &VisibleMessage{
				Role: VisibleRoleAgent, Text: live, CreatedAt: t.CreatedAt, Via: t.Via,
			}})
			continue
		}
		text := visibleText(t.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, TimelineItem{Kind: "message", Message: &VisibleMessage{
			Role: role, Text: text, CreatedAt: t.CreatedAt, Via: t.Via,
		}})
	}
	return out
}

// HasOpeningTurn reports whether turns contains a session's durable opening
// turn: index 0, role user — the runner's copy of the AgentSession's
// spec.prompt.inline.
//
// The runner appends it only once its pod is running, so this is false for the
// whole window between a session being created and its runner starting. A
// mirror replaying the transcript inside that window can use it to render the
// opening message from the session's own spec, rather than showing a
// conversation that appears to have begun with the agent speaking.
//
// Deliberately the SAME test the runner's own replay uses for
// hadInitialPrompt, so "the opening turn is placed" means one thing on both
// sides of the transcript.
func HasOpeningTurn(turns []memory.Turn) bool {
	for _, t := range turns {
		if t.Index == 0 && t.Role == roleUser {
			return true
		}
	}
	return false
}

// parsePlanNote returns the "plans" note carried by a system_note turn's text
// block, if it is one. Generic — no plan-type import.
func parsePlanNote(blocks []memory.ContentBlock) (planNote, bool) {
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		var pn planNote
		if err := json.Unmarshal([]byte(b.Text), &pn); err == nil && pn.Kind == "plans" {
			return pn, true
		}
	}
	return planNote{}, false
}

// applyPlanNote inserts a new plan card at first appearance, or updates the
// existing card for that name in place.
func applyPlanNote(out *[]TimelineItem, planPos map[string]int, pn planNote, createdAt time.Time) {
	name := pn.Data.Name
	if pn.Data.Op != "delete" {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(pn.Data.Plan, &p)
		name = p.Name
	}
	if name == "" {
		return
	}
	if pos, ok := planPos[name]; ok {
		item := (*out)[pos].Plan
		if pn.Data.Op == "delete" {
			item.Deleted = true
		} else {
			item.Deleted = false
			item.SnapshotJSON = append([]byte(nil), pn.Data.Plan...)
		}
		return
	}
	pi := &PlanTimelineItem{PlanName: name, CreatedAt: createdAt}
	if pn.Data.Op == "delete" {
		pi.Deleted = true
	} else {
		pi.SnapshotJSON = append([]byte(nil), pn.Data.Plan...)
	}
	planPos[name] = len(*out)
	*out = append(*out, TimelineItem{Kind: "plan", Plan: pi})
}

// visibleRole maps a stored turn role to the chat-timeline role, reporting
// false for roles that render no message.
func visibleRole(role string) (string, bool) {
	switch role {
	case roleUser, roleInbox:
		return VisibleRoleUser, true
	case roleAssistant:
		return VisibleRoleAgent, true
	default:
		return "", false
	}
}

// renderedLiveText returns the user-facing text of the tool_result blocks a
// turn carries that the live surfaces ALREADY rendered to the user, joined by a
// blank line; empty when the turn carries none — which is the case for every
// ordinary tool result.
//
// Hiding tool results is the default and stays the default: a memory query's
// rows or a kubectl dump is plumbing, and rendering it would bury the
// conversation. What is rendered here is the narrow set a producer marked, and
// only a producer can honestly mark it — the mark means "the user watched this
// go by", which nothing about a stored block can infer after the fact. See
// memory.ToolResultBlock.RenderedLive and tool.Result.RenderedLive: the
// streaming/interactive toolkit path is the only thing that sets it today,
// because it is the only tool output composed FOR A HUMAN rather than for the
// model. Nothing here knows what a toolkit is, and nothing here should.
//
// The stored Content is the enveloped form the model saw, so the envelope comes
// off before a human reads it — via toolenvelope.Unwrap, the one definition of
// that format, never a second copy of the tag. Unenveloped content (a record
// predating the envelope) passes through unchanged rather than being refused.
func renderedLiveText(blocks []memory.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type != "tool_result" || b.ToolResult == nil || !b.ToolResult.RenderedLive {
			continue
		}
		text, _ := toolenvelope.Unwrap(b.ToolResult.Content)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// visibleText returns a turn's user-facing text. When the turn delivers a reply
// via respond_to_user, that reply is authoritative and is the ONLY visible text:
// loose text blocks in the same turn are model preamble the live surfaces (chat,
// Slack) never showed, so a resumed transcript must match by dropping them too.
//
// A turn making any OTHER tool call (query_memory, a sidecar tool, …) but no
// respond_to_user is an internal reasoning step. Its text blocks are that same
// preamble, which live surfaces showed only as a transient "thinking…" caption
// and then cleared, so dropping them is what stops a resumed transcript leaking
// internal agent reasoning as an agent message the user never saw live.
//
// Only a turn with NO tool_use at all is a plain-text reply (or a user turn);
// its text blocks ARE the message. Multiple parts join with a blank line.
func visibleText(blocks []memory.ContentBlock) string {
	var responds []string
	hasToolUse := false
	for _, b := range blocks {
		if b.Type != "tool_use" || b.ToolUse == nil {
			continue
		}
		hasToolUse = true
		if b.ToolUse.Name != respondToUserToolName {
			continue
		}
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b.ToolUse.Input, &args); err == nil {
			if strings.TrimSpace(args.Text) != "" {
				responds = append(responds, args.Text)
			}
		}
	}
	if len(responds) > 0 {
		return strings.Join(responds, "\n\n")
	}
	if hasToolUse {
		// An internal tool-calling turn with no respond_to_user: any text is
		// preamble/thinking the live surfaces discarded. Contribute no message.
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}
