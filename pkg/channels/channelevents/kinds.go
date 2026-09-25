// pkg/channels/channelevents/kinds.go
//
// Sub-channel kind taxonomy. Closed enum: new kinds require a code change.
// The taxonomy is a security-boundary doc artifact — the publish-side
// validation rejects unknown kinds.
package channelevents

import "github.com/authzed/openagentprimitives/pkg/platform/identity"

// Kind is a sub-channel kind name. See the spec for the publishing
// discipline (who's allowed to publish each kind).
type Kind string

const (
	KindUserMessage Kind = "user_message" // implemented — IN: content-free wake-up; OUT: the agent's reply
	// KindPermissionRequest is the multiplayer session-join prompt. Its
	// payload type and the local/browser/fake sub-channel senders are live,
	// but NOTHING publishes it today: the join flow rides the generic
	// interaction model under category "permission_request".
	KindPermissionRequest Kind = "permission_request"
	KindPermissionGrant   Kind = "permission_grant" // RESERVED — operator→runner
	KindNotification      Kind = "notification"     // implemented — runner→channelsd, one-way (status updates, etc.)
	KindToolActivity      Kind = "tool_activity"    // implemented — runner→channelsd, one-way (silent watchdog tick on each tool dispatch)

	// KindPermissionDecision is published by the channel kind when the
	// approver clicks approve/deny. Carries the kind-defined opaque
	// RequestRef so the decision_applied callback can find the kind's
	// user-facing artifacts.
	KindPermissionDecision Kind = "permission_decision"

	// KindPermissionDecisionApplied is emitted by the pipeline after
	// it has acted on a decision (written SpiceDB, cleared
	// PendingRequesters). The channel kind uses it to update its
	// user-facing artifacts (slack: edit rejection + DM).
	KindPermissionDecisionApplied Kind = "permission_decision_applied"

	// KindAssistantStreamDelta carries a single high-signal LLM streaming
	// event from the runner to channelsd. See AssistantStreamDeltaPayload.
	KindAssistantStreamDelta Kind = "assistant.stream.delta"

	// KindPlanUpdate carries a structured agent plan / checklist update.
	// Runner→channelsd, one-way. Channels render it their own way (Slack:
	// edited Block Kit message; the oap chat TUI: a plan overlay). See
	// PlanUpdatePayload.
	KindPlanUpdate Kind = "plan_update"

	KindToolSessionDelta Kind = "tool_session_delta" // implemented — interactive tool's bridge → channelsd, one-way streamed output
	KindToolSessionInput Kind = "tool_session_input" // implemented — channelsd → runner, human input into a live interactive tool

	// KindToolSessionEvent carries one neutral, parsed event from an
	// interactive tool whose toolkit declares a streamFormat
	// (pkg/tools/toolkitstream). When this kind is emitted, the runner is
	// NOT also emitting KindToolSessionDelta for the same stdout
	// bytes (parsed and raw are mutually exclusive on stdout).
	// Stderr stays on KindToolSessionDelta regardless.
	KindToolSessionEvent Kind = "tool_session_event"

	// KindLiveViewOffer is published by the runner's artifact_offer_view
	// tool. channelsd renders it as an interactive button (or link) that
	// opens the artifact live-view. runner→channelsd, one-way.
	KindLiveViewOffer Kind = "live_view_offer"

	// KindTurnActivity is one-way runner→channelsd. Published by the runner
	// at coarse active⇄paused transitions (turn-start, yield-to-reply,
	// approval block/resume, awaiting-retry, failure). channelsd routes it to
	// the silence watchdog: active re-arms, paused tears the indicator down.
	// Planless — does not depend on the session having a plan. See
	// TurnActivityPayload.
	KindTurnActivity Kind = "turn_activity"

	// KindTurnProgress is one-way runner→channelsd, render-only. The runner's
	// progress reporter publishes a throttled snapshot of the in-flight turn's
	// cumulative token usage + elapsed time; channels that opt in render it on
	// their status surface (Slack: spinner frame + counts on the status line).
	// Pure metrics — no caption, no formatting. See TurnProgressPayload.
	KindTurnProgress Kind = "turn_progress"

	// KindToolProgress is one-way runner→channelsd, render-only. The sandbox
	// tool publishes a throttled per-tool snapshot while a SYNC tool executes
	// (elapsed, resolved budget, and optional Layer-2 progress). channels that
	// opt in render it on their status surface (Slack: a tool clause appended
	// to the agent's caption). Done=true removes the tool from the in-flight
	// set. See ToolProgressPayload.
	KindToolProgress Kind = "tool_progress"

	// KindOperationActivity is one-way runner→channelsd, render-only. The
	// runner emits a throttled snapshot of the ACTIVE operation subtree
	// (operations that have been running past the activation gate, plus
	// their active + recent tool calls) so surfaces can show live progress.
	// update_status/plan captions are separate and unaffected. See
	// OperationActivityPayload.
	KindOperationActivity Kind = "operation_activity"

	// KindIdentityChoiceRequest is published runner→channelsd when a session
	// under identity mode "ask" or "dynamic" needs the requester to pick
	// which identity drives the next action. Rendered as a 3-button
	// ephemeral to Requester. See IdentityChoiceRequestPayload.
	KindIdentityChoiceRequest Kind = "identity_choice_request"

	// KindIdentityChoiceApplied is published channelsd→runner carrying the
	// resolved 3-way answer (agent | userPassthrough | cancel) via Action.
	// See IdentityChoiceAppliedPayload.
	KindIdentityChoiceApplied Kind = "identity_choice_applied"

	// KindIdentityChoiceDecision is published by the channel kind's listener
	// on button click, carrying the clicker's identity. Mirrors
	// KindPermissionDecision. See IdentityChoiceDecisionPayload.
	KindIdentityChoiceDecision Kind = "identity_choice_decision"
	// KindThreadTitle carries an agent-chosen conversation title (+ optional
	// emoji glyph). Runner→channelsd, one-way. Slack renders it as a native
	// assistant.threads.setTitle in DMs, or an in-place edit of the "started"
	// bootstrap message in bot-rooted channel threads. See ThreadTitlePayload.
	KindThreadTitle Kind = "thread_title"

	// KindViewMessage is a user message injected by a browser/TUI *view* of a
	// session rather than by the Channel's own listener. It is one of two
	// inbound kinds that carry message content directly rather than relying on
	// `.in.user_message`'s content-free wake-up shape (KindAgentMessageSend is the
	// other): `.in.user_message` is a content-free wake-up ping because memory
	// is the durable record, but a view has no listener behind it and no way to
	// write memory itself (webd's memory token is read-only by design — see
	// pkg/memory/tokens/tokens.go).
	//
	// Request/reply: channelsd replies with a ViewMessageResultPayload so the
	// caller keeps the synchronous InboundDecision that its UI renders.
	KindViewMessage Kind = "view_message"

	// KindInterruptRequest is published channel→runner when a user
	// requests a mid-turn interrupt of the agent's in-flight work. See
	// InterruptRequestPayload.
	KindInterruptRequest Kind = "interrupt_request"

	// KindInterruptApplied is published runner→channel carrying the
	// outcome of an interrupt request ("interrupted" | "rejected"). See
	// InterruptAppliedPayload.
	KindInterruptApplied Kind = "interrupt_applied"

	// KindEnqueueAck is published channelsd→channel when an inbound
	// message arrives mid-turn and is queued rather than interrupting the
	// agent's in-flight work. Rendered as a proactive ephemeral ack
	// ("your message is queued while I'm working") on the queued_messages
	// sub-channel, alongside KindInterruptApplied. See EnqueueAckPayload.
	KindEnqueueAck Kind = "enqueue_ack"

	// KindUserEcho mirrors a view-originated user message back to the session's
	// originating channel (Slack). Published OUT by channelsd after a
	// view_message routes, so participants in the origin channel see the
	// browser/TUI-typed message ("💬 @alice, via the artifact view of …: …")
	// instead of the agent apparently answering a question nobody asked. The
	// origin kind's user_echo sub-channel sender renders it; kinds that ARE the
	// view surface (builtin/local) don't implement it and the relay degrades.
	KindUserEcho Kind = "user_echo"

	// KindSessionViewOffer is published runner→channelsd when the runner
	// auto-escalates an MCP tool result carrying an interactive ui:// widget
	// (tool.Result.UIResource, set by MCP dispatch). It carries the session
	// identity pointing at the browser session-view page, not the widget
	// content; the channel kind's session_view_offer sub-channel sender mints
	// the page URL and renders it (button or link, per kind). Deduped by the
	// runner per escalation window, so repeated widgets in one session don't
	// spam the anchor. See SessionViewOfferPayload.
	KindSessionViewOffer Kind = "session_view_offer"

	// KindWidgetOffer is published runner→channelsd by applyUIResource
	// alongside KindSessionViewOffer, once per persisted MCP-UI widget
	// (never soft-muted the way the session_view_offer anchor is — a user
	// already on the session-view page should see every widget live). It
	// carries the durable artifact identity the widget was persisted under
	// (the identity "mcpui" channelassets renderer); the session-view page
	// fetches and frames the widget content by ArtifactID. See
	// WidgetOfferPayload.
	KindWidgetOffer Kind = "widget_offer"

	// KindAgentUIOffer is published runner→channelsd by the agent's
	// show_agent_ui meta tool: the agent has decided to hand the user a way
	// into the agent-defined UI for this session. The channel kind's
	// agent_ui_offer sub-channel sender composes the page URL from SessionRef
	// and renders it (Slack: a Block Kit URL button; the TUI: a timeline link
	// line).
	//
	// Distinct from KindSessionViewOffer, which the runner publishes
	// AUTOMATICALLY when a tool result carries an mcp-ui widget, and which
	// points at a different page. The payload shapes are identical, which is
	// exactly why the two kinds must not be conflated: a mis-published kind
	// decodes cleanly on the far side and lands the user on the wrong page.
	// See AgentUIOfferPayload.
	KindAgentUIOffer Kind = "agent_ui_offer"

	// KindResurfaceRequest is published surface→channelsd when a view of a
	// session attaches (a chat tab opens its websocket, the TUI's outbound
	// relay subscribes). It asks channelsd to re-surface whatever prompt the
	// session is currently parked on. Delivery of a parked prompt is otherwise
	// a one-shot live publish to whoever is subscribed at that instant, and
	// the per-prompt dedup (credential_link's CredentialRequestPublished
	// condition) guarantees it is never re-sent — so without this request a
	// surface attaching afterwards shows a durably-parked session with no
	// visible reason. The only other trigger for the same machinery is an
	// inbound user message, which the waiting user has no reason to send.
	// See ResurfaceRequestPayload.
	KindResurfaceRequest Kind = "resurface_request"

	// The metaagent family. Four subjects carrying the dynamic-scope
	// conversation between the runner, the channel, authzd and the operator.
	// See metaagent.go for the payload shapes, the tolerant decode helpers, and
	// why these four are still published BARE rather than wrapped.
	//
	// KindMetaagentRequest is published channel→authzd (a metaagent @mention)
	// and runner→authzd (the cold-start scope review) on IN. It asks authzd to
	// run the staged scope lifecycle for the subject's session.
	KindMetaagentRequest Kind = "metaagent_request"

	// KindMetaagentScopeApproval is published authzd→channelsd on OUT when the
	// scope lifecycle needs a human decision. Rendered by the channel kind's
	// metaagent_scope_approval sub-channel sender.
	KindMetaagentScopeApproval Kind = "metaagent_scope_approval"

	// KindMetaagentApprovalApplied is published channel→authzd on IN carrying
	// the human's approve/deny click. authzd gates the clicker on
	// agentsession#manage_scope before routing it.
	KindMetaagentApprovalApplied Kind = "metaagent_approval_applied"

	// KindMetaagentNotice is published authzd→channelsd and operator→channelsd
	// on OUT: a one-line private acknowledgment addressed to a single user.
	// Rendered by the channel kind's metaagent_notice sub-channel sender.
	KindMetaagentNotice Kind = "metaagent_notice"

	// KindAppToolCall is published browser/webd→runner for a synchronous
	// MCP-UI app-visible tool call (an mcp-ui widget's window.postMessage
	// tool invocation, relayed by webd). The runner replies via
	// msg.Respond with an AppToolCallResponse — this is a request/reply
	// kind like KindViewMessage, not one-way. See AppToolCallRequest /
	// AppToolCallResponse.
	KindAppToolCall Kind = "app_tool_call"

	// KindUIDataBinding is a browser→runner request to resolve ONE agent-UI
	// data binding whose source is "tool". It carries an AppToolCallRequest
	// and is answered with an AppToolCallResponse — the wire shape is
	// identical to KindAppToolCall's, and the two are separate kinds rather
	// than a flag on one kind because they have different preconditions: a
	// data binding may only name a readonly tool, and its result takes the
	// browser-sized ingress ceiling rather than the model-sized one. See
	// runner.HandleUIDataBinding.
	KindUIDataBinding Kind = "ui_data_binding"

	// KindUIAction is a browser->runner request to INVOKE one agent-UI action
	// binding. Separate from KindAppToolCall and KindUIDataBinding — three kinds,
	// not one kind with two flags — because the three differ in preconditions and
	// in wire shape: a data binding may only name a readonly tool, an action
	// carries its DECLARED NAME alongside the tool (the record is keyed by what
	// the author called it, not by what it dispatches to), and only an action has
	// an addressable lifecycle. See runner.HandleUIAction.
	KindUIAction Kind = "ui_action"

	// KindUIActionUpdate is the runner→browser push carrying one lifecycle
	// transition, delivered over livemirror. The DURABLE half is the ui_action
	// memory record, which is the source of truth — so a dropped envelope
	// costs latency, never correctness: a reconnecting browser re-reads the
	// record.
	KindUIActionUpdate Kind = "ui_action_update"

	// KindUIViewUpdate is the runner->browser push telling webd that the agent
	// rewrote one agent-UI slot. OUT only, like KindUIActionUpdate.
	//
	// It carries NO declaration. webd re-resolves the merged view from the
	// AgentUI CR and the ui_view_model records and pushes what IT validated, so
	// a fragment can never reach a browser without passing the same
	// uicomponents.Validate every other read passes. The slot name is here for
	// logging and for the browser's own diagnostics, not as a patch key.
	KindUIViewUpdate Kind = "ui_view_update"

	// KindUIPresence is a browser→runner LIVENESS heartbeat: someone is
	// looking at this session's agent-defined UI right now. webd republishes
	// it while a live socket is attached and the viewer's tab reports itself
	// visible AND focused; the runner extends its idle deadline on each one.
	//
	// It exists because a runner's lifetime is shaped by CONVERSATION while an
	// agent-UI's data bindings are answered with no conversation at all.
	// Observed: a runner took two turns in six seconds, idled, and exited
	// after 5m19s, after which every binding on an open dashboard timed out
	// for want of a subscriber.
	//
	// A REPEATED heartbeat, not an attach/detach pair, for failure behaviour
	// rather than traffic: if webd crashes, is evicted, or the network drops,
	// the heartbeats simply stop and the runner idles out on its own. A lease
	// released by an explicit detach strands a runner alive on any lost
	// detach, so it would need an expiry anyway — a heartbeat with extra steps
	// and one more way to leak a pod.
	//
	// One-way, with no reply and no ack: an answered heartbeat would let a
	// watching browser block on the runner, and the next heartbeat carries
	// strictly fresher information than a retry of this one.
	//
	// It is a LIVENESS hint and NEVER an authorization input — it says a
	// viewer is watching, nothing about who they are or what they may do.
	// Every binding call keeps its own per-call viewer-bound interact
	// re-authorization (see KindUIDataBinding).
	KindUIPresence Kind = "ui_presence"
)

