package runner

// identitygate_test.go exercises the SessionStart IdentityChoiceGate with a
// REAL approval orchestrator (not a fake): the capturing IdentityChoicePublish
// extracts the reqID the gate minted and delivers a canned decision from a
// goroutine, so Await routes a genuine Decision through the real orchestrator.
// This proves the gate's decision-routing (Action → Verdict + lifecycle event),
// not just its happy path.
//
// The gate publishes the unified Interaction model (channelevents/interaction.go):
// KindInteractionRequest(category=identity_choice) for the initial prompt,
// KindInteractionApplied(category=identity_choice, Outcome=Expired) for the
// synthetic timeout resolution. This fixture captures both generically.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/runner/identityadvisor"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// identityGateFixture builds a Loop wired for the identity gate: a real
// orchestrator, a lifecycle log to capture emitted events, and (optionally) a
// recommender. The returned deliver func is what the capturing publish calls
// from a goroutine once it has the minted reqID.
type identityGateFixture struct {
	l    *Loop
	host *runnerHost
	orch *approval.Orchestrator
	mem  *memory.Local
	key  memory.NamespacedName

	// lastPayload is the interaction_request payload the gate published
	// (captured by the publish closure), available after Eval returns.
	lastPayload *channelevents.InteractionRequestPayload
	// appliedPayloads captures any interaction_applied envelopes the gate
	// published on the OUT path (the synthetic timeout-applied). Populated
	// synchronously within Eval, so it is race-free to read after Eval returns.
	appliedPayloads []channelevents.InteractionAppliedPayload
}

// buildIdentityGateFixture wires a Loop + gate. When deliver is non-nil, the
// capturing publish spawns a goroutine that calls it with the minted reqID
// (unblocking Await). When deliver is nil, no decision is delivered (timeout
// path). recommender may be nil (plain ask).
func buildIdentityGateFixture(t *testing.T, recommender identityadvisor.Provider, deliver func(orch *approval.Orchestrator, reqID string)) *identityGateFixture {
	t.Helper()
	key := memory.NamespacedName{Namespace: "ns-id", Name: "sess-id"}
	mem := buildLifecycleMemory()
	orch := approval.New()
	f := &identityGateFixture{orch: orch, mem: mem, key: key}

	l := &Loop{
		SessionKey:          key,
		LifecycleMemory:     mem,
		Approval:            orch,
		IdentityRecommender: recommender,
		ChannelKind:         "slack",
		// requester() (identitygate.go) reads the paired StartedBy* annotations,
		// NOT LastInboundExternalID (see its doc comment) — set both here so
		// TestIdentityChoiceGate_PublishesInteractionRequestWithThreeActionsAndRequesterAudience
		// exercises the real production pairing.
		AgentSession: &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					spiceboxv1alpha1.AnnotationStartedByExternalID: "U-requester",
					spiceboxv1alpha1.AnnotationStartedByEmail:      "requester@example.org",
				},
			},
		},
		AgentName:             "helper",
		IdentityChoiceTimeout: 2 * time.Second,
	}
	l.IdentityChoicePublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		switch env.Kind {
		case channelevents.KindInteractionApplied:
			var ap channelevents.InteractionAppliedPayload
			if err := json.Unmarshal(env.Payload, &ap); err != nil {
				return err
			}
			f.appliedPayloads = append(f.appliedPayloads, ap)
			return nil
		case channelevents.KindInteractionRequest:
			var pl channelevents.InteractionRequestPayload
			if err := json.Unmarshal(env.Payload, &pl); err != nil {
				return err
			}
			f.lastPayload = &pl
			if deliver != nil {
				go deliver(orch, pl.RequestRef)
			}
			return nil
		default:
			t.Fatalf("unexpected envelope kind published: %s", env.Kind)
			return nil
		}
	}
	f.l = l
	f.host = newRunnerHost(l, hostSession{Namespace: key.Namespace, Name: key.Name, Class: l.AgentName})
	return f
}

