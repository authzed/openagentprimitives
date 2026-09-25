package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// agentClass builds the AgentClass CR the test Channels bind to ("ac1"),
// carrying the given Valid condition.
func agentClass(t *testing.T, status metav1.ConditionStatus, reason, message string) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentClassStatus{
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.AgentClassConditionValid,
				Status:             status,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// withHealthyAgentClass seeds a Valid=True "ac1" AgentClass unless the caller
// supplied one, making a WORKING agent the default every pipeline test starts
// from.
//
// Every test Channel binds to "ac1", and a Channel pointing at a nonexistent
// agent correctly produces a notice and an alert — real behaviour, not test
// noise, since nothing watches an AgentClass that does not exist and such a
// session parks forever. Tests that want an unhealthy agent pass their own
// class and override this.
func withHealthyAgentClass(t *testing.T, objs []client.Object) []client.Object {
	t.Helper()
	for _, o := range objs {
		if ac, ok := o.(*spiceboxv1alpha1.AgentClass); ok && ac.Name == "ac1" {
			return objs
		}
	}
	return append([]client.Object{
		agentClass(t, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve, ""),
	}, objs...)
}

// seedAgentClass creates ac, or overwrites it when the fixture already seeded
// one. An upsert rather than a Create because the shared fixtures now provide a
// healthy "ac1" by default (see withHealthyAgentClass), and a test that wants a
// class with a particular spec is refining that default, not competing with it.
func seedAgentClass(t *testing.T, cli client.Client, ac *spiceboxv1alpha1.AgentClass) {
	t.Helper()
	ctx := context.Background()
	var existing spiceboxv1alpha1.AgentClass
	if err := cli.Get(ctx, client.ObjectKeyFromObject(ac), &existing); err == nil {
		ac.ResourceVersion = existing.ResourceVersion
		require.NoError(t, cli.Update(ctx, ac), "seed AgentClass (overwrite)")
		return
	}
	require.NoError(t, cli.Create(ctx, ac), "seed AgentClass")
}

// deliverFrom sends one inbound from U1 on channel c1's default thread.
func deliverFrom(t *testing.T, p *Pipeline, ch *spiceboxv1alpha1.Channel, text string) channelkinds.InboundDecision {
	t.Helper()
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: text,
	})
	require.NoError(t, err, "Deliver(%q)", text)
	return dec
}

// unavailableNotices returns every agent_unavailable notice the recorder saw.
func unavailableNotices(t *testing.T, rec *fakeNATS) []channelevents.InteractionRequestPayload {
	t.Helper()
	var out []channelevents.InteractionRequestPayload
	for i, subj := range rec.subjects {
		if !strings.HasSuffix(subj, ".out."+string(channelevents.KindInteractionRequest)) {
			continue
		}
		var env channelevents.Envelope
		if err := json.Unmarshal(rec.payloads[i], &env); err != nil {
			continue
		}
		var pl channelevents.InteractionRequestPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			continue
		}
		if pl.Category == categories.AgentUnavailable {
			out = append(out, pl)
		}
	}
	return out
}

// monitoringEvents decodes every monitoring event the recorder captured.
func monitoringEvents(t *testing.T, rec *fakeNATS) []channelevents.MonitoringEvent {
	t.Helper()
	var out []channelevents.MonitoringEvent
	for i, subj := range rec.subjects {
		if subj != channelevents.MonitoringEventSubject {
			continue
		}
		var ev channelevents.MonitoringEvent
		require.NoError(t, json.Unmarshal(rec.payloads[i], &ev), "decode monitoring event %d", i)
		out = append(out, ev)
	}
	return out
}