// Valid reports whether k is a known kind. Unknown kinds are rejected at
// the publish boundary; new kinds require a code change here.
func (k Kind) Valid() bool {
	switch k {
	case KindUserMessage, KindPermissionRequest, KindPermissionGrant, KindNotification, KindToolActivity,
		KindPermissionDecision, KindPermissionDecisionApplied,
		KindAssistantStreamDelta, KindPlanUpdate,
		KindToolSessionDelta, KindToolSessionInput, KindToolSessionEvent,
		KindCredentialRequest, KindCredentialLinked, KindPortalAccess,
		KindRestartTrigger, KindLiveViewOffer, KindTurnActivity, KindTurnProgress, KindToolProgress,
		KindOperationActivity,
		KindIdentityChoiceRequest, KindIdentityChoiceApplied, KindIdentityChoiceDecision,
		KindThreadTitle, KindViewMessage, KindUserEcho,
		KindInterruptRequest, KindInterruptApplied, KindEnqueueAck,
		KindInteractionRequest, KindInteractionApplied, KindInteractionDecision, KindInteractionDecisionRejected,
		KindRevoked, KindSessionViewOffer, KindWidgetOffer, KindAgentUIOffer, KindAppToolCall, KindResurfaceRequest,
		KindMetaagentRequest, KindMetaagentScopeApproval, KindMetaagentApprovalApplied, KindMetaagentNotice,
		KindUIDataBinding, KindUIAction, KindUIActionUpdate, KindUIViewUpdate, KindUIPresence,
		KindAgentMessageSend:
		return true
	default:
		return false
	}
}

