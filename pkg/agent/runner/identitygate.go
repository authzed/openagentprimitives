package runner

// identitygate.go implements the SessionStart IdentityChoiceGate for
// AgentClass.spec.identityMode == "ask" | "dynamic". Unlike ColdStartScope
// (which lives in pkg/authz/hooks and reaches the runner through a deps struct
// to avoid an import cycle), this gate lives IN package runner and holds the
// *Loop + *runnerHost directly, so it calls Loop internals without indirection.
//
// Shape (mirrors ColdStartScope.Eval): publish the choice request → block on the
// approval orchestrator → map the decision to a Verdict, failing CLOSED whenever
// the interactive path is unavailable (no publish channel, no orchestrator,
// envelope build failure, transport error from Await). An ask|dynamic session
// must NEVER silently run as the agent identity.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/runner/identityadvisor"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// DefaultIdentityChoiceTimeout bounds the human-choice wait when Loop.
// IdentityChoiceTimeout is unset (0). Matches the CRD default for
// AgentClass.spec.identityChoiceTimeout (30m) so the runner's live wait and the
// operator's backstop measure the same window.
const DefaultIdentityChoiceTimeout = 30 * time.Minute

// User-facing notices delivered via Decision.Notices on a fail-closed / cancel /
// timeout Halt (the executor owns delivery to the requester — no synchronous host
// call from the gate). No-silent-errors: the initiating user always learns why
// the session stopped.
const (
	noticeIdentityCancelled   = "You cancelled — the agent will not run."
	noticeIdentityTimeout     = "No identity was chosen in time; the session was stopped."
	noticeIdentityUnavailable = "This session needs you to choose an identity, but the choice prompt couldn't be delivered; the session was stopped."
)

// IdentityChoiceGate is the SessionStart hook that asks the initiating user to
// choose the session's identity (act as the agent, or pass through as
// themselves) for identityMode=ask|dynamic. It self-gates: a session whose
// effective mode is already resolved (static mode, or a resumed session past its
// choice) short-circuits to Allow, so the gate can be registered unconditionally.
type IdentityChoiceGate struct {
	l    *Loop
	host *runnerHost
}

// newIdentityChoiceGate constructs the gate bound to a Loop and the per-call
// SessionStart host (the host carries the userPassthrough handoff flag Task 6
// reads to exit non-terminally).
func newIdentityChoiceGate(l *Loop, host *runnerHost) *IdentityChoiceGate {
	return &IdentityChoiceGate{l: l, host: host}
}

func (g *IdentityChoiceGate) Name() string { return "identity_choice_gate" }

func (g *IdentityChoiceGate) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.SessionStart}
}

