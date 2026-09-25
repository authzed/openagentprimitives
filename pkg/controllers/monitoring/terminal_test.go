package monitoring

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// terminal_test.go pins the distinction between a LIVE-HEALTH condition and a
// TERMINAL one.
//
// detectTransition treats a first observation that is already failing as a
// none→failed transition, so the operator re-announces open problems once on
// restart and a failure that began during downtime is not missed. That is right
// for every live-health row in the table — Refresh, Valid, Connected, PinDrift
// all flip back to good when the underlying problem is fixed, so the trigger
// clears itself and the re-announce is bounded.
//
// AgentSession/Failed is the one terminal row. Every writer of that condition
// sets it True (runner status, the agentsession controller's failure paths,
// identity choice, expiration, toolcall), nothing ever sets it False or removes
// it, and the CR is deliberately kept after the session's pods are reaped so
// its status stays available for debugging. There is no session-CR GC. So
// failed sessions accumulate for the life of the cluster, and a cold first
// observation of one is history, not an open problem — re-announcing it to the
// monitoring channel on every operator restart is pure noise that buries the
// live incidents the channel exists to surface.

func agentSessionTarget(t *testing.T) Target {
	t.Helper()
	for _, tg := range Targets() {
		if tg.GVKName == "AgentSession" {
			return tg
		}
	}
	t.Fatal("AgentSession target missing")
	return Target{}
}

// failedSession is a session that failed at some point in the past and, like
// every failed session, still carries Failed=True.
func failedSession(name string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
	}
	s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
	s.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentSessionConditionFailed,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAgentSessionBundleFail,
		Message:            "bundle failed",
		LastTransitionTime: metav1.Now(),
	}}
	return s
}

func TestReconciler_ColdObservationOfTerminalFailureIsSilent(t *testing.T) {
	rec := &recorder{}
	sess := failedSession("s1")
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: agentSessionTarget(t), tracker: newTracker()}

	// The operator restarts: the informer LISTs and enqueues an Add for every
	// existing AgentSession, so each one reconciles with an empty tracker.
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: "s1"},
	})
	require.NoError(t, err)

	assert.Empty(t, rec.snapshot(),
		"an already-failed session observed cold is a past incident, not an open "+
			"problem — re-announcing it floods the monitoring channel on every operator restart")
}

func TestReconciler_LiveFailureTransitionStillEmits(t *testing.T) {
	rec := &recorder{}
	sess := failedSession("s1")
	// The session is healthy when the operator first sees it...
	sess.Status.Conditions[0].Status = metav1.ConditionFalse
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: agentSessionTarget(t), tracker: newTracker()}
	req := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: "s1"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Empty(t, rec.snapshot())

	// ...and then fails while the operator is watching. Suppressing the cold
	// re-announce must NOT suppress the real thing: a session failing now is
	// exactly what the monitoring channel is for.
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sess), sess))
	sess.Status.Conditions[0].Status = metav1.ConditionTrue
	require.NoError(t, c.Status().Update(context.Background(), sess))
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	assert.Len(t, rec.snapshot(), 1, "a live none→failed transition must still be announced")
}

func TestReconciler_ColdObservationOfLiveHealthFailureStillEmits(t *testing.T) {
	rec := &recorder{}
	ai := aiWithRefresh(metav1.ConditionFalse, "TokenEndpointError")
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ai).
		WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: agentIdentityTarget(t), tracker: newTracker()}

	reconcileAI(t, r)

	assert.Len(t, rec.snapshot(), 1,
		"a live-health condition that is still failing IS an open problem — the "+
			"restart re-announce must survive for every non-terminal row")
}

func TestTargets_OnlyTerminalConditionsAreMarkedTerminal(t *testing.T) {
	for _, tg := range Targets() {
		for _, rule := range tg.Rules {
			terminal := tg.GVKName == "AgentSession" &&
				rule.ConditionType == spiceboxv1alpha1.AgentSessionConditionFailed
			assert.Equal(t, terminal, rule.Terminal,
				"%s/%s: Terminal must be set iff the condition never recovers",
				tg.GVKName, rule.ConditionType)
		}
	}
}