// emittedEvents reads back the events the gate appended to the lifecycle log.
func (f *identityGateFixture) emittedEvents(t *testing.T) []lifecyclecore.Event {
	t.Helper()
	scope := memory.Scope{Kind: "session", ID: f.key.Namespace + "/" + f.key.Name}
	evs, err := lifecycle.Events(memory.WithSystemApproval(context.Background(), "test"), f.mem, scope)
	require.NoError(t, err, "read lifecycle events")
	return evs
}

func hasResolvedMode(evs []lifecyclecore.Event, mode string) bool {
	for _, e := range evs {
		if r, ok := e.(lifecyclecore.IdentityChoiceResolved); ok && r.Mode == mode {
			return true
		}
	}
	return false
}

// findResolved returns the first IdentityChoiceResolved event, or false.
func findResolved(evs []lifecyclecore.Event) (lifecyclecore.IdentityChoiceResolved, bool) {
	for _, e := range evs {
		if r, ok := e.(lifecyclecore.IdentityChoiceResolved); ok {
			return r, true
		}
	}
	return lifecyclecore.IdentityChoiceResolved{}, false
}

// findCancelled returns the first IdentityChoiceCancelled event, or false.
func findCancelled(evs []lifecyclecore.Event) (lifecyclecore.IdentityChoiceCancelled, bool) {
	for _, e := range evs {
		if c, ok := e.(lifecyclecore.IdentityChoiceCancelled); ok {
			return c, true
		}
	}
	return lifecyclecore.IdentityChoiceCancelled{}, false
}

func hasEvent[T lifecyclecore.Event](evs []lifecyclecore.Event) bool {
	for _, e := range evs {
		if _, ok := e.(T); ok {
			return true
		}
	}
	return false
}

func identityGateCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// deliverAction returns a deliver func that answers with the given Action.
func deliverAction(action string) func(orch *approval.Orchestrator, reqID string) {
	return func(orch *approval.Orchestrator, reqID string) {
		orch.DeliverDecision(reqID, approval.Decision{Action: action, ApproverID: "U-approver"})
	}
}

