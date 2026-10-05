// Mirrors the Go JSON shapes from pkg/web/webui/chat/handlers.go (messageRequest/
// messageResponse, sessionDetail, messagesResponse) and pkg/web/webui/chat/sink.go's
// wsFrame/toFrame (which wraps pkg/channels/channelkinds/browser/events.go's Msg* render
// events). Every endpoint these describe is session-scoped and addressed by
// path: /sessions/api/{ns}/{name}/... .

// NoticeWire mirrors channelevents.NoticeWire — the serialised form of a
// one-way, user-facing notice (a refusal, a takeover, a continuation in a new
// session). It rides on the NON-routed outcomes and on takeover/inherit
// continuations, and it is the surface's ONLY copy of that message: nothing
// else is published for those paths, so dropping it is silence.
//
// `tone` is denormalised from the category (channelinteractions.Tone) exactly
// so a surface can pick a colour without holding the registry — see
// NoticeCard's toneStyle. `excerpt` is UNTRUSTED (see InteractionExcerpt):
// render it inert and fenced, never as markup.
export interface NoticeWire {
  category?: string;
  // tone: critical | privacy | degraded | waiting | routine | housekeeping |
  // resolved (channelinteractions.Tone). Unknown values render neutrally.
  tone?: string;
  terminal?: boolean;
  // glyph: "" | "money" | "clock" (channelinteractions.Glyph) — an optional
  // semantic mark that REPLACES the tone's own icon.
  glyph?: string;
  lead: string;
  body?: string;
  nextStep?: string;
  fields?: InteractionField[];
  excerpt?: InteractionExcerpt;
}

// MessageResponse is the POST .../message response body. `notice` is the
// structured user-facing message (messageResponse.Notice in Go) — there is no
// second copy of it on the websocket, so a send that comes back non-routed
// renders nothing at all unless this is read.
export interface MessageResponse {
  sessionId: string;
  routed: boolean;
  ended: boolean;
  notice?: NoticeWire;
}

// SessionDetail is the GET .../detail info-panel payload. Mirrors chat's
// sessionDetail (Go) and the field set Slack's "Show settings" surface shows.
export interface SessionDetail {
  sessionId: string;
  agentClass: string;
  model: { provider: string; name: string };
  maxTokens: number;
  maxTurns: number;
  maxDuration: string;
  owner: string;
  createdAt: string;
  phase: string;
  // notices are the statements this session's status implies; empty when it is
  // healthy. See SessionNotice for why they ride on detail rather than arriving
  // as channel envelopes.
  notices?: SessionNotice[];
}

// TimelineItem is one ordered entry of a replayed conversation from
// GET .../messages: a message, plan-card snapshot, or session notice. Mirrors
// chat's timelineEntry (Go).
export type TimelineItem =
  | { kind: "interaction"; interactionRequest: InteractionRequestInner; interactionApplied?: InteractionAppliedInner; createdAt: string }
  | { kind: "opening"; opening: SessionOpening; createdAt: string }
  | { kind: "notice"; notice: NoticeWire; createdAt: string }
  | { kind: "message"; role: "user" | "agent"; text: string; createdAt: string; operationID?: string }
  | { kind: "plan"; plan: PlanUpdateInner; createdAt: string };

export interface SessionOpening {
  summary: string;
  instructions: string;
}

// MessagesResponse is the GET .../messages response body: the ordered timeline
// replayed when a conversation is opened or resynced after a reconnect.
export interface MessagesResponse {
  timeline: TimelineItem[];
}

// ConnState is the chat websocket's connection state, surfaced by the
// transcript's own connection indicator: a single connecting attempt, an
// established link, a retry in flight, or a sustained outage (still retrying in
// the background).
export type ConnState = "connecting" | "connected" | "reconnecting" | "offline";

export interface SessionRef {
  namespace: string;
  name: string;
}