// Implemented reports whether the kind is wired up in this version of the
// codebase. Reserved kinds are Valid but not Implemented, and the gate runs on
// BOTH sides: publishEnvelope (PublishOut / PublishIn) and RequestIn refuse
// them at the source, and the outbound relay drops them with a logged reason
// as a backstop.
//
// HAZARD: the producer-side gate lives on publishEnvelope and RequestIn
// specifically, so any caller that builds an Envelope via BuildEnvelope and
// hands the marshaled bytes to a raw NATS publish is ungated by construction —
// PublishOutSeq, the resurface republish in
// pkg/channels/channelsd/pipeline/resurface.go, and channel-kind listener
// paths such as pkg/channels/channelkinds/slack/listener.go that publish
// button-click follow-ups directly. Those are safe only because every one of
// them emits an Implemented kind.
func (k Kind) Implemented() bool {
	switch k {
	case KindUserMessage, KindNotification, KindToolActivity,
		KindPermissionRequest, KindPermissionDecisionApplied,
		KindPermissionDecision, KindAssistantStreamDelta, KindPlanUpdate,
		KindToolSessionDelta, KindToolSessionInput, KindToolSessionEvent,
		KindCredentialRequest, KindCredentialLinked, KindPortalAccess,
		KindRestartTrigger, KindLiveViewOffer, KindTurnActivity, KindTurnProgress, KindToolProgress,
		KindOperationActivity,
		KindIdentityChoiceRequest, KindIdentityChoiceApplied, KindIdentityChoiceDecision,
		KindThreadTitle, KindViewMessage, KindUserEcho,
		KindInterruptRequest, KindInterruptApplied, KindEnqueueAck,
		KindInteractionRequest, KindInteractionApplied, KindInteractionDecision, KindInteractionDecisionRejected,
		KindRevoked, KindSessionViewOffer, KindWidgetOffer, KindAgentUIOffer, KindAppToolCall, KindResurfaceRequest,
		KindMetaagentRequest, KindMetaagentScopeApproval, KindMetaagentApprovalApplied, KindMetaagentNotice,
		KindUIDataBinding, KindUIAction, KindUIActionUpdate, KindUIViewUpdate, KindUIPresence,
		KindAgentMessageSend:
		return true
	default:
		return false
	}
}