func (g *IdentityChoiceGate) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	l := g.l

	// Step 1: already-resolved short-circuit. The reconciler stamps
	// EffectiveIdentityMode for static modes (mirrors spec.identityMode) and it
	// is persisted from the signed choice event once a user has answered. Either
	// way, a set effective mode means there is nothing to ask — proceed. Runs
	// BEFORE the non-interactive guard so a resumed session with no live channel
	// still proceeds on its recorded choice.
	if l.AgentSession != nil && l.AgentSession.Status.EffectiveIdentityMode != "" {
		return pipeline.Decision{}
	}

	// Fail-closed guard: without a publish channel + orchestrator there is no way
	// to ask a human. Refuse to run rather than default to the agent identity.
	if l.IdentityChoicePublish == nil || l.Approval == nil {
		slog.Default().Info("identity gate: interactive path not wired; failing session closed",
			"session", in.Session.String(),
			"hasPublish", l.IdentityChoicePublish != nil,
			"hasApproval", l.Approval != nil)
		return haltWithIdentityNotice("identity_choice_unavailable", noticeIdentityUnavailable)
	}

	// Step 2: park. IdentityChoicePending → phase AwaitingIdentityChoice. Reset
	// activity to active (the first emitActivity always fires, clearing stale
	// paused state) before we flip to paused ahead of the human wait.
	l.emitLifecycleEvent(ctx, lifecyclecore.IdentityChoicePending{})
	l.emitActivity(ctx, false, "")

	// Step 3: dynamic recommendation (fail-open). A wired recommender is what
	// distinguishes dynamic from a plain ask. On ANY error we log and continue
	// with an empty recommendation — session start must NOT couple to advisor
	// availability (degrade dynamic → plain ask).
	mode := "ask"
	var rec identityadvisor.Recommendation
	if l.IdentityRecommender != nil {
		mode = "dynamic"
		l.notifyBestEffort(ctx, "Choosing how to run this session…")
		r, rerr := l.IdentityRecommender.Recommend(ctx, g.recommenderRequest(in))
		if rerr != nil {
			slog.Default().Info("identity gate: recommender failed; degrading to plain ask",
				"session", in.Session.String(),
				"provider", l.IdentityRecommender.Name(),
				"err", rerr.Error())
		} else {
			rec = r
		}
	}

	// Step 4: build the choice request — the unified Interaction model
	// (pkg/channels/channelevents/interaction.go), category identity_choice. This is a
	// generic, channel-agnostic prompt: Lead/Body/Actions are trusted publisher
	// copy rendered live by every surface's "interaction" sub-channel sender
	// (no channel-specific formatting here — AGENTS.md "no channel-specifics in
	// the generic prompt").
	reqID := newRequestID()
	timeout := l.IdentityChoiceTimeout
	if timeout <= 0 {
		timeout = DefaultIdentityChoiceTimeout
	}
	ns, name := l.SessionKey.Namespace, l.SessionKey.Name
	requester := g.requester()
	agentDisplayName := l.agentClassDisplayName()

	lead := "Which identity should this agent use for this session?"
	if agentDisplayName != "" {
		lead = "Which identity should " + agentDisplayName + " use for this session?"
	}
	agentLabel := identityChoiceAgentActionLabel(agentDisplayName)
	actions := []channelevents.InteractionAction{
		{ID: "agent", Kind: channelevents.ActionKindDecision, Label: agentLabel},
		{ID: "userPassthrough", Kind: channelevents.ActionKindDecision, Label: "Run as me"},
		{ID: "cancel", Kind: channelevents.ActionKindDecision, Label: "Cancel"},
	}

	// Dynamic recommendation (fail-open, Step 3 above): mode=="dynamic" alone
	// means a recommender is wired, not that it produced a usable answer — an
	// errored/empty rec.Mode leaves the prompt unadorned (plain ask look).
	var body string
	var excerpt *channelevents.InteractionExcerpt
	if mode == "dynamic" && rec.Mode != "" {
		// SECURITY: rec.Mode is the advisory recommender's raw output — not
		// validated against the two known actions. A `default: recLabel =
		// rec.Mode` here would route that unvalidated string straight into
		// Body, which every surface renders as live markup. Only the two known
		// modes set recLabel (and thence Body); an unrecognized Mode leaves
		// recLabel/Body empty rather than echoing untrusted text.
		var recLabel string
		switch rec.Mode {
		case "agent":
			recLabel = agentLabel
			actions[0].Style = channelevents.ActionStylePrimary
		case "userPassthrough":
			recLabel = "Run as me"
			actions[1].Style = channelevents.ActionStylePrimary
		}
		if recLabel != "" {
			body = "Suggested: " + recLabel
		}
		if rec.Reason != "" {
			// SECURITY: rec.Reason is the advisory recommender LLM's free-text
			// justification — untrusted-influenced (may reflect thread content
			// verbatim or in paraphrase, per identityadvisor.Recommendation's doc
			// comment). Lead/Body/Actions are rendered as live markup by every
			// surface (see InteractionRequestPayload's CONTRACT), so untrusted
			// content MUST route through Excerpt, never Body.
			excerpt = &channelevents.InteractionExcerpt{Label: "Why", Content: rec.Reason}
		}
	}

	payload := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        categories.IdentityChoice,
		RequestRef:      reqID,
		Lead:            lead,
		Body:            body,
		Excerpt:         excerpt,
		Actions:         actions,
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &requester,
		},
	}
	env, berr := channelevents.BuildEnvelope(ns, name, channelevents.KindInteractionRequest, payload)
	if berr != nil {
		slog.Default().Info("identity gate: build choice envelope failed; failing session closed",
			"session", in.Session.String(), "err", berr.Error())
		return haltWithIdentityNotice("identity_choice_build_failed", noticeIdentityUnavailable)
	}

	// Waiting on a human: caption + paused activity so channelsd's silence
	// watchdog extends through the wait.
	l.notifyBestEffort(ctx, "Waiting for you to choose an identity")
	l.emitActivity(ctx, true, channelevents.PauseCauseIdentityChoice)

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	publish := l.IdentityChoicePublish
	d, aerr := l.Approval.Await(waitCtx, approval.Request{
		RequestID:  reqID,
		SessionRef: ns + "/" + name,
		OnPublish: func(pubCtx context.Context) error {
			return publish(pubCtx, ns, name, env)
		},
	})

	// Step 5: map the outcome.
	switch {
	case aerr != nil && errors.Is(aerr, context.DeadlineExceeded):
		// Timeout: the operator backstop finalizes the PHASE (emits
		// IdentityChoiceTimeout); the gate must NOT emit a lifecycle event or
		// the two writers double-finalize. The gate DOES own the channel-facing
		// surface — resolve the 3-button ephemeral to its timed-out wording so it
		// doesn't dangle, publishing on the parent ctx (waitCtx is already done).
		slog.Default().Info("identity gate: choice timed out; halting (operator backstop finalizes)",
			"session", in.Session.String())
		g.publishIdentityChoiceTimeoutApplied(ctx, reqID)
		return haltWithIdentityNotice("identity_choice_timeout", noticeIdentityTimeout)
	case aerr != nil:
		// Publish/transport failure surfaced through Await. Fail closed.
		slog.Default().Info("identity gate: await choice failed; failing session closed",
			"session", in.Session.String(), "err", aerr.Error())
		return haltWithIdentityNotice("identity_choice_await_failed", noticeIdentityUnavailable)
	}

	switch d.Action {
	case "agent":
		// d.ApproverID is the channel identity of the human who clicked — recorded
		// in the signed log so "who confirmed the agent identity" is auditable.
		l.emitLifecycleEvent(ctx, lifecyclecore.IdentityChoiceResolved{Mode: "agent", ConfirmedBy: d.ApproverID})
		// Un-park: the runner proceeds; its "<agent> thinking…" caption replaces
		// the wait caption on the next turn boundary.
		l.emitActivity(ctx, false, "")
		return pipeline.Decision{}

	case "userPassthrough":
		l.emitLifecycleEvent(ctx, lifecyclecore.IdentityChoiceResolved{Mode: "userPassthrough", ConfirmedBy: d.ApproverID})
		if g.host != nil {
			g.host.setIdentityHandoff(true)
		}
		// Handoff halt: NOT a failure. loop.Run distinguishes it from a
		// fail-closed halt by the reason + the host flag and exits non-terminally
		// (Task 6). No user-facing notice — the passthrough flow continues.
		return pipeline.Decision{Verdict: pipeline.Halt, Reason: "identity_handoff_passthrough"}

	case "cancel":
		l.emitLifecycleEvent(ctx, lifecyclecore.IdentityChoiceCancelled{CancelledBy: d.ApproverID})
		return haltWithIdentityNotice("identity_cancelled", noticeIdentityCancelled)

	default:
		// Unknown action from the channel: fail closed rather than assume agent.
		slog.Default().Info("identity gate: unknown choice action; failing session closed",
			"session", in.Session.String(), "action", d.Action)
		return haltWithIdentityNotice("identity_choice_unknown_action", noticeIdentityUnavailable)
	}
}