// TestDeliver_UnhealthyAgent_ParksTheMessageAndSaysSo is the regression test for
// the silent stall. An expired credential leaves the AgentIdentity Valid=False,
// which propagates to the AgentClass; the session is created and parked by the
// operator, and before this NOTHING told the person waiting in the thread that
// their message had stopped moving.
//
// The message must still be accepted — parking is the designed recovery path,
// and the session answers the original message once the agent is healthy again.
func TestDeliver_UnhealthyAgent_ParksTheMessageAndSaysSo(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid,
		`AgentIdentity "pm-tools" is not Valid (Valid=False reason=AgentCredentialExpired)`)
	p, _, _, natsRec, cli := newPipeline(t, ch, ac)

	dec := deliverFrom(t, p, ch, "hello")

	// Accepted, not refused: the message is parked, not thrown away.
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome,
		"the message is accepted and parked; refusing it would lose it and force a re-send")
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions), "List sessions")
	require.Len(t, sessions.Items, 1, "a session is created so it can resume when the agent recovers")

	// …and the user is told, which is the whole point.
	notices := unavailableNotices(t, natsRec)
	require.Len(t, notices, 1, "the stalled conversation must be explained")
	assert.NotEmpty(t, notices[0].Lead, "lead")
	assert.Contains(t, notices[0].NextStep, "No need to re-send",
		"the copy must stop the user re-sending: the parked message will be answered")
	// Operator vocabulary must not reach a chat surface.
	whole := notices[0].Lead + notices[0].Body + notices[0].NextStep
	assert.NotContains(t, whole, "AgentIdentity", "internal object names must not reach the thread")
	assert.NotContains(t, whole, "AgentClass", "internal object names must not reach the thread")
}

// TestDeliver_UnhealthyAgent_AlertsOperatorsWithAnExactRemedy covers the
// operator half. The class going invalid already emits its own transition
// event, but that one cannot say a person is now waiting behind it.
func TestDeliver_UnhealthyAgent_AlertsOperatorsWithAnExactRemedy(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid,
		`AgentIdentity "pm-tools" is not Valid (Valid=False reason=AgentCredentialExpired)`)
	ac.Spec.AgentIdentity = "pm-tools"
	p, _, _, natsRec, _ := newPipeline(t, ch, ac)

	deliverFrom(t, p, ch, "hello")

	events := monitoringEvents(t, natsRec)
	require.Len(t, events, 1, "one alert for a blocked user")
	got := events[0]
	assert.Equal(t, channelevents.MonitoringLevelError, got.Level,
		"a person waiting outranks the background reconcile warning the class transition already sent")
	assert.Equal(t, "AgentClass", got.Source.Kind, "source names the object an operator must fix")
	assert.Equal(t, "ac1", got.Source.Name, "source.name")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentIdentityInvalid, got.Reason, "reason")
	assert.Contains(t, got.Summary, "c1", "summary names the channel someone is waiting on")
	assert.Contains(t, got.Hint, "oap identity setup pm-tools",
		"the remedy is exact: the identity name is on the class spec, so do not make an operator hunt for it")
}

// TestDeliver_UnhealthyAgent_StaysQuietForALiveRunner keeps a mid-session
// credential revocation surgical. The agent loses the revoked tool but its pod
// is up and still answering, so announcing "this agent can't pick this up"
// while it visibly replies would simply be false.
func TestDeliver_UnhealthyAgent_StaysQuietForALiveRunner(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "revoked mid-session")
	running := existingSession(t, "c1-live", "U1", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, _, _, natsRec, _ := newPipeline(t, ch, ac, running)

	dec := deliverFrom(t, p, ch, "and the second one?")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "the live runner answers as normal")
	assert.Empty(t, unavailableNotices(t, natsRec),
		"a session whose pod is up will reply; telling the user otherwise would be false")
	assert.Empty(t, monitoringEvents(t, natsRec),
		"nobody is blocked, so there is nothing to alert on")
}

// TestDeliver_UnhealthyAgent_NoticePerStallAlertPerOutage separates the two
// audiences. An agent can be down for a day; every conversation that stalls in
// that time is owed an explanation, and the operators are owed one alert.
func TestDeliver_UnhealthyAgent_NoticePerStallAlertPerOutage(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired")
	p, _, _, natsRec, _ := newPipeline(t, ch, ac)

	for _, text := range []string{"first", "second", "third"} {
		deliverFrom(t, p, ch, text)
	}

	assert.Len(t, unavailableNotices(t, natsRec), 3, "every stalled message is explained")
	assert.Len(t, monitoringEvents(t, natsRec), 1, "one outage, one alert")
}