// RelayHandles reports whether channelsd's outbound relay is the component
// that consumes this kind's OUT subject.
//
// The relay's subscription is the cluster-wide "ap.session.*.*.out.>", so it
// receives every out-subject in the cluster whether or not it is the intended
// consumer. For all but the metaagent family it is: it decodes the Envelope and
// either folds it into the silence watchdog or resolves a Sender.
//
// The metaagent family answers false. Those subjects have their own dedicated
// subscription (internal/cmd/channelsd/metaagent_handlers.go), which resolves the kind's
// metaagent_scope_approval / metaagent_notice SUB-channel sender — not the main
// sender the relay's default arm would pick. Letting the relay route them would
// deliver an approval prompt twice, once to the wrong surface; letting it
// merely DECODE them is also wrong, since they are raw JSON and each would log
// "drop, envelope validation failed" for traffic that is working correctly —
// noise sitting exactly where a real drop's line would appear.
//
// The relay applies this to the kind read off the SUBJECT, before touching the
// body, so it holds for both the bare payloads published today and any
// enveloped ones published later. An unrecognized leaf answers true: the relay
// keeps its own drop path, which names the reason, rather than having this
// swallow it.
func (k Kind) RelayHandles() bool { return !k.isMetaagent() }

// ExternalIdentity is the channel-side identity of a user as carried
// in envelope payloads. Channel kinds set whichever fields they have;
// Email is preferred when known. Mirrored from channelkinds.ExternalIdentity
// to keep this package free of runtime-package imports (channelkinds →
// channelevents, not the reverse).
//
// ExternalID is always the raw, channel-native id — never a canonical
// user id. Use Principal() to derive the canonical identity.
type ExternalIdentity struct {
	// Kind is the channel family ("slack", …) that issued ExternalID; the two
	// together are the natural addressable form.
	Kind identity.Kind `json:"kind"`
	// TeamScope disambiguates ExternalID across workspaces/tenants of one
	// Kind. Empty for kinds with a single global id space.
	TeamScope  identity.TeamScope     `json:"teamScope,omitempty"`
	ExternalID identity.RawExternalID `json:"externalId"`
	// Email is the user's address when the channel reported one; empty is
	// common and never an error.
	Email identity.Email `json:"email,omitempty"`
	// Subject, when set, is a pre-formed SpiceDB subject ("user:<id>")
	// that Principal() passes through verbatim via identity.RawSubject,
	// bypassing the kind/teamScope/externalID/email encoding entirely.
	Subject identity.Subject `json:"subject,omitempty"`
}

