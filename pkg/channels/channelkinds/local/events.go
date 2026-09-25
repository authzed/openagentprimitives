package local

import (
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// SessionRef identifies the AgentSession a render event concerns. The
// TUI uses it to ignore events for a stale session.
type SessionRef struct {
	Namespace string
	Name      string
}

// EventSink receives render events from the local kind's senders. The
// production implementation wraps a bubbletea *tea.Program (via the
// ProgramSink adapter in cmd/oap); tests use RecordingSink. The single
// method must be safe for concurrent calls — multiple senders push
// from different goroutines.
type EventSink interface {
	Emit(msg any)
}

// RecordingSink is the test EventSink: it appends every emitted event
// to a slice. Safe for concurrent Emit.
type RecordingSink struct {
	mu     sync.Mutex
	events []any
}

func (s *RecordingSink) Emit(msg any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, msg)
}

// Events returns a snapshot of every event emitted so far.
func (s *RecordingSink) Events() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.events...)
}

// --- render event types ---
// Each is a bubbletea tea.Msg (any value qualifies). The TUI's Update
// type-switches on them. Senders construct and Emit them; they carry NO
// rendering logic — translation only.

// MsgUserMessage is an outbound assistant reply (KindUserMessage). Despite
// the name it carries the AGENT's text — the envelope kind is named for
// the sub-channel ("user_message"), not the speaker.
type MsgUserMessage struct {
	Session SessionRef
	Text    string
}

// MsgNotification is an update_status / mid-flight status line
// (KindNotification).
type MsgNotification struct {
	Session                 SessionRef
	Text                    string
	Short                   string
	ExpectedDurationSeconds int
	// Ephemeral mirrors channelevents.NotificationPayload.Ephemeral: a one-shot
	// l.Notify announcement (e.g. "Picking up N messages you sent.") a surface
	// shows briefly then decays to a neutral "Working…", as opposed to a
	// persistent update_status caption (Ephemeral unset) that stays until
	// superseded.
	Ephemeral bool
}

// MsgTurnProgress is a throttled in-flight progress snapshot
// (KindTurnProgress): cumulative token usage + elapsed seconds for the turn the
// agent is actively working. Pure metrics — the surface owns the spinner frame
// and number formatting, mirroring the Slack channel's assistant-status caption
// and the web chat's live status line.
type MsgTurnProgress struct {
	Session SessionRef
	Payload channelevents.TurnProgressPayload
}

// MsgToolProgress is a per-tool liveness/progress tick for a running sync tool
// (KindToolProgress). Done=true means the tool finished.
type MsgToolProgress struct {
	Session        SessionRef
	CallID         string
	Name           string
	BudgetSeconds  int
	ElapsedSeconds int
	Done           bool
	Percent        *int32
	TailLine       string
	EtaSeconds     *int32
}

// MsgPlanUpdate is a structured plan snapshot (KindPlanUpdate).
type MsgPlanUpdate struct {
	Session SessionRef
	Payload channelevents.PlanUpdatePayload
}

// MsgOperationActivity is a throttled snapshot of the runner's live operation
// tree (KindOperationActivity): the active operation subtree plus its active
// + recent tool calls. The TUI renders Payload.CompactLine as the inline
// status line, mirroring the Slack/web-chat channels' resolved caption.
type MsgOperationActivity struct {
	Session SessionRef
	Payload channelevents.OperationActivityPayload
}

// MsgToolSessionEvent is one parsed interactive-tool event
// (KindToolSessionEvent).
type MsgToolSessionEvent struct {
	Session SessionRef
	Payload channelevents.ToolSessionEventPayload
}

// MsgToolSessionDelta is one raw interactive-tool output chunk
// (KindToolSessionDelta) — stdout/stderr bytes, plus the terminal
// exit delta.
type MsgToolSessionDelta struct {
	Session SessionRef
	Payload channelevents.ToolSessionDeltaPayload
}