// MsgAttachment mirrors builtin.MsgAttachment: an artifact the agent attached to
// a reply. The chat builds a same-origin download link from it.
export interface MsgAttachment {
  artifactId: string;
  filename: string;
  mime?: string;
}

// UserMessagePayload mirrors builtin.MsgUserMessage. NOTE: despite the
// "user_message" / MsgUserMessage name, `text` carries the AGENT's finalized
// reply (the runner's respond_to_user), NOT a user send — the name is a
// historical artifact of the builtin render-event vocabulary.
export interface UserMessagePayload {
  delivery?: { id: string };
  session: SessionRef;
  text: string;
  attachments?: MsgAttachment[];
}

// UserEchoPayload mirrors builtin.MsgUserEcho: a user message that ANOTHER view
// of this session (the artifact view, or another chat tab) typed, mirrored into
// this chat so every view shows the same conversation. requestID is the
// originating send's idempotency key — this tab suppresses the echo of a
// message it already rendered optimistically (same requestID as one of its own
// lines) while showing echoes from other surfaces (whose requestID it never used).
export interface UserEchoPayload {
  session: SessionRef;
  text: string;
  author?: string;
  via?: string;
  requestID?: string;
}

// LiveViewOfferPayload mirrors builtin.MsgLiveViewOffer: a ready-to-follow
// artifact live-view link the agent offered via artifact_offer_view. url is
// empty when the viewer origin isn't configured (a clean skip).
export interface LiveViewOfferPayload {
  session: SessionRef;
  url: string;
}

export interface NotificationPayload {
  session: SessionRef;
  text: string;
  short?: string;
  expectedDurationSeconds?: number;
  // ephemeral marks a one-shot announcement (the runner's l.Notify — "Picking up
  // N messages you sent.", "<agent> thinking…") as opposed to a persistent
  // update_status caption. An ephemeral notice shows briefly, then decays to a
  // generic "Working…" on the next activity frame instead of lingering as the
  // caption for the rest of the turn. Mirrors builtin.MsgNotification.Ephemeral.
  ephemeral?: boolean;
}

export interface PlanItemRef {
  id: string;
  label: string;
  status: string;
  details?: string;
  output?: string;
  operationId?: string;
  // phase names the PlanPhaseRef this step is grouped under, for DISPLAY only.
  // Empty/absent means ungrouped, which is legal alongside grouped siblings —
  // the renderer must place those steps somewhere rather than drop them.
  // Mirrors channelevents.PlanItemRef.Phase.
  phase?: string;
}

// PlanPhaseRef mirrors channelevents.PlanPhaseRef — one declared phase of a
// plan, as a display heading.
//
// Both strings are AGENT-AUTHORED and untrusted, exactly like the step labels
// beside them. React escapes them on render; never parse one or key anything
// on it. In particular this `label` is NOT the same thing as the approval
// card's phase title: the card titles phases by INDEX precisely because the
// agent's label must not carry authority. Show the label as the agent's words,
// never as the name of the thing being approved.
export interface PlanPhaseRef {
  id: string;
  label: string;
}

export interface PlanUpdateInner {
  planName: string;
  parentPlan?: string;
  parentItem?: string;
  items: PlanItemRef[] | null;
  updatedAt: string;
  paused?: boolean;
  pauseCause?: string;
  // phases is the staging the agent declared, in order. Absent means the plan
  // declared none — the ordinary single-phase shape — and the renderer falls
  // back to a flat list. Mirrors channelevents.PlanUpdatePayload.Phases.
  phases?: PlanPhaseRef[];
}

export interface PlanUpdatePayload {
  session: SessionRef;
  payload: PlanUpdateInner;
}

export interface SendErrorPayload {
  session: SessionRef;
  kind: string;
  err: string;
  at: string;
}

// StreamDeltaInner mirrors channelevents.AssistantStreamDeltaPayload — one live
// LLM stream event. Only text_delta (partial assistant text) is rendered by the
// chat UI; tool_use_* / stop are accepted but not shown.
export interface StreamDeltaInner {
  eventType: string; // "text_delta" | "tool_use_start" | "tool_use_stop" | "stop"
  text?: string;
  toolName?: string;
  toolId?: string;
  blockIdx: number;
}