// TestDeliver_UnhealthyAgent_ReAlertsAfterRecovery makes the dedup
// self-clearing. An agent that breaks, is fixed, and breaks again is a second
// outage; remembering the first forever would silence the alert for it.
func TestDeliver_UnhealthyAgent_ReAlertsAfterRecovery(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired")
	p, _, _, natsRec, cli := newPipeline(t, ch, ac)

	deliverFrom(t, p, ch, "during the first outage")
	require.Len(t, monitoringEvents(t, natsRec), 1, "first outage alerts")

	setValid(t, cli, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve)
	deliverFrom(t, p, ch, "after the fix")

	setValid(t, cli, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid)
	deliverFrom(t, p, ch, "during the second outage")
	assert.Len(t, monitoringEvents(t, natsRec), 2, "the second outage alerts too")
}

// TestDeliver_MissingAgentClass_PromisesNoRecovery is the one case where the
// reassurance elsewhere would be a lie. Nothing watches an AgentClass that does
// not exist, so a session parked behind one never resumes on its own — and
// telling the user to sit tight would leave them waiting forever.
func TestDeliver_MissingAgentClass_PromisesNoRecovery(t *testing.T) {
	ch := newChannel("c1")
	ch.Spec.AgentClass = "ac-deleted" // bound to an agent that does not exist
	p, _, _, natsRec, _ := newPipeline(t, ch)

	deliverFrom(t, p, ch, "anyone there?")

	notices := unavailableNotices(t, natsRec)
	require.Len(t, notices, 1, "a conversation bound to no agent must still be explained")
	assert.NotContains(t, notices[0].NextStep, "No need to re-send",
		"a missing class never self-heals, so the copy must not promise an eventual answer")
	assert.Contains(t, notices[0].NextStep, "follow up",
		"it must send the user to a human instead")

	events := monitoringEvents(t, natsRec)
	require.Len(t, events, 1, "operators are alerted")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassMissing, events[0].Reason, "reason")
	assert.Contains(t, events[0].Hint, "will NOT resume",
		"the hint must say this one needs a human, unlike every other reason")
}

// TestDeliver_HealthyAgent_SaysNothing pins the quiet path: the surfacing must
// cost a healthy conversation nothing at all.
func TestDeliver_HealthyAgent_SaysNothing(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve, "")
	p, _, _, natsRec, _ := newPipeline(t, ch, ac)

	dec := deliverFrom(t, p, ch, "hello")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	assert.Empty(t, unavailableNotices(t, natsRec), "a healthy agent explains nothing")
	assert.Empty(t, monitoringEvents(t, natsRec), "and alerts nothing")
}

// TestDeliver_UnhealthyAgent_CronInboundIsNotDescribedAsAUser keeps the alert
// honest about who is waiting. A cron firing has no human behind it: its acting
// subject comes from the Channel's authzSubject and is already a fully
// qualified "service:<id>". Prefixing that with "user:" would put a subject
// into the monitoring channel that matches nothing an admin can look up.
func TestDeliver_UnhealthyAgent_CronInboundIsNotDescribedAsAUser(t *testing.T) {
	ch := newBentoChannel("cron1", "service:nightly-report")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired")
	out := newSlackOutputChannel("cron1-out", "C_OUT")
	p, _, _, natsRec, _ := newPipeline(t, ch, ac, out)

	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ChannelKey:   "cron:cron1:1",
		MessageText:  "nightly run",
		AuthzSubject: "service:nightly-report",
	})
	require.NoError(t, err, "Deliver")

	events := monitoringEvents(t, natsRec)
	require.Len(t, events, 1, "a cron firing into a broken agent still alerts")
	assert.NotContains(t, events[0].Summary, "user:service:",
		"a service subject must not be re-prefixed as a user")
	assert.Contains(t, events[0].Summary, "service:nightly-report",
		"the acting subject is still named, just not mislabelled")
}

