// Package livemirror holds the shared, session-agnostic logic for replaying a
// session's durable memory turns into the ordered timeline a live view renders
// — the piece every mirror of a live conversation (the built-in web chat, the
// read-only session-view page) needs identically. Keyed only by (ns, name) and
// operator memory access; no chat- or session-view-specific dependency belongs
// here.
package livemirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/interactionhistory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/replydelivery"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// ErrNoMemoryAccess is returned by ReadHistory when the caller has no operator
// memory access configured (empty base URL or bearer token) — callers fail
// closed with a clear sentinel rather than silently returning an empty
// history.
var ErrNoMemoryAccess = errors.New("no operator memory access; cannot replay history")

// TimelineEntry is one ordered item of a resumed conversation: a message or a
// plan or interaction card. Mirrors the client's TimelineItem (types.ts).
type TimelineEntry struct {
	InteractionRequest *channelevents.InteractionRequestPayload `json:"interactionRequest,omitempty"`
	InteractionApplied *channelevents.InteractionAppliedPayload `json:"interactionApplied,omitempty"`
	OperationID        string                                   `json:"operationID,omitempty"`
	// Kind is "message", "plan", "interaction", or a surface-authored "notice".
	Kind    string                        `json:"kind"`
	Notice  *channelevents.NoticeWire     `json:"notice,omitempty"`
	Opening *channelevents.SessionOpening `json:"opening,omitempty"`
	// Role is the chat-timeline role for a "message" entry: "user" (a human
	// message — a runner "user" turn or a channelsd "inbox" turn) or "agent"
	// (a runner "assistant" turn). Unset for "plan" entries.
	Role string `json:"role,omitempty"`
	// Text is a message entry's visible body; unset for "plan" entries.
	Text string `json:"text,omitempty"`
	// Plan is a plan entry's card payload, byte-identical to the one the live
	// path published; unset for "message" entries.
	Plan *channelevents.PlanUpdatePayload `json:"plan,omitempty"`
	// CreatedAt is the durable turn's own timestamp, and the sort key.
	CreatedAt time.Time `json:"createdAt"`
}

// History is a session's replayed conversation.
type History struct {
	// Timeline is the ordered conversation — messages and plan cards. Empty
	// means the session has said nothing yet, never a failed read (that is an
	// error).
	Timeline []TimelineEntry
	// HasOpeningTurn reports whether the durable transcript already contains
	// the session's opening turn (turn.HasOpeningTurn). False for the window
	// between a session being created and its runner placing turn 0 — a caller
	// holding the AgentSession renders the opening message from
	// spec.prompt.inline in that window, because nothing else will: turn 0 is
	// never published live (user_echo covers only view-originated messages), so
	// a mirror that read the transcript inside the window would otherwise show
	// no opening message until a manual reload.
	HasOpeningTurn bool
}