// requester resolves WHO is being asked to choose: the session's startedBy
// external ID + verified email, read together from the AgentSession's
// StartedBy* annotations. Mirrors the tool_call approval requester resolution
// (pipeline_wiring.go) for ExternalID, but ALSO carries Email, because
// identity_choice is a DecideRequester category
// (pkg/channels/channelinteractions/categories.go): channelsd's
// HandleInteractionDecision accepts a decision only when the cached Requester's
// identity.FromExternal(Kind,TeamScope,ExternalID,Email).Canonical() matches the
// decider's own, WITHOUT AllowSynthetic — and Canonical() keys purely on Email
// once it is set (see pkg/platform/identity/principal.go), so an email-less
// Requester can never canonicalize and every decision is rejected.
//
// DELIBERATELY does NOT read l.LastInboundExternalID: Loop has no paired "last
// inbound email" counterpart, so preferring it pairs a live inbound id with a
// DIFFERENT identity's email — or with none at all when it beats the annotation
// fallback. ExternalID and Email MUST come from the SAME identity, and the
// annotations are stamped as a matched pair by the AgentSession reconciler /
// channel binding (spiceboxv1alpha1.AnnotationStartedByExternalID /
// AnnotationStartedByEmail). Do not reach for LastInboundExternalID without also
// adding and wiring a paired email field.
func (g *IdentityChoiceGate) requester() channelevents.ExternalIdentity {
	l := g.l
	var ext, email string
	if l.AgentSession != nil {
		ext = l.AgentSession.Annotations[spiceboxv1alpha1.AnnotationStartedByExternalID]
		email = l.AgentSession.Annotations[spiceboxv1alpha1.AnnotationStartedByEmail]
	}
	return channelevents.ExternalIdentity{Kind: identity.Kind(l.AddressableChannelKind()), ExternalID: identity.RawExternalID(ext), Email: identity.Email(email)}
}

// identityChoiceAgentActionLabel renders the "agent" action's label, falling
// back to a generic phrasing when the AgentClass has no display name.
func identityChoiceAgentActionLabel(agentDisplayName string) string {
	if agentDisplayName == "" {
		return "Run as the agent"
	}
	return "Run as " + agentDisplayName
}