// actionByID returns the InteractionAction with the given ID, or fails the test.
func actionByID(t *testing.T, actions []channelevents.InteractionAction, id string) channelevents.InteractionAction {
	t.Helper()
	for _, a := range actions {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no action with id %q in %+v", id, actions)
	return channelevents.InteractionAction{}
}

// TestIdentityChoiceGate_PublishesInteractionRequestWithThreeActionsAndRequesterAudience
// verifies the gate's Eval publishes a category=identity_choice
// interaction_request with exactly the 3 decision actions
// (agent/userPassthrough/cancel) and a requester-scoped audience addressed to
// the initiating user — the shape the generic pipe's DecideRequester standing
// check and the runner's resume subscriber both depend on.
func TestIdentityChoiceGate_PublishesInteractionRequestWithThreeActionsAndRequesterAudience(t *testing.T) {
	ctx := identityGateCtx(t)
	f := buildIdentityGateFixture(t, nil, deliverAction("agent"))
	gate := newIdentityChoiceGate(f.l, f.host)

	dec := gate.Eval(ctx, pipeline.Input{Session: pipeline.SessionRef{Namespace: f.key.Namespace, Name: f.key.Name}})
	require.Equal(t, pipeline.Allow, dec.Verdict)

	require.NotNil(t, f.lastPayload, "publish must have been called")
	assert.Equal(t, categories.IdentityChoice, f.lastPayload.Category, "Category")
	assert.NotEmpty(t, f.lastPayload.RequestRef, "RequestRef")
	assert.NotEmpty(t, f.lastPayload.Lead, "Lead must not be empty (InteractionRequestPayload.Validate requires it)")

	require.Len(t, f.lastPayload.Actions, 3, "exactly 3 decision actions")
	for _, id := range []string{"agent", "userPassthrough", "cancel"} {
		a := actionByID(t, f.lastPayload.Actions, id)
		assert.Equal(t, channelevents.ActionKindDecision, a.Kind, "action %q kind", id)
		assert.NotEmpty(t, a.Label, "action %q label", id)
	}

	assert.Equal(t, channelevents.AudienceRequester, f.lastPayload.Audience.Scope, "Audience.Scope")
	require.NotNil(t, f.lastPayload.Audience.Requester, "Audience.Requester")
	assert.Equal(t, "slack", f.lastPayload.Audience.Requester.Kind.String())
	assert.Equal(t, "U-requester", f.lastPayload.Audience.Requester.ExternalID.String())
	assert.Equal(t, "requester@example.org", f.lastPayload.Audience.Requester.Email.String(),
		"Email must be paired with ExternalID from the SAME source (StartedBy* annotations) — "+
			"see requester()'s doc comment on why LastInboundExternalID must never be preferred here")
}

// TestIdentityChoiceGate_Agent verifies the agent action → Allow and a signed
// IdentityChoiceResolved{agent} event.
func TestIdentityChoiceGate_AgentActionAllowsAndResolvesAgent(t *testing.T) {
	ctx := identityGateCtx(t)
	f := buildIdentityGateFixture(t, nil, deliverAction("agent"))
	gate := newIdentityChoiceGate(f.l, f.host)

	dec := gate.Eval(ctx, pipeline.Input{Session: pipeline.SessionRef{Namespace: f.key.Namespace, Name: f.key.Name}})

	assert.Equal(t, pipeline.Allow, dec.Verdict, "agent choice must Allow")
	evs := f.emittedEvents(t)
	assert.True(t, hasEvent[lifecyclecore.IdentityChoicePending](evs), "IdentityChoicePending must be emitted at park")
	assert.True(t, hasResolvedMode(evs, "agent"), "IdentityChoiceResolved{agent} must be emitted")
	r, ok := findResolved(evs)
	require.True(t, ok, "a resolved event must be emitted")
	assert.Equal(t, "U-approver", r.ConfirmedBy, "the clicker's channel identity is recorded in the signed log")
	assert.False(t, f.host.takeIdentityHandoff(), "agent choice must NOT set the handoff flag")
}

// TestIdentityChoiceGate_Passthrough verifies userPassthrough → handoff Halt +
// the host flag set + IdentityChoiceResolved{userPassthrough}.
func TestIdentityChoiceGate_PassthroughActionHandsOff(t *testing.T) {
	ctx := identityGateCtx(t)
	f := buildIdentityGateFixture(t, nil, deliverAction("userPassthrough"))
	gate := newIdentityChoiceGate(f.l, f.host)

	dec := gate.Eval(ctx, pipeline.Input{Session: pipeline.SessionRef{Namespace: f.key.Namespace, Name: f.key.Name}})

	assert.Equal(t, pipeline.Halt, dec.Verdict, "passthrough must Halt (handoff)")
	assert.Equal(t, "identity_handoff_passthrough", dec.Reason, "handoff reason distinguishes it from fail-closed")
	assert.True(t, f.host.takeIdentityHandoff(), "passthrough must set the host handoff flag")
	assert.True(t, hasResolvedMode(f.emittedEvents(t), "userPassthrough"),
		"IdentityChoiceResolved{userPassthrough} must be emitted")
}

// TestIdentityChoiceGate_Cancel verifies cancel → Halt + IdentityChoiceCancelled
// + a ToRequester notice.
func TestIdentityChoiceGate_CancelActionHaltsWithNoticeAndCancelledEvent(t *testing.T) {
	ctx := identityGateCtx(t)
	f := buildIdentityGateFixture(t, nil, deliverAction("cancel"))
	gate := newIdentityChoiceGate(f.l, f.host)

	dec := gate.Eval(ctx, pipeline.Input{Session: pipeline.SessionRef{Namespace: f.key.Namespace, Name: f.key.Name}})

	assert.Equal(t, pipeline.Halt, dec.Verdict, "cancel must Halt")
	assert.NotEqual(t, "identity_handoff_passthrough", dec.Reason, "cancel is not a handoff")
	require.Len(t, dec.Notices, 1, "cancel must carry a user-facing notice")
	assert.True(t, dec.Notices[0].ToRequester, "cancel notice goes to the requester")
	assert.NotEmpty(t, dec.Notices[0].Text, "cancel notice has text")
	c, ok := findCancelled(f.emittedEvents(t))
	require.True(t, ok, "IdentityChoiceCancelled must be emitted")
	assert.Equal(t, "U-approver", c.CancelledBy, "the canceller's channel identity is recorded in the signed log")
	assert.False(t, f.host.takeIdentityHandoff(), "cancel must NOT set the handoff flag")
}

// TestIdentityChoiceGate_Timeout verifies the timeout path: Halt with a notice
// and NO resolution/cancel/timeout lifecycle event (the operator backstop owns
// finalizing the timeout).
func TestIdentityChoiceGate_TimeoutHaltsWithoutResolutionEvent(t *testing.T) {
	ctx := identityGateCtx(t)
	f := buildIdentityGateFixture(t, nil, nil) // no deliver → Await times out
	f.l.IdentityChoiceTimeout = 60 * time.Millisecond
	gate := newIdentityChoiceGate(f.l, f.host)

	dec := gate.Eval(ctx, pipeline.Input{Session: pipeline.SessionRef{Namespace: f.key.Namespace, Name: f.key.Name}})

	assert.Equal(t, pipeline.Halt, dec.Verdict, "timeout must Halt")
	require.Len(t, dec.Notices, 1, "timeout must carry a user-facing notice")
	assert.True(t, dec.Notices[0].ToRequester, "timeout notice goes to the requester")

	evs := f.emittedEvents(t)
	assert.True(t, hasEvent[lifecyclecore.IdentityChoicePending](evs), "park event is still expected")
	assert.False(t, hasEvent[lifecyclecore.IdentityChoiceResolved](evs), "gate must NOT resolve on timeout")
	assert.False(t, hasEvent[lifecyclecore.IdentityChoiceCancelled](evs), "gate must NOT cancel on timeout")
	assert.False(t, hasEvent[lifecyclecore.IdentityChoiceTimeout](evs),
		"gate must NOT emit the timeout event (operator backstop owns it)")

	// The timeout path resolves the channel-facing prompt: a synthetic
	// interaction_applied (category identity_choice, Outcome=Expired,
	// Reason="timeout") is published on the OUT path so the choice prompt does
	// not dangle. RequestRef matches the request the gate minted.
	require.Len(t, f.appliedPayloads, 1, "timeout must publish exactly one synthetic applied")
	assert.Equal(t, categories.IdentityChoice, f.appliedPayloads[0].Category, "synthetic applied Category")
	assert.Equal(t, channelevents.OutcomeExpired, f.appliedPayloads[0].Outcome, "synthetic applied Outcome")
	assert.Equal(t, "timeout", f.appliedPayloads[0].Reason, "synthetic applied carries the timeout reason")
	require.NotNil(t, f.lastPayload, "the request was published before the wait")
	assert.Equal(t, f.lastPayload.RequestRef, f.appliedPayloads[0].RequestRef,
		"the timeout-applied resolves the same request the gate published")
}

// TestIdentityChoiceGate_RecommenderRequestStructuralSignals verifies the gate
// derives IsDirectMessage + ParticipantCount from the ChannelBinding.Key
// conversation-shape convention: a "dm:" key is a 1:1 DM (ParticipantCount=1);
// a "thread:" key (or any non-DM format) is a channel with an unknown count
// (0, deferred). ThreadDepth/ThreadTranscript stay zero/empty (deferred).
func TestIdentityChoiceGate_RecommenderRequestStructuralSignals(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		wantDM     bool
		wantPartic int
	}{
		{name: "dm key: IsDirectMessage=true, ParticipantCount=1", key: "dm:U-requester", wantDM: true, wantPartic: 1},
		{name: "thread key: IsDirectMessage=false, ParticipantCount=0 (deferred)", key: "thread:C1:1699.5", wantDM: false, wantPartic: 0},
		{name: "unknown key format degrades to not-DM", key: "fake-echo", wantDM: false, wantPartic: 0},
		{name: "no input channel (kubectl) degrades to not-DM", key: "", wantDM: false, wantPartic: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildIdentityGateFixture(t, nil, nil)
			f.l.AgentSession = &spiceboxv1alpha1.AgentSession{}
			if tc.key != "" {
				f.l.AgentSession.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Key: tc.key}
			}
			gate := newIdentityChoiceGate(f.l, f.host)

			req := gate.recommenderRequest(pipeline.Input{Turn: &pipeline.TurnInfo{Text: "hi"}})

			assert.Equal(t, tc.wantDM, req.IsDirectMessage, "IsDirectMessage from key shape")
			assert.Equal(t, tc.wantPartic, req.ParticipantCount, "ParticipantCount from key shape")
			assert.Zero(t, req.ThreadDepth, "ThreadDepth is deferred (needs thread-history fetch)")
			assert.Empty(t, req.ThreadTranscript, "ThreadTranscript is deferred (needs thread-history fetch)")
			assert.Equal(t, "slack", req.ChannelKind, "channel kind still flows through")
			assert.Equal(t, "hi", req.InboundText, "inbound text still flows through")
		})
	}
}