// StreamDeltaPayload mirrors builtin.MsgStreamDelta (events.go): the wsFrame
// payload wraps the inner AssistantStreamDeltaPayload under `payload`.
export interface StreamDeltaPayload {
  session: SessionRef;
  payload: StreamDeltaInner;
}

// TurnProgressInner mirrors channelevents.TurnProgressPayload — a throttled
// cumulative token/elapsed snapshot for the in-flight turn.
export interface TurnProgressInner {
  inputTokens: number;
  outputTokens: number;
  elapsedSeconds: number;
  seq: number;
}

// TurnProgressPayload mirrors builtin.MsgTurnProgress (events.go).
export interface TurnProgressPayload {
  session: SessionRef;
  payload: TurnProgressInner;
}

// OperationActivityCallNode mirrors channelevents.OperationActivityCallNode: one
// active/recent tool call under an operation node.
export interface OperationActivityCallNode {
  tool: string;
  reason: string;
  elapsedSeconds: number;
  active: boolean;
}

// OperationActivityNode mirrors channelevents.OperationActivityNode: one node
// in the runner's live operation tree (an operation, optionally with a
// planItem link and its active + recent tool calls beneath it).
export interface OperationActivityNode {
  id: string;
  description: string;
  parentOpId?: string;
  planItem?: PlanItemRef;
  elapsedSeconds: number;
  active: boolean;
  calls?: OperationActivityCallNode[];
}

// OperationActivityInner mirrors channelevents.OperationActivityPayload: a
// throttled snapshot of the runner's active operation subtree. cleared marks
// the final tick (no operation is active) — the UI tears down the tree.
export interface OperationActivityInner {
  operations?: OperationActivityNode[];
  compactLine?: string;
  cleared?: boolean;
}

// OperationActivityPayload mirrors builtin.MsgOperationActivity (events.go):
// the wsFrame payload wraps the inner OperationActivityPayload under
// `payload`, same double-nesting as TurnProgressPayload/StreamDeltaPayload
// (verified against pkg/web/webui/chat/sink.go's toFrame, which emits
// `Payload: m` for this Msg* the same way it does for turn_progress).
export interface OperationActivityPayload {
  session: SessionRef;
  payload: OperationActivityInner;
}

// ToolSessionEventInner mirrors channelevents.ToolSessionEventPayload: one
// PARSED event from an interactive tool's stdout (the claude-stream-json
// path). Reason and outerTool ride on every event so a renderer's header needs
// no accumulated state. Note toolName is the streaming agent's OWN internal
// tool (Write, Read, …) — outerTool is the dispatched, agent-facing one.
export interface ToolSessionEventInner {
  toolCallRef: string;
  eventType?: string; // text_delta | tool_use_start | tool_use_stop | result
  text?: string;
  toolName?: string;
  toolId?: string;
  outerTool?: string;
  reason?: string;
  summary?: string;
  ok?: boolean;
  durationMs?: number;
  costUsd?: number;
}

// ToolSessionDeltaInner mirrors channelevents.ToolSessionDeltaPayload: one RAW
// output chunk. `data` is Go []byte, so it arrives BASE64-encoded — and chunks
// are not line-aligned, so it must be reassembled (see toolSession.ts).
export interface ToolSessionDeltaInner {
  toolCallRef: string;
  stream?: string; // "stdout" | "stderr"
  data?: string; // base64
  terminal?: boolean;
  exitReason?: string;
  exitCode?: number;
}

// ToolSession*Payload mirror builtin.MsgToolSession{Event,Delta} (events.go):
// the wsFrame payload wraps the inner payload under `payload`, the same
// double-nesting as TurnProgressPayload/OperationActivityPayload.
export interface ToolSessionEventPayload {
  session: SessionRef;
  payload: ToolSessionEventInner;
}