// TestDeliver_UnhealthyAgent_ExplainsAParkedSessionToo is the other half of the
// liveRunner branch. A conversation whose session went Idle has no pod: waking
// it needs a respawn the class gate blocks, so it stalls exactly like a fresh
// one and is owed the same explanation. Only a session with a pod ALREADY up
// (TestDeliver_UnhealthyAgent_StaysQuietForALiveRunner) is quiet.
func TestDeliver_UnhealthyAgent_ExplainsAParkedSessionToo(t *testing.T) {
	cases := []struct {
		name  string
		phase string
	}{
		{name: "Idle: pod exited, respawn is blocked → explain", phase: spiceboxv1alpha1.AgentSessionPhaseIdle},
		{name: "AwaitingCredentials: parked, respawn is blocked → explain", phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired")
			parked := existingSession(t, "c1-parked", "U1", tc.phase)
			p, _, _, natsRec, _ := newPipeline(t, ch, ac, parked)

			deliverFrom(t, p, ch, "still there?")

			assert.Len(t, unavailableNotices(t, natsRec), 1,
				"a parked session has no runner to answer, so the stall must be explained")
		})
	}
}

// TestDeliver_AgentHealthUnknown_StaysQuiet pins the fail-open half of the
// health check. Neither of these is an outage, and reporting one would be worse
// than reporting nothing: a cold-start install would greet its first user with
// "this agent is unavailable", and an apiserver hiccup would do the same to a
// perfectly healthy agent.
func TestDeliver_AgentHealthUnknown_StaysQuiet(t *testing.T) {
	t.Run("class not yet reconciled (no Valid condition): the ordinary cold-start window", func(t *testing.T) {
		ch := newChannel("c1")
		unreconciled := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		}
		p, _, _, natsRec, _ := newPipeline(t, ch, unreconciled)

		deliverFrom(t, p, ch, "hello")

		assert.Empty(t, unavailableNotices(t, natsRec), "an unjudged agent is not a diagnosed outage")
		assert.Empty(t, monitoringEvents(t, natsRec), "and must not alert")
	})

	t.Run("channel binds no agent class at all: nothing to judge", func(t *testing.T) {
		ch := newChannel("c1")
		ch.Spec.AgentClass = ""
		p, _, _, natsRec, _ := newPipeline(t, ch)

		deliverFrom(t, p, ch, "hello")

		assert.Empty(t, unavailableNotices(t, natsRec), "no bound agent means no agent-health claim to make")
	})
}

// TestDeliver_AgentClassReadFails_StaysQuiet is the third fail-open case, and
// the one most likely to fire in anger: an apiserver blip must not tell a user
// their agent is broken. Distinguished from NotFound, which is a real and
// permanent diagnosis and does report.
func TestDeliver_AgentClassReadFails_StaysQuiet(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired")
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "add scheme")
	require.NoError(t, corev1.AddToScheme(scheme), "add corev1")
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ch, ac).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isClass := obj.(*spiceboxv1alpha1.AgentClass); isClass {
					return apierrors.NewServiceUnavailable("apiserver having a moment")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	p, _, _, natsRec := newPipelineOn(t, cli)

	deliverFrom(t, p, ch, "hello")

	assert.Empty(t, unavailableNotices(t, natsRec),
		"a read failure is not a diagnosis; the agent here is in fact broken, but we cannot know that")
	assert.Empty(t, monitoringEvents(t, natsRec), "and must not alert on one")
}

