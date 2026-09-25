package channelevents

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"
)

// Envelope is the wire format for every NATS publish. JSON-encoded; Validate
// accepts Version 1 only.
type Envelope struct {
	// Version is the envelope schema version. Anything but 1 is refused.
	Version int `json:"v"`
	// Kind names the sub-channel and decides what type Payload decodes into.
	Kind Kind `json:"kind"`
	// Session is ADVISORY, and on the bus it is CROSS-CHECKED — never the
	// routing authority on its own. It is publisher-controlled JSON, whereas
	// the NATS subject is the only session identity the per-session runner JWT
	// authorizes (PubAllow "ap.session.<ns>.<own-name>.>" — which covers ".in."
	// as well as ".out."). A consumer that takes an envelope OFF THE BUS MUST
	// cross-check it against the subject it arrived on — AuthorizeInSubject /
	// AuthorizeOutSubject, over ParseInSubject / ParseOutSubject /
	// ParseHistorySubject — and drop any envelope whose Session disagrees;
	// routing off this field instead lets a publisher permitted on one
	// session's subject act on another's. BOTH directions enforce this:
	// pkg/channels/channelsd/outbound/relay.go for ".out.", and
	// internal/cmd/channelsd/main.go's envelopeHandler / respondingHandler for ".in.",
	// with test/e2e's harness applying the same gate so scenarios run against
	// it rather than around it.
	//
	// A WILDCARD subscription is the sharpest case, since the subject is then
	// the only session identity in play. But a CONCRETE per-session
	// subscription needs the check too the moment it passes the decoded
	// envelope onward: the subject pins where the message came from, while
	// this field is whatever its publisher wrote, and a runner holds publish
	// across its whole own subtree. pkg/web/webui/livemirror.WatchOutbound is
	// that shape — it drops a mismatch rather than forwarding a mislabelled
	// event to a viewer.
	//
	// It stays on the wire because envelopes also travel WITHOUT a subject:
	// BuildEnvelope hands them straight to a channelkinds.Sender (channelsd's
	// status watchdog, the relay's own delivery-failure notice). Handlers that
	// receive the decoded Envelope alone get it only AFTER their wrapper has
	// cross-checked it, so by the time one runs this field EQUALS the
	// subject-authorized pair — which is what makes their env.Session routing
	// sound.
	Session SessionRef `json:"session"`
	// PublishedAt is when the publisher stamped the envelope (UTC). Required;
	// a zero value fails Validate.
	PublishedAt time.Time `json:"publishedAt"`
	// Payload is the kind-specific body. Required — `{}` for an empty payload,
	// never absent and never null.
	Payload json.RawMessage `json:"payload"`
	// Seq is the producer-assigned logical order of this envelope within a
	// session's timeline: PackSeq(memTurnIndex, blockIndex). Zero means
	// unordered — every envelope that is not a status envelope.
	Seq uint64 `json:"seq,omitempty"`
	// SessionUID identifies the session INSTANCE, so a fork/recreate (new UID,
	// same name) resets a consumer's Seq ordering baseline. Empty alongside a
	// zero Seq.
	SessionUID string `json:"sessionUID,omitempty"`
	// ResurfaceInterruptRequestID, when non-empty, instructs the Slack render
	// path to append an "Interrupt & Send Now" button wired to this interrupt
	// RequestID. Set only when channelsd re-surfaces an interruptible approval
	// prompt to a user who re-interacted from another device; empty on every
	// normal publish.
	ResurfaceInterruptRequestID string `json:"resurfaceInterruptRequestID,omitempty"`
	// Publisher is the signing identity in provenance publisher form
	// ("session:<ns>/<name>"). Empty on an unsigned envelope. The verifier
	// requires it to name the subject-authenticated session exactly.
	Publisher string `json:"publisher,omitempty"`
	// SigKeyID is the content-addressed ID (keyid.For) of the Ed25519 public
	// key that produced Sig.
	SigKeyID string `json:"sigKeyId,omitempty"`
	// SigEpoch is a random per-process value minted at signer construction.
	// Consumers key replay high-water marks per epoch so a legitimately
	// restarted publisher (idle-reaped runner pods re-hydrate as a new
	// process) never trips the monotonic-seq check.
	SigEpoch string `json:"sigEpoch,omitempty"`
	// SigSeq is the signer's per-process publish counter, 1-based, strictly
	// increasing across every envelope the signer signs.
	SigSeq uint64 `json:"sigSeq,omitempty"`
	// Sig is the std-base64 Ed25519 signature over EnvelopeSigDigest. The
	// digest covers the NATS subject and every field above except Sig itself.
	Sig string `json:"sig,omitempty"`
}

const (
	// SeqBlockStart sorts before a turn's first tool block; SeqBlockEnd after
	// its last. Tool blocks use index i+1 so SeqBlockStart (0) precedes them.
	SeqBlockStart = 0
	SeqBlockEnd   = 0xFFFFF
)

// PackSeq encodes (memTurnIndex, blockIndex) into a monotonic uint64. The
// memory turn index is durable (rebuilt by replay across resume); blockIndex
// is the tool_use's position in the assistant turn. 20 bits of block space.
func PackSeq(memTurnIndex, blockIndex int) uint64 {
	return uint64(memTurnIndex)<<20 | uint64(blockIndex&0xFFFFF)
}