// TestIdentityChoiceGate_DynamicRecommendation verifies a healthy recommender's
// output flows into the published request: the recommended action is styled
// Primary, the suggestion appears in Body (trusted), and the untrusted
// LLM-generated Reason is routed through Excerpt (never Body/Lead — see the
// SECURITY comment in identitygate.go's Eval).
func TestIdentityChoiceGate_DynamicRecommendationPublished(t *testing.T) {
	ctx := identityGateCtx(t)
	rec := identityadvisor.Fake{Rec: identityadvisor.Recommendation{Mode: "userPassthrough", Reason: "thread is a DM"}}
	f := buildIdentityGateFixture(t, rec, deliverAction("agent"))
	gate := newIdentityChoiceGate(f.l, f.host)

	dec := gate.Eval(ctx, pipeline.Input{Session: pipeline.SessionRef{Namespace: f.key.Namespace, Name: f.key.Name}})

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.NotNil(t, f.lastPayload, "publish must have been called")
	assert.NotEmpty(t, f.lastPayload.Body, "a dynamic recommendation adds a Body suggestion")
	assert.Contains(t, f.lastPayload.Body, "Run as me", "Body names the recommended (trusted) label")

	require.NotNil(t, f.lastPayload.Excerpt, "the advisory Reason routes through Excerpt (untrusted)")
	assert.Equal(t, "thread is a DM", f.lastPayload.Excerpt.Content, "Excerpt.Content carries the reason verbatim")

	passthrough := actionByID(t, f.lastPayload.Actions, "userPassthrough")
	assert.Equal(t, channelevents.ActionStylePrimary, passthrough.Style, "recommended action is styled Primary")
	agentAction := actionByID(t, f.lastPayload.Actions, "agent")
	assert.NotEqual(t, channelevents.ActionStylePrimary, agentAction.Style, "non-recommended action keeps the default style")
}