export interface ToolSessionDeltaPayload {
  session: SessionRef;
  payload: ToolSessionDeltaInner;
}

// TurnActivityPayload mirrors builtin.MsgTurnActivity (events.go): the coarse
// active⇄paused turn signal. active=false means the agent stopped turning — the
// authoritative "stop working" signal — and `cause` says why.
export interface TurnActivityPayload {
  session: SessionRef;
  active: boolean;
  // cause is one of channelevents' PauseCause* values, meaningful only when
  // active is false: "awaiting_reply" (the agent ASKED and is blocked on the
  // answer), "idle" (the agent finished; the session is parked and open for a
  // next message nobody owes it), "awaiting_approval",
  // "awaiting_leakage_approval", "awaiting_identity_choice", "awaiting_retry",
  // "complete", "failed", "stopped". Only awaiting_reply is a question.
  cause?: string;
}

// InterruptAppliedPayload mirrors builtin.MsgInterruptApplied (events.go):
// the runner's answer to a channel-side mid-turn interrupt request,
// delivered on the queued_messages sub-channel. requestID correlates it back
// to the request the UI issued; outcome is "interrupted" | "rejected".
export interface InterruptAppliedPayload {
  session: SessionRef;
  requestID: string;
  outcome: "interrupted" | "rejected";
  reason?: string;
}

// SessionEndedPayload mirrors chat's sessionEndedMsg (sink.go). Reason is one
// of "succeeded" | "failed" | "idle_timeout" | "server_shutdown".
export interface SessionEndedPayload {
  session: SessionRef;
  reason: string;
  failureReason?: string;
  failureMessage?: string;
}

// SessionStartupPayload mirrors sink.go's sessionStartupMsg: the health
// watcher's startup line, every pre-start tick, and the one frame with
// `started` that ends it.
export interface SessionStartupPayload {
  session: SessionRef;
  text: string;
  short?: string;
  stillTrying?: boolean;
  started?: boolean;
}

// InteractionField is one label/value row on an interaction card ("Tool:
// git_push"). Mirrors channelevents.InteractionField — publisher-authored and
// trusted, rendered as live markup (unlike InteractionExcerpt).
export interface InteractionField {
  label: string;
  value: string;
  // items is the same content as STRUCTURE, for surfaces that can lay it out.
  // Mirrors channelevents.InteractionField.Items. When present it is rendered
  // instead of value — the two say the same thing, and value remains what a
  // plain-text surface prints. Absent for every field whose value is one line.
  items?: InteractionItem[];
}

// InteractionItem is one line of a structured field, optionally with children.
// Mirrors channelevents.InteractionItem. Publisher-authored and trusted, like
// InteractionField.value.
export interface InteractionItem {
  text: string;
  detail?: string;
  // tone is emphasis, never meaning: the line's own text says what it is, so a
  // renderer that ignored tone entirely would still be telling the truth.
  // "readonly" | "readwrite" | "external" are the three blast-radius tiers
  // (channelevents.ToneReadonly/ToneReadwrite/ToneExternal); "muted" marks
  // supporting detail unrelated to blast radius.
  tone?: "readonly" | "readwrite" | "external" | "muted";
  // hint is supplementary text a surface MAY reveal on demand — a tooltip —
  // never rendered inline as part of the line. Typically the raw permission
  // handle: a human decides from `text`, never from this. Mirrors
  // channelevents.InteractionItem.Hint.
  hint?: string;
  items?: InteractionItem[];
  // icon names a glyph from a CLOSED, code-defined registry — never a URL.
  // Publisher-authored, from a resource type's declared display, never from
  // anything an agent wrote. An unrecognized name renders NO icon — never a
  // fallback image, never a guess. Mirrors channelevents.InteractionItem.Icon.
  icon?: string;
  // href, when set, makes `detail` a real link. The publisher guarantees
  // href is byte-identical to detail; the renderer must not trust that
  // alone — it renders the href STRING ITSELF as both the anchor's text and
  // its target, so the two can never diverge no matter what the payload
  // says. https only. Mirrors channelevents.InteractionItem.Href.
  href?: string;
}