// Principal derives the identity.Principal for this ExternalIdentity.
// When Subject is set it wins (RawSubject passthrough); otherwise the
// principal is derived from the raw channel-attributed fields via
// identity.FromExternal.
func (e ExternalIdentity) Principal() identity.Principal {
	if e.Subject != "" {
		return identity.RawSubject(e.Subject.String())
	}
	return identity.FromExternal(e.Kind, e.TeamScope, e.ExternalID, e.Email)
}

// FromPrincipal builds an ExternalIdentity from a channel-native Principal
// (Kind/TeamScope/ExternalID/Email). NOTE: it does NOT preserve a RawSubject
// passthrough — a Principal built via identity.RawSubject has no accessor for
// its subject, so FromPrincipal(e.Principal()) is NOT round-trip-safe for a
// Subject-passthrough ExternalIdentity. For those, construct
// ExternalIdentity{Subject: …} directly (the leaked_to / bento cases).
func FromPrincipal(p identity.Principal) ExternalIdentity {
	return ExternalIdentity{Kind: p.Kind(), TeamScope: p.TeamScope(), ExternalID: p.ExternalID(), Email: p.Email()}
}

// HasIdentity reports whether this identity is resolvable — either the natural
// raw form (Kind+ExternalID) or a Subject passthrough. Delivery/resolve gates
// MUST use this rather than `ExternalID == ""`, which rejects a Subject-only
// identity that Principal() would happily resolve.
func (e ExternalIdentity) HasIdentity() bool {
	return (e.Kind != "" && e.ExternalID != "") || e.Subject != ""
}