// TestIdentityChoiceGate_DynamicRecommenderError verifies fail-open: a
// recommender error degrades to a request with no recommendation adornment —
// the session still proceeds (does not fail closed on advisor unavailability).
func TestIdentityChoiceGate_DynamicRecommenderErrorDegradesToEmptyRecommendation(t *testing.T) {
	ctx := identityGateCtx(t)
	rec := identityadvisor.Fake{Err: errors.New("advisor timeout")}
	f := buildIdentityGateFixture(t, rec, deliverAction("agent"))
	gate := newIdentityChoiceGate(f.l, f.host)

	dec := gate.Eval(ctx, pipeline.Input{Session: pipeline.SessionRef{Namespace: f.key.Namespace, Name: f.key.Name}})

	assert.Equal(t, pipeline.Allow, dec.Verdict, "recommender failure must not fail the session closed")
	require.NotNil(t, f.lastPayload, "publish must have been called despite advisor error")
	assert.Empty(t, f.lastPayload.Body, "advisor error degrades to no suggestion Body")
	assert.Nil(t, f.lastPayload.Excerpt, "advisor error degrades to no Excerpt")
	for _, a := range f.lastPayload.Actions {
		assert.NotEqual(t, channelevents.ActionStylePrimary, a.Style, "no action is styled Primary without a usable recommendation")
	}
}