// publishIdentityChoiceTimeoutApplied resolves the channel-facing choice prompt
// when the human wait lapses with no answer: a synthetic KindInteractionApplied
// envelope (category identity_choice, Outcome=Expired) on the same OUT subject
// the request used, so the sender renders the timed-out wording instead of the
// prompt dangling with live buttons until the transport ages it out.
//
// Mirrors host_approval.go's publishTimeoutApplied, but OUT-only: identity_choice
// keeps no channelsd pending queue (no IN-side surface to clear), and the runner
// itself subscribes to interaction_applied on OUT. No double-resolve — the
// runner's own subscribeInteractionApplied receives this publish and calls
// DeliverDecision, but Await forgot the request when its deadline fired.
//
// Best-effort: without a publish hook (kubectl/test session) there is no surface
// to resolve; a build/publish failure is logged, never fatal — the session fails
// closed on timeout regardless.
func (g *IdentityChoiceGate) publishIdentityChoiceTimeoutApplied(ctx context.Context, reqID string) {
	l := g.l
	if l.IdentityChoicePublish == nil {
		return
	}
	ns, name := l.SessionKey.Namespace, l.SessionKey.Name
	env, err := channelevents.BuildEnvelope(ns, name, channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
			Category:        categories.IdentityChoice,
			RequestRef:      reqID,
			Outcome:         channelevents.OutcomeExpired,
			Reason:          "timeout",
		})
	if err != nil {
		slog.Default().Info("identity gate: build timeout-applied envelope failed",
			"session", ns+"/"+name, "reqID", reqID, "err", err.Error())
		return
	}
	if perr := l.IdentityChoicePublish(ctx, ns, name, env); perr != nil {
		slog.Default().Info("identity gate: publish timeout-applied failed",
			"session", ns+"/"+name, "reqID", reqID, "err", perr.Error())
	}
}

// recommenderRequest assembles the advisory LLM's input from material available
// at the gate. The recommendation is advisory and a human always confirms.
//
// IsDirectMessage and ParticipantCount come from the ChannelBinding.Key
// conversation-shape convention (see below) — the most trustworthy signals for
// the "solo DM vs busy channel thread" heuristic. ThreadDepth and
// ThreadTranscript stay zero/empty: both need an upstream thread-history fetch
// (the ReadHistory / read_channel_history path), a runtime tool call not plumbed
// to a SessionStart hook.
func (g *IdentityChoiceGate) recommenderRequest(in pipeline.Input) identityadvisor.Request {
	l := g.l
	req := identityadvisor.Request{
		AgentName:      l.agentClassDisplayName(),
		ChannelKind:    l.ChannelKind,
		InitiatingUser: l.LastInboundExternalID,
	}
	if l.AgentClass != nil && l.AgentClass.Spec.IdentityRecommendation != nil {
		req.ClassPrompt = l.AgentClass.Spec.IdentityRecommendation.Prompt
	}
	if in.Turn != nil {
		req.InboundText = in.Turn.Text
	}
	// ChannelBinding.Key encodes the conversation shape (documented on the CRD
	// type): "dm:<user>" is a 1:1 direct message; "thread:<chan>:<ts>" is a
	// channel thread. A kind that uses neither prefix degrades safely to the
	// "channel, not DM" default. (A first-class channelkinds method reporting
	// conversation shape would be the cleaner seam than reading the key prefix
	// here; deferred to avoid new cross-package plumbing for an advisory signal.)
	if l.AgentSession != nil && l.AgentSession.Spec.InputChannel != nil {
		if strings.HasPrefix(l.AgentSession.Spec.InputChannel.Key, "dm:") {
			req.IsDirectMessage = true
			// A DM is 1:1 — exactly the initiating human. The exact participant
			// count of a multi-person thread is NOT knowable here without a
			// thread-history fetch (deferred, as above), so it stays 0 for threads.
			req.ParticipantCount = 1
		}
	}
	return req
}

// notifyBestEffort sets a channel caption when a channel surface is wired.
// Best-effort UX only (kubectl-driven sessions and tests have no channel).
func (l *Loop) notifyBestEffort(ctx context.Context, text string) {
	if l.Notify == nil {
		return
	}
	l.Notify(ctx, text)
}

// haltWithIdentityNotice builds a fail-closed Halt carrying a user-facing
// notice. Routing the notice through Decision.Notices lets the executor own
// delivery (no synchronous host call from the gate). Mirrors the
// coldstartscope.go haltWithNotice pattern.
func haltWithIdentityNotice(reason, userText string) pipeline.Decision {
	return pipeline.Decision{
		Verdict: pipeline.Halt,
		Reason:  reason,
		Notices: []pipeline.Notice{{
			Notice: notice.New(categories.SessionHalted, notice.Args{
				Lead:     "This session was stopped",
				Body:     userText,
				NextStep: "Start a new session to try again.",
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester},
			}),
			ToRequester: true,
		}},
	}
}

var _ pipeline.Hook = (*IdentityChoiceGate)(nil)