// PermissionRequestPayload is the body of a KindPermissionRequest envelope.
type PermissionRequestPayload struct {
	AgentSessionRef SessionRef `json:"agentSessionRef"`
	// Requester wants to join the session; StartedBy owns it and decides.
	Requester ExternalIdentity `json:"requester"`
	StartedBy ExternalIdentity `json:"startedBy"`
	// Preview is the requester's own message text, shown so the approver can
	// judge the ask. External content, not publisher copy. Empty when no
	// preview was available.
	Preview string `json:"preview,omitempty"`
	// AgentClassName denormalizes AgentSession.Spec.Class so the
	// channel kind's Sender can show "<agent>'s session" in approval
	// DMs without a separate cluster lookup.
	AgentClassName string `json:"agentClassName,omitempty"`
	// IdentityMode mirrors AgentClass.Spec.IdentityMode of the
	// agent the requester wants to join. Carried through so the
	// channel kind's permission_request sender can render an
	// elevated-risk warning when the session runs under
	// userPassthrough — approving lets the requester drive actions
	// that execute AS THE APPROVER on every service the agent
	// connects to with the approver's credentials. Empty for the
	// default (operator-identity) mode where approval doesn't share
	// the approver's connected accounts.
	IdentityMode string `json:"identityMode,omitempty"`
	// LinkedServices is the human-readable list of provider labels
	// the approver has currently linked AND that the joining session's
	// AgentClass actually uses. Populated only under userPassthrough;
	// empty in other modes. The slack sender renders these in the
	// elevated-risk warning, so the approver sees exactly which services
	// they're about to share: a concrete list ("Linear, GitHub") is what
	// lets a careful approver actually weigh the decision.
	//
	// Computed as the approver's UserIdentity.Spec.Credentials intersected
	// with the AgentClass's referenced MCPServers (never list an account the
	// joining agent doesn't use), each mapped to its ProviderLabel
	// (Spec.Auth.Provider → metadata.name → credential name), deduped and
	// alphabetized for stable rendering.
	//
	// Empty (no overlap, or a lookup failure) means fall back to the generic
	// "your connected accounts" phrasing — degraded UX, not blocking.
	LinkedServices []string `json:"linkedServices,omitempty"`
}