// UnpackTurn extracts the memTurnIndex from a value produced by PackSeq. It is
// the inverse of PackSeq's high bits; the blockIndex low bits are discarded.
func UnpackTurn(seq uint64) int {
	return int(seq >> 20)
}

// PublishOutSeq is PublishOut with a stamped Seq + session UID, for status
// envelopes whose ordering the consumer must reason about.
func PublishOutSeq(publish PublishFunc, ns, name string, k Kind, payload any, seq uint64, uid string) error {
	return (*EnvelopeSigner)(nil).PublishOutSeq(publish, ns, name, k, payload, seq, uid)
}

// PublishOutSeq is the *EnvelopeSigner variant of PublishOutSeq that signs
// the envelope after stamping Seq and SessionUID.
//
// publishMu is held across Sign→marshal→publish; see publishEnvelope's doc
// comment for why (nil receiver stays lock-free).
func (s *EnvelopeSigner) PublishOutSeq(publish PublishFunc, ns, name string, k Kind, payload any, seq uint64, uid string) error {
	if s != nil {
		s.publishMu.Lock()
		defer s.publishMu.Unlock()
	}
	subject := SubjectOut(SubjectPrefix(ns, name), k)
	env, err := BuildEnvelope(ns, name, k, payload)
	if err != nil {
		return err
	}
	env.Seq = seq
	env.SessionUID = uid
	if err := s.Sign(subject, &env); err != nil {
		return fmt.Errorf("sign envelope: %w", err)
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	return publish(subject, data)
}

// SessionRef identifies the AgentSession the envelope concerns.
type SessionRef struct {
	Namespace string `json:"ns"`
	Name      string `json:"name"`
}

// LiveViewOfferPayload is the payload for ap.session.<ns>.<name>.out.live_view_offer.
// The runner gates the offer (renderer supports live-view + channel asset
// capability) and publishes only the artifact identity; channelsd mints the
// signed deep-link and renders the button/link.
type LiveViewOfferPayload struct {
	// ArtifactID is the artifact the offered live view opens; the receiving
	// kind mints the signed deep-link from it.
	ArtifactID string `json:"artifactId"`
	// RendererKind is the channelassets renderer that produced the artifact,
	// so the kind can choose its affordance (button vs plain link).
	RendererKind string `json:"rendererKind"`
}

// SessionViewOfferPayload is the payload for
// ap.session.<ns>.<name>.out.session_view_offer. Published by the runner's
// applyUIResource hook when a tool result carries a UIResource; the channel
// kind's session_view_offer sub-channel sender mints the actual page URL from
// SessionRef — the payload itself carries no link, only the session identity,
// so the anchor is never a bearer capability.
type SessionViewOfferPayload struct {
	// SessionRef is "<ns>/<name>" for the AgentSession being escalated.
	SessionRef string `json:"sessionRef"`
}

// WidgetOfferPayload is the payload for ap.session.<ns>.<name>.out.widget_offer.
// Published by the runner's applyUIResource hook alongside
// SessionViewOfferPayload, once per persisted MCP-UI widget (not soft-muted —
// every widget gets its own offer, unlike the session_view_offer anchor). The
// session-view page uses ArtifactID to fetch and frame the widget's persisted
// content; RendererKind is always "mcpui" today but carried explicitly so a
// future second widget-renderer kind needs no envelope-shape change.
type WidgetOfferPayload struct {
	// ArtifactID is the persisted widget the session-view page fetches and
	// frames. Required — an offer without it points at nothing.
	ArtifactID string `json:"artifactId"`
	// Tool is the tool whose result produced the widget, for labelling only.
	Tool string `json:"tool,omitempty"`
	// RendererKind is the channelassets renderer that persisted it ("mcpui"
	// today). Empty means the consumer assumes the default.
	RendererKind string `json:"rendererKind,omitempty"`
}

// AgentUIOfferPayload is the payload for
// ap.session.<ns>.<name>.out.agent_ui_offer. It carries the session
// identity and nothing else: the channel kind's sender composes the page
// URL at send time from a minter holding webd's live external base URL, so
// the envelope is never a bearer capability and never goes stale when that
// base URL changes.
type AgentUIOfferPayload struct {
	// SessionRef is "<ns>/<name>" for the AgentSession whose UI is offered.
	SessionRef string `json:"sessionRef"`
}

// Validate runs cheap shape checks. Callers should validate before publishing
// to catch programmer errors at the publish boundary (closed enum, etc.).
func (e Envelope) Validate() error {
	if e.Version != 1 {
		return fmt.Errorf("unsupported envelope version %d (want 1)", e.Version)
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("unknown kind %q", e.Kind)
	}
	if e.Session.Namespace == "" {
		return errors.New("session.ns is required")
	}
	if e.Session.Name == "" {
		return errors.New("session.name is required")
	}
	if len(e.Payload) == 0 {
		return errors.New("payload is required (use {} for empty payloads)")
	}
	if e.PublishedAt.IsZero() {
		return errors.New("publishedAt is required")
	}
	return nil
}

// InboundUserMessagePayload is the payload for ap.session.<ns>.<name>.in.user_message.
// Empty by design — memory is the durable record; the publish is a wake-up
// signal only.
type InboundUserMessagePayload struct{}

// OutboundUserMessagePayload is the payload for ap.session.<ns>.<name>.out.user_message.
// channelsd's kind-specific Sender consumes Text to render the reply.
type OutboundUserMessagePayload struct {
	// Text is the agent's reply body; the kind's Sender renders it.
	Text string `json:"text"`
	// Attachments references rendered artifacts that channelsd should
	// deliver alongside Text. Refs only — bytes are fetched at delivery
	// time via the operator's HTTP /artifact/... endpoint and never travel
	// via NATS. Empty means a text-only reply.
	Attachments []AttachmentRef `json:"attachments,omitempty"`
}

// AttachmentRef references an ArtifactRender CR whose status.outputRef
// holds the bytes channelsd will fetch via the operator's HTTP endpoint
// at delivery time. Refs only — bytes never travel via NATS.
type AttachmentRef struct {
	// RenderName is the ArtifactRender CR name in the session's namespace.
	// channelsd does a fresh Get to validate ownership + read
	// status.outputRef before fetching bytes.
	RenderName string `json:"renderName"`

	// ArtifactID is the logical artifact this render belongs to
	// (LabelArtifactID on the CR). A browser channel builds a same-origin
	// download link (/artifact-download?artifactId=…) from it; the download
	// endpoint keys on the artifact for its CheckView gate.
	ArtifactID string `json:"artifactId,omitempty"`

	// MIME, Filename, AltText are echoed from the CR's status as a routing
	// hint for channelsd; the CR is authoritative and re-read at delivery.
	// AltText is empty when the renderer supplied none.
	MIME     string `json:"mime"`
	Filename string `json:"filename"`
	AltText  string `json:"altText,omitempty"`
}

// Validate checks that all AttachmentRef entries have a non-empty RenderName.
// Call before building an envelope to catch programming errors early.
// BuildEnvelope does not call this automatically — wiring is the caller's
// responsibility, consistent with how other payload types in this package work.
func (p OutboundUserMessagePayload) Validate() error {
	for i, a := range p.Attachments {
		if a.RenderName == "" {
			return fmt.Errorf("attachments[%d]: renderName empty", i)
		}
	}
	return nil
}

// NotificationPayload is the payload for ap.session.<ns>.<name>.out.notification.
// KindNotification is one-way runner→channelsd. Used for mid-flight
// status updates; rendered by the kind impl as an ephemeral status
// indicator (Slack: assistant.threads.setStatus) that auto-clears
// when the bot's next user_message message lands.
type NotificationPayload struct {
	// Text is the primary status. Rendered as the thread-top assistant
	// indicator on Slack, which has no documented length cap (~100 chars
	// is safe).
	Text string `json:"text"`

	// Short is an optional ≤50-character variant used for surfaces that
	// reject longer strings (e.g. Slack's loading_messages, which caps
	// each element at < 51 characters). When omitted (or itself too long
	// to fit), the kind impl falls back to truncating Text.
	Short string `json:"short,omitempty"`

	// ExpectedDurationSeconds is an optional hint from the agent that the
	// next operation will take roughly this long. The channel kind passes
	// the value to the silence watchdog, which pushes its warn/timeout
	// deadlines forward so the user doesn't see a "Taking longer than
	// expected…" message during a known-slow op. Zero means "no hint —
	// fall back to the default warn/timeout windows." channelsd caps the
	// honored value at 5 minutes regardless of what the agent declares.
	ExpectedDurationSeconds int `json:"expectedDurationSeconds,omitempty"`

	// Ephemeral marks a one-shot announcement — the runner's l.Notify path
	// ("Picking up N messages you sent.", "<agent> thinking…") — as opposed to
	// a persistent update_status caption (update_status / update_plan, which
	// leave this unset). It lets a surface render the announcement briefly and
	// then fall back to a neutral working indicator instead of pinning the
	// stale text as the status caption for the rest of the turn. The web chat
	// UI honors it (decays to "Working…"); surfaces that don't read it simply
	// render it like any other mid-flight status.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// AssistantStreamDeltaPayload is the channelsd-bound mirror of a
// runner-side llm.StreamEvent. The runner forwards only the high-
// signal events (text_delta, tool_use_start, tool_use_stop, stop).
// Per-character JSON fragments and thinking deltas stay runner-local.
type AssistantStreamDeltaPayload struct {
	EventType string `json:"eventType"`          // "text_delta" | "tool_use_start" | "tool_use_stop" | "stop"
	Text      string `json:"text,omitempty"`     // text_delta
	ToolName  string `json:"toolName,omitempty"` // tool_use_start
	ToolID    string `json:"toolId,omitempty"`   // tool_use_*
	BlockIdx  int    `json:"blockIdx"`           // ordering hint
}

// ToolActivityPayload is the payload for ap.session.<ns>.<name>.out.tool_activity.
// KindToolActivity is one-way runner→channelsd. Published by the runner
// each time it dispatches a sandbox or MCP tool call. The outbound relay
// uses these as automatic watchdog "ticks" so a chatty agent making
// continuous progress never trips the silence alarms even if it forgets
// to call update_status. Not rendered to the user on any surface.
type ToolActivityPayload struct {
	// Tool is the LLM-facing name of the tool being dispatched (e.g.
	// "code_gh", "linear_list_issues"). Diagnostic / log-only — the relay
	// uses only the envelope's session ref to Touch the watchdog.
	Tool string `json:"tool,omitempty"`
}

// TurnActivityPayload is the payload for ap.session.<ns>.<name>.out.turn_activity.
// KindTurnActivity is one-way runner→channelsd. Published at coarse turn
// active⇄paused transitions. The outbound relay routes it to the silence
// watchdog: Active=true re-arms (Touch); Active=false tears the indicator
// down (Forget+Clear) — except Cause=awaiting_leakage_approval, whose notice
// must remain. Not rendered to the user on any surface.
type TurnActivityPayload struct {
	// Active reports the agent is turning. false = paused / yielded.
	Active bool `json:"active"`
	// Cause is one of the channelevents.PauseCause* constants when Active is
	// false; empty when active. Diagnostic + drives the watchdog's leakage
	// exception.
	Cause string `json:"cause,omitempty"`
}

// TurnProgressPayload is the payload for ap.session.<ns>.<name>.out.turn_progress.
// KindTurnProgress is one-way runner→channelsd, render-only. The runner's
// progressReporter publishes a throttled snapshot of the in-flight turn's
// cumulative token usage and elapsed time; channels that opt in render it on
// their status surface. Pure metrics — no caption, no formatting; the spinner
// frame and number formatting are the channel kind's concern.
type TurnProgressPayload struct {
	// InputTokens / OutputTokens are CUMULATIVE for the turn so far, not the
	// delta since the previous tick.
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	// ElapsedSeconds is wall-clock since the turn started.
	ElapsedSeconds int `json:"elapsedSeconds"`
	// Seq is a monotonic per-turn tick counter a channel maps to a spinner
	// frame; it does NOT relate to Envelope.Seq.
	Seq int `json:"seq"`
}

// ToolProgressPayload is the payload for ap.session.<ns>.<name>.out.tool_progress.
// KindToolProgress is one-way runner→channelsd, render-only. The sandbox tool
// publishes a throttled snapshot per running SYNC tool (the LLM is blocked
// awaiting the tool and cannot narrate it). channelsd folds it through the
// silence-watchdog (yield-gated) then renders it: the channel kind decides how
// (Slack: a tool clause appended to the agent's caption; 1 tool → detail,
// N → summary). CallID keys the tool in the channel's in-flight set; Done=true
// removes it. Percent/TailLine/EtaSeconds are Layer-2 enrichment — empty until
// a progress extractor supplies them.
type ToolProgressPayload struct {
	// CallID is the stable per-dispatch id (the tool_use ID) keying this tool
	// in the channel's in-flight set across active ticks and the final Done.
	CallID string `json:"callId"`
	// Name is the LLM-facing tool name being run (e.g. "git clone").
	Name string `json:"name,omitempty"`
	// BudgetSeconds is the resolved per-call execution budget (the ceiling the
	// channel renders elapsed against). Zero = unknown.
	BudgetSeconds int `json:"budgetSeconds,omitempty"`
	// ElapsedSeconds is wall-clock since dispatch, computed by the runner.
	ElapsedSeconds int `json:"elapsedSeconds"`
	// Done=true means the tool finished; the channel removes it from its set.
	Done bool `json:"done,omitempty"`

	// Layer-2 enrichment (all optional; empty until a progress extractor runs):
	Percent    *int32 `json:"percent,omitempty"`    // 0..100
	TailLine   string `json:"tailLine,omitempty"`   // last meaningful console line
	EtaSeconds *int32 `json:"etaSeconds,omitempty"` // derived estimate
}

// OperationActivityPayload is the payload for ap.session.<ns>.<name>.out.operation_activity.
// KindOperationActivity is one-way runner→channelsd, render-only. The runner
// emits a throttled snapshot of the ACTIVE operation subtree (operations that
// have been running past the activation gate, plus their active + recent tool
// calls) so surfaces can show live progress. update_status/plan captions are
// separate and unaffected.
type OperationActivityPayload struct {
	// Operations are the active branch(es), each with active + recent calls.
	Operations []OperationActivityNode `json:"operations,omitempty"`
	// CompactLine is the runner-derived "op ‣ reason" single line (untruncated).
	// The channelsd machine overwrites this in-flight with its resolved
	// EffectiveLine (which reverts to the remembered caption on a Cleared tick);
	// senders that show one line render THIS field. Empty on a bare Cleared tick
	// before the machine fills it.
	CompactLine string `json:"compactLine,omitempty"`
	// Cleared marks the final tick: no operation is active. Web chat tears down
	// the tree; the machine reverts the compact line to the remembered caption.
	Cleared bool `json:"cleared,omitempty"`
}

// OperationActivityNode is one operation in the reported subtree.
type OperationActivityNode struct {
	// ID is the operation's stable id across ticks; Description is the
	// agent's one-line statement of what it is doing.
	ID          string `json:"id"`
	Description string `json:"description"`
	// ParentOpID nests this under another node in the same tick. Empty means
	// a root operation.
	ParentOpID string `json:"parentOpId,omitempty"`
	// PlanItem is the plan row this operation is working, when it claimed
	// one; nil means the operation is not tied to a plan item.
	PlanItem *PlanItemRef `json:"planItem,omitempty"`
	// ElapsedSeconds is wall-clock since the operation started.
	ElapsedSeconds int `json:"elapsedSeconds"`
	// Active distinguishes a running operation from a recently-finished one
	// still carried for context.
	Active bool `json:"active"`
	// Calls are this operation's active + recent tool calls, oldest first.
	Calls []OperationActivityCallNode `json:"calls,omitempty"`
}

// OperationActivityCallNode is one tool call under an OperationActivityNode.
type OperationActivityCallNode struct {
	// Tool is the LLM-facing tool name being run.
	Tool           string `json:"tool"`
	Reason         string `json:"reason"` // the agent's per-call _reason
	ElapsedSeconds int    `json:"elapsedSeconds"`
	// Active distinguishes a running call from a recently-finished one.
	Active bool `json:"active"`
}

// ToolSessionDeltaPayload is one chunk of an interactive tool's output
// stream, published by the runner's gateway bridge to channelsd as
// KindToolSessionDelta. Data is the raw bytes the tool emitted on Stream
// — for a headless event-mode tool (e.g. Claude Code in stream-json mode)
// this is one or more newline-delimited JSON events; the channel kind
// decides how to render them.
type ToolSessionDeltaPayload struct {
	ToolCallRef string `json:"toolCallRef"`          // ToolCall CR name; correlation key
	Stream      string `json:"stream"`               // "stdout" | "stderr"
	Data        []byte `json:"data,omitempty"`       // raw chunk; empty when Terminal
	Terminal    bool   `json:"terminal,omitempty"`   // true on the final delta (exit)
	ExitReason  string `json:"exitReason,omitempty"` // set when Terminal: completed | idle | maxDuration | failed
	ExitCode    int32  `json:"exitCode,omitempty"`   // set when Terminal
}

// ToolSessionInputPayload carries human input from a channel into a live
// interactive tool's stdin, published by channelsd's pipeline to the
// runner as KindToolSessionInput.
type ToolSessionInputPayload struct {
	// ToolCallRef is the ToolCall CR name — the correlation key naming which
	// live tool receives this input.
	ToolCallRef string `json:"toolCallRef"`
	// Requester is who typed it, as the surface saw them — a CLAIM, gated on
	// arrival, never an authorization input on its own.
	Requester ExternalIdentity `json:"requester"`
	Data      []byte           `json:"data"` // raw bytes to write to the tool's stdin
}

// ToolSessionEventPayload is one parsed event from an interactive
// tool's stdout, published by the runner as KindToolSessionEvent.
// See pkg/tools/toolkitstream for the Event type's field semantics.
// The wire format is a flat struct (not a wrapped union) so JSON
// validators can introspect known fields without parsing twice.
type ToolSessionEventPayload struct {
	ToolCallRef string `json:"toolCallRef"` // correlation; matches ToolSessionDelta
	EventType   string `json:"eventType"`   // text_delta | tool_use_start | tool_use_stop | result
	Text        string `json:"text,omitempty"`
	ToolName    string `json:"toolName,omitempty"`
	ToolID      string `json:"toolId,omitempty"`
	// OuterTool is the agent-facing tool that produced this stream
	// (e.g. "claude") — the dispatched tool, set by the runner. It is
	// distinct from ToolName, which on tool_use events carries a
	// streaming agent's own internal tool (Write, Read, …).
	OuterTool string `json:"outerTool,omitempty"`
	// Reason is the agent's `_reason` for the tool call that produced
	// this stream — the dispatching call's stated intent. Carried on
	// every event so the Slack renderer's header is stateless.
	Reason string `json:"reason,omitempty"`
	// Summary, OK, DurationMs and CostUSD are the result event's fields and
	// are set only on EventType "result"; zero on every streaming event.
	Summary    string  `json:"summary,omitempty"`
	OK         bool    `json:"ok,omitempty"`
	DurationMs int64   `json:"durationMs,omitempty"`
	CostUSD    float64 `json:"costUsd,omitempty"`
}

// UIActionRequest is the browser→runner invoke for KindUIAction. Requester
// is the cookie-verified viewer webd resolved; the runner re-authorizes it
// independently and fully-consistently rather than trusting it.
type UIActionRequest struct {
	// RequestID is webd's correlation key; it keys the response, every later
	// UIActionUpdatePayload, and the durable ui_action record.
	RequestID string `json:"requestId"`
	Action    string `json:"action"`   // the DECLARED action name
	ToolName  string `json:"toolName"` // read from the server-side declaration, never the browser
	// Args is the action's argument payload, passed through opaque. Empty for
	// an action that takes none.
	Args json.RawMessage `json:"args,omitempty"`
	// Requester is the canonical viewer subject ("user:<...>"). A claim webd
	// verified and the runner re-checks; never trusted as-is.
	Requester string `json:"requester"`
}

// UIActionResponse is the SYNCHRONOUS reply to a KindUIAction request. For a
// readonly auto-run action State is already terminal; for a side-effecting
// one it is submitted and the real outcome arrives later, over
// KindUIActionUpdate and out of the ui_action memory record.
type UIActionResponse struct {
	// RequestID echoes the request's key so the browser can match the reply.
	RequestID string `json:"requestId"`
	// State is the action's lifecycle state at reply time — already terminal
	// for a readonly auto-run, merely submitted for a side-effecting one.
	State   string `json:"state"`
	Message string `json:"message,omitempty"` // browser-safe human copy only
}

// UIActionUpdatePayload is the body of a KindUIActionUpdate envelope: one
// lifecycle transition on the out subject.
//
// It deliberately carries NO tool result. A side-effecting action's value to
// the UI is the data it changed, which the page re-reads through its ordinary
// data bindings once the action settles — so shipping result bytes over NATS
// would be a second, unmetered data path for no gain. Requester is present so
// webd can filter to the connected viewer; webd strips it before anything
// reaches a browser.
type UIActionUpdatePayload struct {
	// RequestID and Action identify which invocation transitioned; RequestID
	// also keys the durable ui_action record a reconnecting browser re-reads.
	RequestID string `json:"requestId"`
	Action    string `json:"action"`
	// Requester is the canonical subject that invoked it, so webd can filter
	// to the connected viewer. webd STRIPS it before anything reaches a
	// browser.
	Requester string `json:"requester"`
	// State is the lifecycle state now reached.
	State string `json:"state"`
	// Message is human copy about this transition; empty when there is none.
	Message string `json:"message,omitempty"`
	// ApprovalAddressedToViewer reports that the approval this action is
	// blocked on is addressed to THIS viewer, so the page can prompt them
	// rather than just say "waiting".
	ApprovalAddressedToViewer bool `json:"approvalAddressedToViewer,omitempty"`
	// UpdatedAt is when the transition happened.
	UpdatedAt time.Time `json:"updatedAt"`
}

// UIViewUpdatePayload is the body of a KindUIViewUpdate envelope: a trigger,
// not a document. webd's live route reacts to it by re-running resolveView
// and pushing what IT resolved — the identical "memory is the source of
// truth; the envelope is the push" discipline UIActionUpdatePayload's own
// doc comment documents, and here it is also a security property: a browser
// can only ever receive a declaration webd itself validated under the
// current CR and grant. Hook is present for logging and the browser's own
// diagnostics only — never a patch key, since the push carries no content
// to patch with.
type UIViewUpdatePayload struct {
	// Hook names the rewritten hook, for logging and browser diagnostics
	// ONLY — never a patch key, since this envelope carries no content.
	Hook string `json:"hook"`
	// UpdatedAt is when the agent rewrote the hook.
	UpdatedAt time.Time `json:"updatedAt"`
}

// SubjectPrefix returns "ap.session.<ns>.<name>" — the shared prefix for
// every subject concerning one AgentSession. Kind-typed façade over
// subjects.Session, which owns the grammar.
func SubjectPrefix(ns, name string) string {
	return subjects.Session(ns, name).String()
}

// AnySessionPrefix is SubjectPrefix with NATS single-token wildcards in place
// of the namespace and name: the prefix a CLUSTER-WIDE subscriber builds its
// subscription from ("ap.session.*.*"). Composing it with SubjectIn/SubjectOut
// keeps a subscriber's pattern and its publisher's subject derived from the one
// grammar, so a subscription can never silently stop matching because the two
// were hand-written in different files.
//
// A subscriber that uses this MUST re-derive the session from each message's
// own subject (ParseInSubject / ParseOutSubject) — the wildcard is why the
// subject is the only authorized identity in the first place.
func AnySessionPrefix() string { return subjects.AnySessionPrefix }

// SubjectIn returns "<prefix>.in.<kind>" — the subject channelsd publishes
// inbound on and the runner subscribes to.
func SubjectIn(prefix string, k Kind) string {
	return subjects.PrefixOf(prefix).In(string(k))
}

// SubjectOut returns "<prefix>.out.<kind>" — the subject the runner publishes
// outbound on and channelsd subscribes to.
func SubjectOut(prefix string, k Kind) string {
	return subjects.PrefixOf(prefix).Out(string(k))
}

// ParseOutSubject extracts the namespace and session name from an outbound
// subject ("ap.session.<ns>.<name>.out.<kind>") — the inverse of
// SubjectOut(SubjectPrefix(ns, name), k). It exists so a consumer of the
// cluster-wide "ap.session.*.*.out.>" subscription can route on the identity
// NATS actually authorized (the subject a publisher's per-session JWT let it
// publish on) rather than on Envelope.Session, which is publisher-controlled
// JSON. Same shape as ParseHistorySubject (pkg/channels/channelevents/history.go),
// which does this for the history request subject.
//
// Kubernetes namespaces and names are DNS labels, so neither can contain a
// dot — but a Kind CAN (KindAssistantStreamDelta is "assistant.stream.delta"),
// and the "out.>" wildcard matches one-or-more trailing tokens. Hence six-or-
// MORE tokens, unlike ParseHistorySubject's fixed six; requiring exactly six
// here would reject every stream-delta subject.
func ParseOutSubject(subject string) (ns, name string, ok bool) {
	ns, name, _, ok = ParseOutSubjectKind(subject)
	return ns, name, ok
}

// ParseOutSubjectKind is ParseOutSubject plus the Kind the subject addresses —
// the trailing token(s) after ".out.", rejoined, so the one dotted Kind
// (KindAssistantStreamDelta) round-trips.
//
// It exists so a consumer of the cluster-wide "ap.session.*.*.out.>"
// subscription can decide what to do with a message BEFORE unmarshalling its
// body: not every out-subject carries an Envelope (the metaagent family is raw
// JSON with its own dedicated subscriber), and decoding one as an Envelope
// produces a spurious "malformed envelope" drop for perfectly valid traffic.
// See Kind.RelayHandles.
//
// The returned Kind is NOT validated — an unregistered leaf comes back as a
// Kind whose Valid() is false, which is the caller's answer, not an error here.
func ParseOutSubjectKind(subject string) (ns, name string, k Kind, ok bool) {
	ns, name, leaf, ok := subjects.ParseOut(subject)
	return ns, name, Kind(leaf), ok
}

// ParseInSubject is ParseOutSubject's inbound twin: it extracts the namespace
// and session name from "ap.session.<ns>.<name>.in.<kind>", the inverse of
// SubjectIn(SubjectPrefix(ns, name), k).
//
// It exists for the same reason, and it matters MORE on this side. A runner's
// per-session grant is PubAllow "ap.session.<ns>.<own-name>.>", which covers
// ".in." as well as ".out." — so a publisher authorized on ONE session's
// inbound subject that is trusted on Envelope.Session instead could drive
// channelsd's inbound handlers against a DIFFERENT session: resolve a parked
// interaction decision, park or clear a PendingInteractions entry, deliver a
// view message, trigger a resurface. channelsd's envelopeHandler /
// respondingHandler (internal/cmd/channelsd/main.go) are the enforcing choke point.
//
// No inbound Kind is dotted today — KindAssistantStreamDelta
// ("assistant.stream.delta") is the only dotted Kind and travels outbound
// only — but this accepts six-or-MORE tokens anyway, exactly as its twin does.
// Two parsers that differ only in a segment name must not also differ in
// strictness: the day an inbound kind acquires a dot, a len==6 check here
// would silently drop every one of its envelopes.
func ParseInSubject(subject string) (ns, name string, ok bool) {
	ns, name, _, ok = ParseInSubjectKind(subject)
	return ns, name, ok
}

// ParseInSubjectKind is ParseInSubject plus the Kind the subject addresses, the
// inbound twin of ParseOutSubjectKind. Its callers are the handlers whose
// subscription is a single exact subject rather than a wildcard: reading the
// kind back off the subject is what turns "safe because the subscription
// happens to pin the leaf" into a property of the handler itself.
func ParseInSubjectKind(subject string) (ns, name string, k Kind, ok bool) {
	ns, name, leaf, ok := subjects.ParseIn(subject)
	return ns, name, Kind(leaf), ok
}

// PublishFunc is the NATS-publish callback used by Publish{In,Out}. The
// runner and channelsd both define a PublishFunc that wraps their nats.Conn
// (with retry, ctx awareness, etc.); this lets the helper here stay
// transport-agnostic and work for both processes.
type PublishFunc func(subject string, data []byte) error

// PublishOut marshals payload, builds an Envelope of kind k for session
// ns/name, validates it, marshals the envelope, and publishes it on
// SubjectOut(SubjectPrefix(ns, name), k).
//
// PublishedAt is stamped to time.Now().UTC() inside the helper —
// callers do not need to thread time.
func PublishOut(publish PublishFunc, ns, name string, k Kind, payload any) error {
	return (*EnvelopeSigner)(nil).PublishOut(publish, ns, name, k, payload)
}

// PublishOut is the *EnvelopeSigner variant that signs the envelope before
// marshalling. A nil signer is a no-op, making this delegable from the
// package-level PublishOut function.
func (s *EnvelopeSigner) PublishOut(publish PublishFunc, ns, name string, k Kind, payload any) error {
	subject := SubjectOut(SubjectPrefix(ns, name), k)
	return s.publishEnvelope(publish, subject, ns, name, k, payload)
}

// PublishIn is the inbound twin of PublishOut. Used by channelsd's
// pipeline to publish user-message envelopes destined for the runner.
func PublishIn(publish PublishFunc, ns, name string, k Kind, payload any) error {
	return (*EnvelopeSigner)(nil).PublishIn(publish, ns, name, k, payload)
}

// PublishIn is the *EnvelopeSigner variant that signs the envelope before
// marshalling. A nil signer is a no-op, making this delegable from the
// package-level PublishIn function.
func (s *EnvelopeSigner) PublishIn(publish PublishFunc, ns, name string, k Kind, payload any) error {
	subject := SubjectIn(SubjectPrefix(ns, name), k)
	return s.publishEnvelope(publish, subject, ns, name, k, payload)
}

// publishEnvelope builds, signs, marshals, and publishes an envelope. Called
// by both PublishOut and PublishIn signer variants.
//
// publishMu is held across the whole Sign→marshal→publish sequence (nil
// receiver stays lock-free, matching the nil-signer no-op contract every
// method on *EnvelopeSigner honors) so two goroutines publishing through the
// same signer can never land on the wire in an order that disagrees with
// their assigned SigSeq — see EnvelopeSigner.publishMu's doc comment.
func (s *EnvelopeSigner) publishEnvelope(publish PublishFunc, subject, ns, name string, k Kind, payload any) error {
	if s != nil {
		s.publishMu.Lock()
		defer s.publishMu.Unlock()
	}
	if !k.Implemented() {
		return fmt.Errorf("publish of reserved (unimplemented) kind %q refused", k)
	}
	env, err := BuildEnvelope(ns, name, k, payload)
	if err != nil {
		return err
	}
	if err := s.Sign(subject, &env); err != nil {
		return fmt.Errorf("sign envelope: %w", err)
	}
	envBytes, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	return publish(subject, envBytes)
}

// RequestFunc is a NATS request-reply transport. Kept as a func type (like
// PublishFunc) so callers need not depend on the NATS client.
//
// nats.Conn.Request does NOT satisfy this signature directly — it returns
// (*nats.Msg, error), not ([]byte, error) — so every wiring site adapts it with
// a thin closure that unwraps msg.Data:
//
//	deps.NATSRequest = func(subj string, data []byte, timeout time.Duration) ([]byte, error) {
//		msg, err := nc.Request(subj, data, timeout)
//		if err != nil {
//			return nil, err
//		}
//		return msg.Data, nil
//	}
type RequestFunc func(subject string, data []byte, timeout time.Duration) ([]byte, error)

// RequestIn publishes an envelope on the session's IN subject and waits for a
// reply, returning the raw reply bytes.
//
// Used by kinds whose "listener" is a browser or TUI rather than a socket:
// they have no memory-write credential of their own, so channelsd performs the
// inbound (memory append + wake) and replies with the decision. Fire-and-forget
// would drop that decision on the floor — the caller's UI renders it.
//
// Unlike PublishIn (whose kind check reaches Envelope.Validate → Kind.Valid),
// RequestIn requires Kind.Implemented: a reserved-but-unimplemented kind is
// Valid, and requesting one would block until timeout with no responder.
func RequestIn(req RequestFunc, ns, name string, k Kind, payload any, timeout time.Duration) ([]byte, error) {
	return (*EnvelopeSigner)(nil).RequestIn(req, ns, name, k, payload, timeout)
}

// RequestIn is the *EnvelopeSigner variant that signs the envelope before
// sending. A nil signer is a no-op, making this delegable from the
// package-level RequestIn function.
//
// publishMu is held across Sign→marshal→request; see publishEnvelope's doc
// comment for why (nil receiver stays lock-free).
func (s *EnvelopeSigner) RequestIn(req RequestFunc, ns, name string, k Kind, payload any, timeout time.Duration) ([]byte, error) {
	if s != nil {
		s.publishMu.Lock()
		defer s.publishMu.Unlock()
	}
	if req == nil {
		return nil, fmt.Errorf("channelevents: RequestIn on %q: nil requester (wiring bug)", k)
	}
	if !k.Implemented() {
		return nil, fmt.Errorf("channelevents: RequestIn on unimplemented kind %q", k)
	}
	subject := SubjectIn(SubjectPrefix(ns, name), k)
	env, err := BuildEnvelope(ns, name, k, payload)
	if err != nil {
		return nil, fmt.Errorf("channelevents: RequestIn build envelope: %w", err)
	}
	if err := s.Sign(subject, &env); err != nil {
		return nil, fmt.Errorf("channelevents: RequestIn sign envelope: %w", err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("channelevents: RequestIn marshal envelope: %w", err)
	}
	reply, err := req(subject, b, timeout)
	if err != nil {
		return nil, fmt.Errorf("channelevents: RequestIn %q: %w", k, err)
	}
	return reply, nil
}

// BuildEnvelope marshals payload and stitches it into a validated
// Envelope of kind k for session ns/name. Used by callers that hand the
// envelope to a non-NATS sender (e.g. internal/cmd/channelsd's statusWatchdog
// passes envelopes directly to channelkinds.Sender.Send) — i.e. anyone
// who needs the envelope object but isn't going through NATS.
//
// PublishedAt is stamped to time.Now().UTC().
func BuildEnvelope(ns, name string, k Kind, payload any) (Envelope, error) {
	pl, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal payload: %w", err)
	}
	env := Envelope{
		Version:     1,
		Kind:        k,
		Session:     SessionRef{Namespace: ns, Name: name},
		PublishedAt: time.Now().UTC(),
		Payload:     pl,
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, fmt.Errorf("envelope: %w", err)
	}
	return env, nil
}