// ReadHistory reads a session's durable transcript turns from operator memory
// (via baseURL+token) and maps them to the ordered timeline — messages AND
// plan cards — a resumed conversation renders. Session-agnostic: keyed only by
// (ns, name), the memory scope every mirror of a live conversation shares.
//
// The mapping itself lives in turn.VisibleTimeline — role collapsing,
// respond_to_user extraction, dropping tool-only turns, de-duplicating a
// drained "inbox" turn against the "user" turn the runner promotes it into,
// grouping "plans" system_notes into one card per plan name — the single
// source of truth shared with every other renderer, so a resumed conversation
// matches the one the user saw live. Plan snapshots go through
// plans.Plan.ToEnvelopePayload for the same reason: a reconstructed card is
// byte-identical to the one shown live.
func ReadHistory(ctx context.Context, baseURL, token, ns, name string, logger logr.Logger) (History, error) {
	if baseURL == "" || token == "" {
		return History{}, ErrNoMemoryAccess
	}
	mem := httpclient.New(baseURL, token)
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	turns, err := turn.ReadAll(ctx, mem, scope)
	if err != nil {
		return History{}, fmt.Errorf("read transcript turns: %w", err)
	}
	replies, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{replydelivery.KindName}})
	if err != nil {
		return History{}, fmt.Errorf("read accepted replies: %w", err)
	}
	accepted := make(map[string]replydelivery.Content)
	for _, entry := range replies.Entries {
		if entry.Kind != replydelivery.KindName {
			continue
		}
		if entry.Provenance == nil || entry.Provenance.Publisher != "system:operator" {
			return History{}, fmt.Errorf("accepted reply has no operator provenance")
		}
		var c replydelivery.Content
		if err := json.Unmarshal(entry.Content, &c); err != nil {
			return History{}, err
		}
		if c.Intent.Session.Namespace != ns || c.Intent.Session.Name != name {
			return History{}, fmt.Errorf("accepted reply session mismatch")
		}
		if c.Closed {
			if err := c.Intent.Validate(); err != nil {
				return History{}, err
			}
			continue
		}
		if err := c.Receipt.Validate(c.Intent); err != nil {
			return History{}, err
		}
		accepted[c.Intent.Payload.Delivery.ToolUseID] = c
	}
	// Tool-use turns are persisted before approval and execution. Their reply
	// arguments are proposals, not visible messages. Legacy publication notes
	// establish that a live reply was sent; durable receipts remain authoritative
	// even when a crash prevents writing that note.
	published := make(map[string]bool)
	for _, t := range turns {
		if t.Role != "system_note" {
			continue
		}
		for _, block := range t.Content {
			if block.Type != "text" {
				continue
			}
			var note struct {
				Delivered []string `json:"delivered"`
			}
			if err := json.Unmarshal([]byte(block.Text), &note); err != nil {
				continue // Other system notes are not JSON publication notes.
			}
			for _, id := range note.Delivered {
				published[id] = true
			}
		}
	}
	// Suppress unconfirmed proposals and replace accepted proposals with the
	// exact wire body below. Retain tool blocks so internal preambles stay hidden.
	for ti := range turns {
		for bi := range turns[ti].Content {
			block := &turns[ti].Content[bi]
			if block.ToolUse != nil && block.ToolUse.Name == "respond_to_user" {
				if _, ok := accepted[block.ToolUse.ID]; ok || !published[block.ToolUse.ID] {
					copy := *block.ToolUse
					copy.Name = "reply_proposal"
					block.ToolUse = &copy
				}
			}
		}
	}
	items := turn.VisibleTimeline(turns)
	out := make([]TimelineEntry, 0, len(items))
	for _, it := range items {
		if it.Kind == "plan" {
			if it.Plan.Deleted {
				// A deleted plan renders as the cancelled stub the live path
				// emits: update_plan publishes an empty-items snapshot on delete,
				// which renders as a header-only card. Reproducing that stub
				// (planName + no items) keeps live == reload for the delete edge,
				// rather than silently dropping it.
				payload := (plans.Plan{Name: it.Plan.PlanName}).ToEnvelopePayload()
				out = append(out, TimelineEntry{Kind: "plan", Plan: &payload, CreatedAt: it.Plan.CreatedAt})
				continue
			}
			var p plans.Plan
			if err := json.Unmarshal(it.Plan.SnapshotJSON, &p); err != nil {
				logger.Info("livemirror: skip unparseable plan snapshot on resume", "session", ns+"/"+name, "err", err.Error())
				continue
			}
			payload := p.ToEnvelopePayload()
			out = append(out, TimelineEntry{Kind: "plan", Plan: &payload, CreatedAt: it.Plan.CreatedAt})
			continue
		}
		out = append(out, TimelineEntry{Kind: "message", Role: it.Message.Role, Text: it.Message.Text, CreatedAt: it.Message.CreatedAt})
	}
	for _, c := range accepted {
		out = append(out, TimelineEntry{Kind: "message", Role: turn.VisibleRoleAgent, Text: c.Intent.Payload.Text, OperationID: c.Receipt.OperationID, CreatedAt: c.Receipt.AcceptedAt})
	}
	interactionRecords, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{interactionhistory.KindName}})
	if err != nil {
		return History{}, fmt.Errorf("read interaction history: %w", err)
	}
	interactionItems, err := replayInteractions(interactionRecords.Entries)
	if err != nil {
		return History{}, fmt.Errorf("replay interaction history: %w", err)
	}
	out = append(out, interactionItems...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return History{Timeline: out, HasOpeningTurn: turn.HasOpeningTurn(turns)}, nil
}