// PermissionDecisionPayload is the body of a KindPermissionDecision envelope.
type PermissionDecisionPayload struct {
	AgentSessionRef SessionRef       `json:"agentSessionRef"`
	Requester       ExternalIdentity `json:"requester"`
	// Approver is who clicked — a CLAIM, re-checked before it grants anything.
	Approver ExternalIdentity `json:"approver"`
	Decision string           `json:"decision"` // "approve" | "deny"
	// RequestRef is the channel-kind-opaque handle for the artifacts that
	// rendered the prompt, so decision_applied can find and edit them.
	RequestRef string `json:"requestRef"`
}

// PermissionDecisionAppliedPayload is the body of a KindPermissionDecisionApplied envelope.
type PermissionDecisionAppliedPayload struct {
	AgentSessionRef SessionRef       `json:"agentSessionRef"`
	Requester       ExternalIdentity `json:"requester"`
	Approver        ExternalIdentity `json:"approver"`
	Decision        string           `json:"decision"`
	// RequestRef round-trips the decision's handle so the kind can edit the
	// artifacts it originally rendered.
	RequestRef string `json:"requestRef"`
	// Reason distinguishes an owner's explicit deny (empty) from a request
	// that lapsed unanswered ("timeout"). The channel kind uses it to render
	// "expired" wording instead of "denied" — nobody rejected the request, it
	// simply timed out. Empty on the normal approve/deny click path.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Resubmitted reports whether the requester's original message
	// was successfully replayed through the inbound pipeline as part
	// of an approve. False when the message wasn't stashed (too
	// large to fit in PendingRequester) or when the replay failed —
	// the channel kind should fall back to "please re-send" wording
	// in that case so the user knows they need to retype.
	Resubmitted bool `json:"resubmitted,omitempty"`
}

// ViewMessagePayload is the body of a KindViewMessage envelope, published on
// ap.session.<ns>.<name>.in.view_message.
//
// Author is a CLAIM. channelsd re-derives the canonical subject from it via
// identity.FromExternal(...).Canonical() with allowSynthetic=false, and
// re-checks agentsession#interact. Never trust it as an authorization input.
type ViewMessagePayload struct {
	// Text is the message body — one of two inbound kinds that carries content
	// directly (KindAgentMessageSend is the other; see its own doc for why).
	Text   string           `json:"text"`
	Author ExternalIdentity `json:"author"`
	// Via names the surface that produced this message, as a view URN
	// (pkg/web/viewurn). Server-minted by webd/ap; channelsd re-validates the
	// grammar fail-closed on receipt. Empty for a surface with no view URN.
	Via string `json:"via,omitempty"`
	// DeferEcho, when true, tells channelsd NOT to publish the raw user_echo for
	// this message: its Text is a large/structured payload (an annotation
	// bundle) whose human-readable mirror is authored elsewhere (the runner's
	// summary echo). Left false by ordinary user messages, which echo verbatim.
	DeferEcho bool `json:"deferEcho,omitempty"`

	// RequestID is a client-generated idempotency key. A view surface that may
	// retry a send (the web chat's Retry) reuses the SAME id across retries;
	// channelsd's HandleViewMessage returns the first delivery's cached result
	// for a repeat id instead of re-delivering, so a reply lost to a transient
	// timeout can't cause a double-post. Empty disables dedup (surfaces that
	// never retry).
	RequestID string `json:"requestId,omitempty"`
}