// InteractionExcerpt is UNTRUSTED content shown for human judgment (a
// content-inspection preview, tool output, ...). Mirrors
// channelevents.InteractionExcerpt. CONTRACT: every field (label and content
// alike) MUST render inert — inside a <pre>/<code> block, never interpreted
// as markup — same rule the Go side documents on the struct.
export interface InteractionExcerpt {
  label?: string;
  content: string;
}

// InteractionAction is one thing the user can do with an interaction prompt.
// Mirrors channelevents.InteractionAction. kind "link"/"link_mint" render as
// an anchor (url); "decision" renders as a button that posts a decision via
// onDecision. style is a rendering hint only ("primary" | "danger" | "").
export interface InteractionAction {
  id: string;
  label: string;
  style?: string;
  kind: "decision" | "link" | "link_mint";
  url?: string;
}

// InteractionRequestInner mirrors channelevents.InteractionRequestPayload —
// the semantic request body (Lead/Body/Fields/Excerpt/Actions/Audience) that
// replaced the per-prompt-type kind families (tool approval, credential
// link, info leakage, ...). CONTRACT: every field EXCEPT excerpt is
// publisher-authored/trusted; excerpt is untrusted (see InteractionExcerpt).
export interface InteractionRequestInner {
  agentSessionRef: SessionRef;
  category: string;
  requestRef: string;
  lead: string;
  body?: string;
  fields?: InteractionField[];
  excerpt?: InteractionExcerpt;
  actions?: InteractionAction[];
  details?: unknown;
  audience: { scope: string };
  expiresAt?: string;
  interruptible?: boolean;
}

// InteractionRequestPayload mirrors builtin.MsgInteractionRequest
// (events.go): the wsFrame payload wraps the inner InteractionRequestInner
// under `payload`, same double-nesting as TurnProgressPayload/
// OperationActivityPayload (sink.go's toFrame emits `Payload: m` for this
// Msg* the same way — verified against pkg/web/webui/chat/sink.go's toFrame).
export interface InteractionRequestPayload {
  session: SessionRef;
  payload: InteractionRequestInner;
}

// InteractionAppliedInner mirrors channelevents.InteractionAppliedPayload —
// the decision (or timeout) outcome that resolves a pending interaction
// prompt in place. outcome is one of "approved" | "denied" | "expired" |
// "resolved" (channelevents.Outcome* constants); mintedUrl carries an
// ActionKindLinkMint result.
export interface InteractionAppliedInner {
  agentSessionRef: SessionRef;
  category: string;
  requestRef: string;
  outcome: string;
  outcomeText?: string;
  reason?: string;
  mintedUrl?: string;
}

// InteractionAppliedPayload mirrors builtin.MsgInteractionApplied
// (events.go) — same wsFrame double-nesting as InteractionRequestPayload.
export interface InteractionAppliedPayload {
  session: SessionRef;
  payload: InteractionAppliedInner;
}

// ExternalIdentity mirrors channelevents.ExternalIdentity — only the fields
// the browser chat UI needs to build a human-readable name (email preferred,
// externalId as a fallback). kind/teamScope/subject travel on the wire but
// are unused here.
export interface ExternalIdentity {
  kind: string;
  externalId: string;
  email?: string;
}

// InteractionDecisionRejectedInner mirrors
// channelevents.InteractionDecisionRejectedPayload — a per-clicker decision
// rejection (KindInteractionDecisionRejected): the decision pipe refused this
// click (the clicker lacked standing, the prompt was already resolved by
// someone else, a category mismatch, or the bound handler failed — e.g. a
// tool_approval grant-write error). class is machine-readable:
// "not_authorized" | "already_resolved" | "handler_error" |
// "category_mismatch". originalDecider/originalOutcome are set only for the
// already_resolved class, naming who actually resolved it.
export interface InteractionDecisionRejectedInner {
  agentSessionRef: SessionRef;
  category: string;
  requestRef: string;
  class: string;
  reason?: string;
  originalDecider?: ExternalIdentity;
  originalOutcome?: string;
}

