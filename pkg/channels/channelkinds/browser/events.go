package browser

import (
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// SessionRef identifies the AgentSession a render event concerns. The
// chat UI uses it to ignore events for a stale session.
type SessionRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// EventSink receives render events from the browser kind's senders.
// pkg/web/webui/chat's wsSink wraps a websocket connection to the browser
// page; tests use RecordingSink. The single method must be safe for
// concurrent calls — multiple senders push from different goroutines.
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
// Each is a plain Go value, JSON-serialized over the websocket, that the
// browser chat UI's event loop dispatches on. Senders construct and Emit them;
// they carry NO rendering logic — translation only.

// MsgUserMessage is an outbound assistant reply (KindUserMessage). Despite
// the name it carries the AGENT's text — the envelope kind is named for
// the sub-channel ("user_message"), not the speaker.
type MsgUserMessage struct {
	Delivery    *channelevents.DeliveryOperation `json:"delivery,omitempty"`
	Session     SessionRef                       `json:"session"`
	Text        string                           `json:"text"`
	Attachments []MsgAttachment                  `json:"attachments,omitempty"`
}

// MsgSessionOpening is the inspectable opening notice, distinct from an agent
// reply so live delivery and transcript bootstrap can share one card.
type MsgSessionOpening struct {
	Session SessionRef                    `json:"session"`
	Opening *channelevents.SessionOpening `json:"opening"`
}

// MsgAttachment is one artifact the agent attached to a reply (respond_to_user's
// `attached`). The browser chat UI renders it as a download chip whose link is
// the same-origin /artifact-download?artifactId=…&fn=… (gated by the viewer's
// CheckView); it carries only the fields that chip needs.
type MsgAttachment struct {
	ArtifactID string `json:"artifactId"`
	Filename   string `json:"filename"`
	// MIME is the artifact's declared content type; empty means unknown.
	MIME string `json:"mime,omitempty"`
}

// MsgUserEcho is the outbound mirror of a view-originated user message
// (KindUserEcho) — a message another view of this session (the artifact view,
// or another web-chat tab) typed, surfaced in THIS chat so every view sees the
// same conversation. Unlike MsgUserMessage (the AGENT's reply) this carries a
// USER's text and renders as a user bubble.
type MsgUserEcho struct {
	Session SessionRef `json:"session"`
	Text    string     `json:"text"`
	// Author labels the sender for a human; empty when none resolved.
	Author string `json:"author,omitempty"`
	// Via is the view URN of the surface the message was typed on.
	Via string `json:"via,omitempty"`
	// RequestID is the send's idempotency key, so the originating tab can suppress its own echo.
	RequestID string `json:"requestID,omitempty"`
}

// MsgNotification is an update_status / mid-flight status line
// (KindNotification).
type MsgNotification struct {
	Session SessionRef `json:"session"`
	Text    string     `json:"text"`
	// Short is a compact variant for a narrow caption; empty means use Text.
	Short string `json:"short,omitempty"`
	// ExpectedDurationSeconds is the agent's own estimate; 0 means it gave none.
	ExpectedDurationSeconds int `json:"expectedDurationSeconds,omitempty"`
	// Ephemeral marks a one-shot announcement the UI shows briefly then decays
	// to a neutral "Working…", as opposed to a persistent update_status caption
	// (Ephemeral unset) that stays until superseded.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// MsgPlanUpdate is a structured plan snapshot (KindPlanUpdate).
type MsgPlanUpdate struct {
	Session SessionRef                      `json:"session"`
	Payload channelevents.PlanUpdatePayload `json:"payload"`
}

// MsgToolSessionEvent is one parsed interactive-tool event
// (KindToolSessionEvent).
type MsgToolSessionEvent struct {
	Session SessionRef                            `json:"session"`
	Payload channelevents.ToolSessionEventPayload `json:"payload"`
}

// MsgToolSessionDelta is one raw interactive-tool output chunk
// (KindToolSessionDelta) — stdout/stderr bytes, plus the terminal
// exit delta.
type MsgToolSessionDelta struct {
	Session SessionRef                            `json:"session"`
	Payload channelevents.ToolSessionDeltaPayload `json:"payload"`
}

// MsgPermissionRequest is a multiplayer session-join request
// (KindPermissionRequest). v1 browser sessions are single-user, so this
// is rendered as an informational timeline note, not a modal.
type MsgPermissionRequest struct {
	Session SessionRef                             `json:"session"`
	Payload channelevents.PermissionRequestPayload `json:"payload"`
}

// MsgPermissionDecisionApplied resolves a permission request
// (KindPermissionDecisionApplied).
type MsgPermissionDecisionApplied struct {
	Session SessionRef                                     `json:"session"`
	Payload channelevents.PermissionDecisionAppliedPayload `json:"payload"`
}

// MsgStreamDelta is one live LLM stream event
// (KindAssistantStreamDelta).
type MsgStreamDelta struct {
	Session SessionRef                                `json:"session"`
	Payload channelevents.AssistantStreamDeltaPayload `json:"payload"`
}

// MsgTurnProgress is a throttled in-flight progress snapshot
// (KindTurnProgress): cumulative token usage + elapsed seconds for the turn
// the agent is actively working. The web chat UI renders it as a subtle
// status line ("working · 34s · 1.2K in · 6.4K out") while the turn runs,
// mirroring the Slack channel's assistant-status caption. Pure metrics — the
// browser owns the spinner frame and number formatting.
type MsgTurnProgress struct {
	Session SessionRef                        `json:"session"`
	Payload channelevents.TurnProgressPayload `json:"payload"`
}

// MsgToolProgress is a throttled snapshot of a running SYNC sandbox tool
// (KindToolProgress): while the LLM is blocked awaiting a long tool it cannot
// narrate, the sandbox publishes per-tool progress. The main Sender must accept
// it — an unhandled kind surfaces as a repeating send_error in the chat, once
// per throttled snapshot.
type MsgToolProgress struct {
	Session SessionRef                        `json:"session"`
	Payload channelevents.ToolProgressPayload `json:"payload"`
}

// MsgOperationActivity is a throttled snapshot of the runner's live operation
// tree (KindOperationActivity): the active operation subtree plus its active
// + recent tool calls. The browser kind routes it through the main Sender so
// the browser page can render live progress, mirroring the Slack/local
// channels' EffectiveLine-resolved caption.
type MsgOperationActivity struct {
	Session SessionRef                             `json:"session"`
	Payload channelevents.OperationActivityPayload `json:"payload"`
}

// MsgTurnActivity is the coarse active⇄paused turn signal (KindTurnActivity).
// The UI uses it to CLEAR the "working" throbber even on a turn that produced
// no final user_message (a plan-only or notification-only turn) — it is the
// authoritative "is the agent working" transition.
//
// Unlike the other Msg* types this does NOT flow through a Sender: the outbound
// relay routes KindTurnActivity to its OnTurnActivity seam before any Sender is
// reached, and the web chat's Registry wires that seam to this event.
type MsgTurnActivity struct {
	Session SessionRef `json:"session"`
	// Active false means the turn finished and the agent awaits the next message.
	Active bool `json:"active"`
	// Cause is the channelevents.PauseCause* reason; only meaningful when Active is false.
	Cause string `json:"cause,omitempty"`
}

// MsgLiveViewOffer is a ready-to-follow browser link, rendered as a "View live"
// link. Two sub-channel senders share it because both are the same "here's a
// link" shape: liveViewOfferSender for an artifact's live view, and
// sessionViewOfferSender for an interactive session-view escalation.
type MsgLiveViewOffer struct {
	Session SessionRef `json:"session"`
	// URL is always non-empty here; the sender discards the minter's clean-skip.
	URL string `json:"url"`
}

// MsgInterruptApplied is the runner's answer to a channel-side interrupt
// request (KindInterruptApplied), delivered on the queued_messages sub-channel.
type MsgInterruptApplied struct {
	Session SessionRef `json:"session"`
	// RequestID correlates this back to the interrupt request the UI issued.
	RequestID string `json:"requestID"`
	// Outcome is "interrupted" or "rejected".
	Outcome string `json:"outcome"`
	// Reason explains a rejection for a human; empty on the interrupted path.
	Reason string `json:"reason,omitempty"`
}

// MsgInteractionRequest is a generic interaction prompt (KindInteractionRequest)
// — the unified request/applied/decision envelope covering every prompt type
// (tool approval, info leakage, credential link, …). The browser chat UI renders
// it per Category using the shared Lead/Body/Fields/Actions/Audience shape.
type MsgInteractionRequest struct {
	Session SessionRef                              `json:"session"`
	Payload channelevents.InteractionRequestPayload `json:"payload"`
}

// MsgInteractionApplied resolves a pending interaction prompt
// (KindInteractionApplied) — the decision (or timeout) outcome; the UI
// renders it as an in-place edit of the original prompt.
type MsgInteractionApplied struct {
	Session SessionRef                              `json:"session"`
	Payload channelevents.InteractionAppliedPayload `json:"payload"`
}

// MsgInteractionRejected is a per-clicker decision rejection
// (KindInteractionDecisionRejected) — the decision pipe refused the click
// (the clicker lacked standing, was a spectator on an already-resolved prompt,
// a category mismatch, or the bound handler failed). The browser chat UI
// renders it as a visible timeline note scoped to the clicker; the shared
// prompt is left intact (a rejected click never resolves the card). Payload
// carries Class/Reason plus, for the already_resolved class, the original
// decider + outcome.
type MsgInteractionRejected struct {
	Session SessionRef                                       `json:"session"`
	Payload channelevents.InteractionDecisionRejectedPayload `json:"payload"`
}

// MsgSendError is emitted by any sender that fails to parse or process its
// envelope. The UI renders it as a visible timeline warning — never silently
// dropped.
type MsgSendError struct {
	Session SessionRef `json:"session"`
	// Kind is the envelope kind that failed, not the channel kind.
	Kind channelevents.Kind `json:"kind"`
	Err  string             `json:"err"`
	// At is when the send failed.
	At time.Time `json:"at"`
}