// ViewMessageResultPayload is the reply to a KindViewMessage request. It
// mirrors the caller-visible fields of channelkinds.InboundDecision; this
// package stays free of channelkinds (which imports it), the same reason
// ExternalIdentity is mirrored rather than shared.
type ViewMessageResultPayload struct {
	// Outcome mirrors channelkinds.InboundDecision.Outcome — what channelsd
	// did with the message (delivered, parked, refused, …).
	Outcome string `json:"outcome"`
	// Notice is the user-facing message for this decision, or nil when there
	// is none. It carries its own severity so a receiving surface can style
	// it without holding a category registry.
	Notice *NoticeWire `json:"notice,omitempty"`
	// NewSession reports that this message started a session rather than
	// joining one, so the caller can render a fresh transcript.
	NewSession bool `json:"newSession,omitempty"`
	// RequesterCanonicalID is the canonical subject channelsd derived from
	// Author. Empty when it could not be derived.
	RequesterCanonicalID string `json:"requesterCanonicalId,omitempty"`

	// Error carries a handler-side failure so the requester surfaces a real
	// reason instead of waiting out its timeout (AGENTS.md: no silent errors).
	Error string `json:"error,omitempty"`
}

// ResurfaceRequestPayload is the body of a KindResurfaceRequest envelope,
// published on ap.session.<ns>.<name>.in.resurface_request. The session it
// applies to is the envelope's own ns/name; the only body field is the
// surface's view URN.
//
// It deliberately carries NO actor claim and channelsd runs no standing check
// on it, unlike view_message next door. A resurface confers nothing on its
// publisher: it re-publishes an already-outstanding prompt on the session's
// OUT subject, to the audience the prompt itself names, resolved downstream by
// the same sender that resolved it the first time. Nothing flows back to
// whoever asked. Adding an actor claim here would mean re-deriving a canonical
// subject to gate an operation that reveals and grants nothing — cost and a
// fail-closed misfire, for no privilege boundary. Revisit if a future
// resurface path ever addresses the REQUESTER rather than the prompt's own
// audience.
type ResurfaceRequestPayload struct {
	// Via names the surface that attached, as a view URN (pkg/web/viewurn).
	// Server-minted by webd/ap; channelsd re-validates the grammar fail-closed
	// on receipt. Empty for a surface with no view URN. It is what makes a
	// resurface storm diagnosable — the handler logs which surface asked.
	Via string `json:"via,omitempty"`
}

// UserEchoPayload is the body of a KindUserEcho envelope. Author + Via are
// carried so the origin kind can resolve a real @mention (via
// UserIdentity.status.channelIdentities) and render the surface description
// (viewurn.Describe). This is the outbound mirror of ViewMessagePayload.
type UserEchoPayload struct {
	// Text is the message as the user typed it, mirrored verbatim.
	Text   string           `json:"text"`
	Author ExternalIdentity `json:"author"`
	// Via is the originating surface's view URN (pkg/web/viewurn); empty for
	// a surface with no view URN.
	Via string `json:"via,omitempty"`
	// RequestID carries the originating ViewMessagePayload.RequestID (the
	// client's idempotency key). A view surface that renders its OWN sends
	// optimistically (the web chat's pending bubble) uses it to suppress the
	// echo of a message it already showed, while still rendering echoes from
	// OTHER surfaces/tabs (whose RequestID matches no local send). Empty for
	// surfaces that don't set a RequestID (e.g. annotation summaries).
	RequestID string `json:"requestId,omitempty"`
}