// InteractionDecisionRejectedPayload mirrors builtin.MsgInteractionRejected
// (events.go) — same wsFrame double-nesting as InteractionAppliedPayload.
export interface InteractionDecisionRejectedPayload {
  session: SessionRef;
  payload: InteractionDecisionRejectedInner;
}

// ChatFrame is the discriminated union of every wsFrame `type` the v1 chat UI
// renders. sink.go's toFrame also emits tool/approval/permission/stream
// frame types (multi-turn interactive-tool + multiplayer sessions) that v1
// chat intentionally does not render here — the built-in chat agent is a
// plain-text conversational agent with no tool UI surface in this view. Those
// arrive as the fallback variant below and are ignored (not an error: they're
// simply outside v1's rendered vocabulary, not a malformed frame).
export type ChatFrame =
  | { type: "session_opening"; session: SessionRef; payload: { session: SessionRef; opening: SessionOpening } }
  | { type: "user_message"; session: SessionRef; payload: UserMessagePayload }
  | { type: "user_echo"; session: SessionRef; payload: UserEchoPayload }
  | { type: "notification"; session: SessionRef; payload: NotificationPayload }
  | { type: "plan_update"; session: SessionRef; payload: PlanUpdatePayload }
  | { type: "stream_delta"; session: SessionRef; payload: StreamDeltaPayload }
  | { type: "turn_progress"; session: SessionRef; payload: TurnProgressPayload }
  | { type: "turn_activity"; session: SessionRef; payload: TurnActivityPayload }
  | { type: "operation_activity"; session: SessionRef; payload: OperationActivityPayload }
  | { type: "interrupt_applied"; session: SessionRef; payload: InterruptAppliedPayload }
  | { type: "send_error"; session: SessionRef; payload: SendErrorPayload }
  | { type: "session_ended"; session: SessionRef; payload: SessionEndedPayload }
  | { type: "session_startup"; session: SessionRef; payload: SessionStartupPayload }
  | { type: "live_view_offer"; session: SessionRef; payload: LiveViewOfferPayload }
  | { type: "interaction_request"; session: SessionRef; payload: InteractionRequestPayload }
  | { type: "interaction_applied"; session: SessionRef; payload: InteractionAppliedPayload }
  | { type: "interaction_decision_rejected"; session: SessionRef; payload: InteractionDecisionRejectedPayload }
  | { type: "tool_session_event"; session: SessionRef; payload: ToolSessionEventPayload }
  | { type: "tool_session_delta"; session: SessionRef; payload: ToolSessionDeltaPayload }
  | { type: string; session: SessionRef; payload?: unknown };

// ChatLine is the local, UI-only timeline model ChatView renders — one entry
// per user turn, agent reply, informational/error note, or plan snapshot.
// Never sent to the server; built entirely from optimistic sends + incoming
// ChatFrames.
export type ChatLineRole =
  | "user"
  | "agent"
  | "system"
  | "error"
  | "plan"
  | "interaction"
  | "notice"
  | "opening"
  | "toolSession";