// TestSurfaceUnhealthyAgent_NoNATS covers the degenerate wiring. channelsd
// always wires a bus, but the guard exists so a partially-constructed Pipeline
// in some future caller reports nothing rather than panicking on a nil publish.
func TestSurfaceUnhealthyAgent_NoNATS(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired")
	p, _, _, _, _ := newPipeline(t, ch, ac)
	p.NATS = nil

	assert.NotPanics(t, func() {
		p.publishUnhealthyAgentMonitoring(context.Background(),
			channelkinds.InboundEvent{Channel: ch},
			unhealthyAgent{namespace: "default", name: "ac1", reason: spiceboxv1alpha1.ReasonAgentIdentityInvalid},
			identity.CanonicalFromTrusted("YWxpY2U", "test fixture"))
	}, "a nil bus must be a no-op, not a crash on the inbound path")
}

// TestDeliver_MonitoringPublishFailure_DoesNotBreakDelivery mirrors the notice
// case for the operator half: the alert is commentary on an already-accepted
// message and must not be able to take it down.
func TestDeliver_MonitoringPublishFailure_DoesNotBreakDelivery(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired")
	p, _, _, natsRec, cli := newPipeline(t, ch, ac)
	natsRec.failSubjectSuffix = channelevents.MonitoringEventSubject

	dec := deliverFrom(t, p, ch, "hello")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "delivery survives a failed alert")
	assert.Len(t, unavailableNotices(t, natsRec), 1,
		"and the user is still told, since the two publishes are independent")
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions), "List sessions")
	assert.Len(t, sessions.Items, 1, "session still created")
}

// TestDeliver_UnhealthyAgent_ReAlertsWhenTheCauseChanges guards the second half
// of the dedup key. An outage that changes cause is new information — the
// remedy for a missing credential is not the remedy for an expired one — so
// collapsing it into the first alert would leave operators acting on a reason
// that no longer applies.
func TestDeliver_UnhealthyAgent_ReAlertsWhenTheCauseChanges(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired")
	p, _, _, natsRec, cli := newPipeline(t, ch, ac)

	deliverFrom(t, p, ch, "first")
	require.Len(t, monitoringEvents(t, natsRec), 1, "first cause alerts")

	// Same agent, still down, DIFFERENT cause.
	setValid(t, cli, metav1.ConditionFalse, spiceboxv1alpha1.ReasonSecretMissing)
	deliverFrom(t, p, ch, "second")

	events := monitoringEvents(t, natsRec)
	require.Len(t, events, 2, "a changed cause is new information and must alert again")
	assert.Equal(t, spiceboxv1alpha1.ReasonSecretMissing, events[1].Reason, "the new cause, not the old")
}

// TestDeliver_NoticePublishFailure_DoesNotBreakDelivery pins the degradation.
// The explanation is best-effort commentary on a message that has already been
// accepted; a publish failure must not take the message down with it. It must
// also not pass silently — the user is then waiting with nothing on screen,
// which is the exact defect this whole path exists to fix.
func TestDeliver_NoticePublishFailure_DoesNotBreakDelivery(t *testing.T) {
	ch := newChannel("c1")
	ac := agentClass(t, metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired")
	p, _, _, natsRec, cli := newPipeline(t, ch, ac)
	natsRec.failSubjectSuffix = ".out." + string(channelevents.KindInteractionRequest)

	dec := deliverFrom(t, p, ch, "hello")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome,
		"the message was already accepted; a failed explanation must not un-accept it")
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions), "List sessions")
	assert.Len(t, sessions.Items, 1, "the session still exists to resume when the agent recovers")
}

// TestAgentUnavailableBody covers the reason→copy table, including the fallback
// every unlisted reason lands on. The default has to be true of EVERY way a
// class can go invalid, since new reasons arrive without visiting this table.
func TestAgentUnavailableBody(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "expired/invalid identity: names the shape of the problem",
			reason: spiceboxv1alpha1.ReasonAgentIdentityInvalid,
			want:   agentUnavailableBodies[spiceboxv1alpha1.ReasonAgentIdentityInvalid],
		},
		{
			name:   "empty credential: names the shape of the problem",
			reason: spiceboxv1alpha1.ReasonCredentialEmpty,
			want:   agentUnavailableBodies[spiceboxv1alpha1.ReasonCredentialEmpty],
		},
		{
			name:   "a reason with no row: falls back, does not render the reason",
			reason: "SomeReasonAddedLater",
			want:   agentUnavailableDefaultBody,
		},
		{
			name:   "no reason at all: falls back",
			reason: "",
			want:   agentUnavailableDefaultBody,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentUnavailableBody(tc.reason)
			assert.Equal(t, tc.want, got)
			if tc.reason != "" {
				assert.NotContains(t, got, tc.reason,
					"the machine reason is operator vocabulary and must never reach user copy")
			}
		})
	}
}