// TestIdentityChoiceGate_NonInteractive verifies the fail-closed guard: without
// a publish channel the gate Halts (never Allows) and never blocks.
func TestIdentityChoiceGate_NonInteractiveFailsClosed(t *testing.T) {
	ctx := identityGateCtx(t)
	f := buildIdentityGateFixture(t, nil, nil)
	f.l.IdentityChoicePublish = nil // non-interactive
	gate := newIdentityChoiceGate(f.l, f.host)

	dec := gate.Eval(ctx, pipeline.Input{Session: pipeline.SessionRef{Namespace: f.key.Namespace, Name: f.key.Name}})

	assert.Equal(t, pipeline.Halt, dec.Verdict, "non-interactive session must fail closed")
	assert.NotEqual(t, pipeline.Allow, dec.Verdict, "must NEVER Allow without a human choice")
	require.Len(t, dec.Notices, 1, "fail-closed must surface a notice (no-silent-errors)")
	assert.True(t, dec.Notices[0].ToRequester)
}

// TestIdentityChoiceGate_AlreadyResolvedShortCircuits verifies that a session
// whose EffectiveIdentityMode is already set (static mode, or a resumed session
// past its choice) proceeds without gating.
func TestIdentityChoiceGate_AlreadyResolvedShortCircuits(t *testing.T) {
	ctx := identityGateCtx(t)
	f := buildIdentityGateFixture(t, nil, nil)
	f.l.IdentityChoicePublish = nil // even non-interactive: the short-circuit precedes the guard
	f.l.AgentSession = &spiceboxv1alpha1.AgentSession{}
	f.l.AgentSession.Status.EffectiveIdentityMode = "agent"
	gate := newIdentityChoiceGate(f.l, f.host)

	dec := gate.Eval(ctx, pipeline.Input{Session: pipeline.SessionRef{Namespace: f.key.Namespace, Name: f.key.Name}})

	assert.Equal(t, pipeline.Allow, dec.Verdict, "an already-resolved mode must not re-gate")
	assert.Empty(t, f.emittedEvents(t), "short-circuit must not emit any lifecycle event")
}