// MsgPermissionRequest is a multiplayer session-join request
// (KindPermissionRequest). v1 local sessions are single-user, so this
// is rendered as an informational timeline note, not a modal.
type MsgPermissionRequest struct {
	Session SessionRef
	Payload channelevents.PermissionRequestPayload
}

// MsgPermissionDecisionApplied resolves a permission request
// (KindPermissionDecisionApplied).
type MsgPermissionDecisionApplied struct {
	Session SessionRef
	Payload channelevents.PermissionDecisionAppliedPayload
}

// MsgStreamDelta is one live LLM stream event
// (KindAssistantStreamDelta).
type MsgStreamDelta struct {
	Session SessionRef
	Payload channelevents.AssistantStreamDeltaPayload
}

// MsgLiveViewOffer is a ready-to-follow browser link the TUI renders as a
// "View in browser: <url>" timeline note. Two sub-channel senders share it
// because both are the same "here's a link" shape: liveViewOfferSender for an
// artifact's live view, and sessionViewOfferSender for an interactive
// session-view escalation. URL is never empty — the sender discards the
// minter's clean-skip before emitting.
type MsgLiveViewOffer struct {
	Session SessionRef
	URL     string
}

// MsgAgentUIOffer is the agent's own offer of this session's dashboard, which
// the TUI renders as its own timeline note.
//
// Deliberately NOT folded into MsgLiveViewOffer: that event renders as a bare
// "View in browser" line, so a third sharer would be indistinguishable from an
// artifact's live view. URL is always non-empty — the sender treats an empty
// minted URL as a loud failure and emits MsgSendError instead.
type MsgAgentUIOffer struct {
	Session SessionRef
	URL     string
}

// MsgInterruptApplied is the outcome of a mid-turn interrupt request
// (KindInterruptApplied), delivered on the queued_messages sub-channel: the
// runner's answer to a channel-side interrupt request the TUI issued.
// RequestID correlates it back to that request. Outcome is
// "interrupted" | "rejected"; Reason carries a human-readable explanation
// for a rejection.
type MsgInterruptApplied struct {
	Session   SessionRef
	RequestID string
	Outcome   string
	Reason    string
}

// MsgInteractionRequest is a semantic interaction prompt (KindInteractionRequest
// on the interaction sub-channel). It carries the decode-able Payload the TUI's
// generic decision modal reads, plus a pre-rendered Text the timeline uses for
// the read-only path where no modal opens. A payload with ActionKindDecision
// actions opens a blocking decision modal; link / link_mint actions stay
// read-only labels.
type MsgInteractionRequest struct {
	Session SessionRef
	Payload channelevents.InteractionRequestPayload
	Text    string
}

// MsgInteractionApplied resolves a pending interaction prompt
// (KindInteractionApplied on the interaction sub-channel). The TUI renders it
// as an in-place edit of the original prompt showing the outcome.
//
// Category is carried because OutcomeText's meaning depends on it —
// identity_choice puts a raw action id there, credential_link a credential
// name — so a surface cannot label the outcome correctly without it (see
// channelinteractions/categories.OutcomeLabel).
type MsgInteractionApplied struct {
	Session     SessionRef
	Category    string
	OutcomeText string
	Outcome     string
}

// MsgInteractionRejected is a per-clicker decision rejection
// (KindInteractionDecisionRejected on the interaction sub-channel): the decision
// pipe refused the click — no standing, a spectator on an already-resolved
// prompt, a category mismatch, or a failed handler. The TUI renders it as a
// visible timeline note and leaves the shared prompt intact, since a rejected
// click never resolves the card. Class is the machine-readable reason; Reason
// the human one; OriginalOutcome is set only for the already_resolved class.
type MsgInteractionRejected struct {
	Session         SessionRef
	Class           string
	Reason          string
	OriginalOutcome string
}

// MsgSendError is emitted by any sender that fails to parse or process
// its envelope. The TUI renders it as a visible timeline warning —
// never silently dropped (AGENTS.md). At is the failure time.
type MsgSendError struct {
	Session SessionRef
	Kind    channelevents.Kind
	Err     string
	At      time.Time
}