// TestUnhealthyAgentHint covers the operator remedy, whose whole value is being
// specific when it can be and honest when it cannot.
func TestUnhealthyAgentHint(t *testing.T) {
	cases := []struct {
		name  string
		agent unhealthyAgent
		want  string
	}{
		{
			name:  "invalid identity with a name: gives the exact command",
			agent: unhealthyAgent{reason: spiceboxv1alpha1.ReasonAgentIdentityInvalid, identity: "pm-tools"},
			want:  "oap identity setup pm-tools",
		},
		{
			name:  "invalid identity, no name on the spec: cannot name it, so does not pretend to",
			agent: unhealthyAgent{reason: spiceboxv1alpha1.ReasonAgentIdentityInvalid},
			want:  "the AgentClass conditions name the failing reference",
		},
		{
			name:  "missing class: says plainly that this one needs a human",
			agent: unhealthyAgent{reason: spiceboxv1alpha1.ReasonAgentClassMissing},
			want:  "will NOT resume",
		},
		{
			name:  "any other reason: the generic remedy",
			agent: unhealthyAgent{reason: spiceboxv1alpha1.ReasonConfigMapMissing},
			want:  "parked until its Valid condition clears",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, unhealthyAgentHint(tc.agent), tc.want)
		})
	}
}

// setValid rewrites the ac1 AgentClass's Valid condition in place, standing in
// for the class controller reacting to its identity breaking or being fixed.
func setValid(t *testing.T, cli client.Client, status metav1.ConditionStatus, reason string) {
	t.Helper()
	ctx := context.Background()
	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t, cli.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ac1"}, &ac), "get AgentClass")
	ac.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentClassConditionValid,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, cli.Update(ctx, &ac), "update AgentClass status")
}