export interface ChatLine {
  operationId?: string;
  id: string;
  role: ChatLineRole;
  text: string;
  // plan is set only for role === "plan": the latest plan snapshot rendered as
  // a checklist. Consecutive plan_update frames update the same line in place.
  plan?: PlanUpdateInner;
  // toolSessionRef is set only for role === "toolSession": the ToolCallRef whose
  // live transcript this line positions. The transcript itself lives in
  // ChatView state (see toolSession.ts), so an output chunk updates the block
  // without rewriting the timeline.
  toolSessionRef?: string;
  // interactionRequest/interactionApplied are set only for role ===
  // "interaction": an InteractionCard rendered inline in the timeline,
  // positioned at first appearance and keyed by a stable id
  // `interaction:<requestRef>` — same one-card-per-key, update-in-place
  // pattern as `plan` above. interactionRequest is set once (an
  // interaction_request frame never re-requests the same requestRef);
  // interactionApplied is attached in place when the matching
  // interaction_applied frame arrives, resolving the card without removing
  // it from the timeline (see ChatView's handleFrame).
  interactionRequest?: InteractionRequestInner;
  interactionApplied?: InteractionAppliedInner;
  // notice is set only for role === "notice": a one-way server notice returned
  // on a send's HTTP response (MessageResponse.notice), rendered as a
  // tone-styled card. Unlike `interactionRequest` it is never updated in place
  // — a notice is terminal by construction (nothing decides it).
  notice?: NoticeWire;
  opening?: SessionOpening;
  // queued is set on a role==="user" line sent while the agent was already
  // working the current turn: it POSTed immediately (the P1a backend holds it
  // server-side), but MessageList renders it grayed with a "Queued" label
  // until the turn ends, so the user can see it hasn't been picked up yet.
  // Cleared in place (not removed) when turn_activity reports the turn has
  // ended (see ChatView's handleFrame) — the line un-grays rather than
  // disappearing-and-reappearing.
  queued?: boolean;
  // sendStatus tracks the delivery lifecycle of a role==="user" line, so the
  // send feels instant (optimistic) without hiding a real failure:
  //   pending — the bubble rendered immediately; the POST .../message that
  //             confirms channelsd accepted it is still in flight. Shown faded
  //             + pulsing ("Sending…"). The composer is NOT blocked on this.
  //   sent    — channelsd accepted it. Normal bubble (or "Queued" gray if it
  //             landed mid-turn — see `queued`, which layers on top of sent).
  //   failed  — the POST errored or timed out (sendError holds the reason).
  //             Rendered red, inline on the bubble, with a Retry affordance —
  //             never as a separate bottom line divorced from the message.
  // Undefined for non-user lines and for replayed-transcript user lines (which
  // are, by definition, already delivered).
  sendStatus?: "pending" | "sent" | "failed";
  // sendError is the failure reason shown on a sendStatus==="failed" bubble.
  sendError?: string;
  // requestId is a client-minted idempotency key stamped on a role==="user"
  // line at first send and REUSED verbatim by Retry. channelsd caches the
  // decision per requestId for a short window, so a Retry of a send that
  // actually landed (its reply merely lost the race to the 30s timeout)
  // returns the first delivery's decision instead of double-posting the turn.
  requestId?: string;
  // attachments are artifact download chips rendered beneath an agent reply
  // (from a user_message frame's attachments). Each carries a ready-to-follow
  // same-origin /artifact-download URL.
  attachments?: ChatAttachment[];
  // liveViewUrl, when set, renders a "View live" link (from a live_view_offer
  // frame — the agent's artifact_offer_view). Opens the artifact viewer.
  liveViewUrl?: string;
}

// ChatAttachment is a rendered download chip: a display filename + the
// same-origin download URL the chip links to.
export interface ChatAttachment {
  filename: string;
  mime?: string;
  url: string;
}

// SessionNotice mirrors pkg/channels/sessionnotice.Notice: a user-facing
// statement derived purely from the session's status.
//
// These arrive on the detail payload rather than as channel envelopes because
// channelsd — which publishes them to a channel — skips every client-hosted
// session, and `browser` is one. Without them webchat showed nothing at all
// while the explanation sat in an AgentSession condition.
export interface SessionNotice {
  kind: string;
  // tone is emphasis only: "routine" (a bounded wait) or "degraded" (a state
  // needing the user). A notice whose tone is dropped still reads correctly,
  // because lead says what it is.
  tone?: string;
  lead: string;
  body?: string;
  // nextStep is what the user can DO, absent when there is nothing but wait.
  nextStep?: string;
}