// TestIdentityChoiceGate_RequesterCarriesStartedByEmail is the focused
// regression test for review Critical fix round 1: the gate's requester()
// must attach the session initiator's verified email, not just their
// external id, or channelsd's HandleInteractionDecision (DecideRequester)
// can never canonicalize the cached requester (identity.Principal.Canonical
// rejects an email-less principal with ErrSyntheticSubject, without
// AllowSynthetic) and every identity_choice decision is rejected — see
// pkg/channels/channelsd/pipeline/interaction_decision_test.go's
// TestHandleInteractionDecision_IdentityChoiceGateShapedRequester_* for the
// end-to-end proof against the real pipeline check.
//
// LastInboundExternalID is left unset (mirrors production: it is never
// assigned by internal/cmd/runner today, only by test fixtures — see
// identitygate.go's requester() doc comment) so this exercises the
// AgentSession-annotation path a real webchat/Slack session takes.
func TestIdentityChoiceGate_RequesterCarriesStartedByEmail(t *testing.T) {
	l := &Loop{
		ChannelKind: browser.KindName, // webui/chat session.go stamps InputChannel.Kind = browser.KindName
		AgentSession: &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					spiceboxv1alpha1.AnnotationStartedByExternalID: "person@example.org",
					spiceboxv1alpha1.AnnotationStartedByEmail:      "person@example.org",
				},
			},
		},
	}
	gate := &IdentityChoiceGate{l: l}

	got := gate.requester()

	assert.Equal(t, browser.KindName, got.Kind.String(), "Kind")
	assert.Equal(t, "person@example.org", got.ExternalID.String(), "ExternalID")
	assert.Equal(t, "person@example.org", got.Email.String(),
		"Email must be populated from AnnotationStartedByEmail — the pre-fix requester() never set this field, "+
			"which made every identity_choice decision unresolvable (review Critical)")
}

// TestIdentityChoiceGate_RequesterEmptyWithoutStartedByEmailAnnotation pins
// the fail-closed default: an AgentSession with no verified-email annotation
// (kubectl-driven session, or a channel-native id the channel kind never
// resolved an email for) yields an empty Email, not a guessed/derived one.
// requester() must never synthesize an email from other fields — the gate
// stays fail-closed for a genuinely unverified initiator, same as they could
// never pass through as themselves (see requester()'s doc comment).
func TestIdentityChoiceGate_RequesterEmptyWithoutStartedByEmailAnnotation(t *testing.T) {
	l := &Loop{
		ChannelKind: "slack",
		AgentSession: &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					spiceboxv1alpha1.AnnotationStartedByExternalID: "U-guest",
					// no AnnotationStartedByEmail
				},
			},
		},
	}
	gate := &IdentityChoiceGate{l: l}

	got := gate.requester()

	assert.Equal(t, "U-guest", got.ExternalID.String(), "ExternalID")
	assert.Empty(t, got.Email, "Email must stay empty when the channel kind has no verified email for the starter")
}

// TestIdentityChoiceGate_RequesterIgnoresLastInboundExternalID is the
// regression guard for FU-1: LastInboundExternalID has no paired "last
// inbound email" counterpart anywhere in Loop, so a requester() that
// preferred it (as an earlier version did) would pair a live inbound id
// with EITHER a different identity's email or no email at all — an
// email-less Requester can never canonicalize in channelsd's
// HandleInteractionDecision (DecideRequester), reintroducing the review
// Critical every time something wires LastInboundExternalID (its doc
// comment on Loop.LastInboundExternalID in loop_deps.go hints at exactly this
// future). This sets BOTH LastInboundExternalID (simulating that future
// wiring) and the paired StartedBy* annotations, and asserts requester()
// still returns the annotation pair — proving the inbound id is ignored,
// not merely absent in today's fixtures.
func TestIdentityChoiceGate_RequesterIgnoresLastInboundExternalID(t *testing.T) {
	l := &Loop{
		ChannelKind:           "slack",
		LastInboundExternalID: "U-live-clicker", // simulates a future live wiring
		AgentSession: &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					spiceboxv1alpha1.AnnotationStartedByExternalID: "U-starter",
					spiceboxv1alpha1.AnnotationStartedByEmail:      "starter@example.org",
				},
			},
		},
	}
	gate := &IdentityChoiceGate{l: l}

	got := gate.requester()

	assert.Equal(t, "U-starter", got.ExternalID.String(),
		"ExternalID must come from the annotation pair, not the unpaired LastInboundExternalID")
	assert.Equal(t, "starter@example.org", got.Email.String(),
		"Email must be present and paired with the SAME ExternalID — a bare LastInboundExternalID must never win and leave Email empty")
}