// TestSurfaceUnhealthyAgent_BusTargetedDelivery_ReportsTheTargetsOwnClass
// covers the one inbound shape where the Channel an inbound arrives over is not
// the target session's own binding.
//
// An `agent` Channel is ONE conversation shared by a PAIR, and its agentClass
// is the CHILD's — so on a child -> parent delivery, health resolved from the
// Channel described the child while the message was routed into the parent.
// That was wrong in both directions at once: a broken CHILD class produced a
// notice naming an agent the reader has no problem with, and a broken PARENT
// class — the one actually stalling them — produced no notice at all, because
// nothing resolved the target's own class.
//
// Health is now asked of the TARGET SESSION's class, below correlation. The
// three rows are the three combinations that distinguishes: the child's class
// broken (silence, correctly), the parent's class broken (the notice, naming
// the parent's agent), and both healthy (silence).
func TestSurfaceUnhealthyAgent_BusTargetedDelivery_ReportsTheTargetsOwnClass(t *testing.T) {
	// parkedParent is a delegating parent whose runner has exited, so it is a
	// session the surfacing genuinely applies to (a Running one is skipped by
	// liveRunner regardless of any class).
	parkedParent := func(class string) *spiceboxv1alpha1.AgentSession {
		return &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: "parent-1", Namespace: "default"},
			Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: class},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle},
		}
	}
	// parentClass is the parent's OWN AgentClass, distinct from the "ac1" the
	// pair Channel (and so the child) carries.
	parentClass := func(status metav1.ConditionStatus, reason string) *spiceboxv1alpha1.AgentClass {
		return &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "parent-class", Namespace: "default"},
			Status: spiceboxv1alpha1.AgentClassStatus{
				Conditions: []metav1.Condition{{
					Type: spiceboxv1alpha1.AgentClassConditionValid, Status: status,
					Reason: reason, LastTransitionTime: metav1.Now(),
				}},
			},
		}
	}

	cases := []struct {
		name string
		// childClassValid is the Valid status of "ac1", which the pair Channel
		// names and the child runs on.
		childClassValid metav1.ConditionStatus
		// parentClassValid is the Valid status of the parent's own class.
		parentClassValid metav1.ConditionStatus
		wantNotices      int
	}{
		{
			name:            "the CHILD's class is broken: the parent hears nothing about an agent it does not run on",
			childClassValid: metav1.ConditionFalse, parentClassValid: metav1.ConditionTrue,
			wantNotices: 0,
		},
		{
			name:            "the PARENT's class is broken: the stall it causes is explained",
			childClassValid: metav1.ConditionTrue, parentClassValid: metav1.ConditionFalse,
			wantNotices: 1,
		},
		{
			name:            "both healthy: nothing to explain",
			childClassValid: metav1.ConditionTrue, parentClassValid: metav1.ConditionTrue,
			wantNotices: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, child := delegationPair(t, "parent-1", "child-1")
			p, az, mem, natsRec, _ := newPipeline(t, ch, child,
				parkedParent("parent-class"),
				agentClass(t, tc.childClassValid, spiceboxv1alpha1.ReasonAgentIdentityInvalid, "expired"),
				parentClass(tc.parentClassValid, spiceboxv1alpha1.ReasonAgentIdentityInvalid),
			)
			seedDelegation(t, az, "default/parent-1", "default/child-1")

			env := agentMessageSendEnvelope(t, "default", "child-1", "default", "parent-1", "which cluster?")
			require.NoError(t, p.HandleAgentMessageSend(context.Background(), env), "HandleAgentMessageSend")
			require.Len(t, mem.appends, 1,
				"the message is delivered either way; only the surfacing is at issue")

			assert.Len(t, unavailableNotices(t, natsRec), tc.wantNotices)
			if tc.wantNotices > 0 {
				// The notice is addressed by SUBJECT, so this is what proves it
				// reaches the session that is actually stalled rather than the
				// one that happens to be bound to the pair Channel.
				assert.Contains(t, natsRec.subjects,
					"ap.session.default.parent-1.out."+string(channelevents.KindInteractionRequest),
					"the notice must be published onto the stalled session's own subject")
			}
			if tc.wantNotices == 0 {
				assert.Empty(t, monitoringEvents(t, natsRec),
					"and the operators must not be alerted about an agent nobody is blocked on")
			}
		})
	}
}

// A monitoring alert raised on this path must name the TARGET's class. The
// notice going to the right session is only half of it: the operator alert is
// what sends a human to fix something, and pointing them at a healthy agent
// while the broken one keeps stalling sessions is the more expensive half of
// the same mistake.
func TestSurfaceUnhealthyAgent_BusTargetedDelivery_AlertsOnTheTargetsClass(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-1", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "parent-class"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle},
	}
	brokenParentClass := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-class", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentClassStatus{
			Conditions: []metav1.Condition{{
				Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionFalse,
				Reason: spiceboxv1alpha1.ReasonAgentIdentityInvalid, LastTransitionTime: metav1.Now(),
			}},
		},
	}
	p, az, _, natsRec, _ := newPipeline(t, ch, child, parent, brokenParentClass)
	seedDelegation(t, az, "default/parent-1", "default/child-1")

	env := agentMessageSendEnvelope(t, "default", "child-1", "default", "parent-1", "which cluster?")
	require.NoError(t, p.HandleAgentMessageSend(context.Background(), env), "HandleAgentMessageSend")

	events := monitoringEvents(t, natsRec)
	require.Len(t, events, 1, "a person is blocked on a broken agent; the operators must hear about it")
	assert.Equal(t, "parent-class", events[0].Source.Name,
		"the alert must name the agent that is actually broken, not the child's healthy one")
}
