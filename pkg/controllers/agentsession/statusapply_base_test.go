// pkg/controllers/agentsession/statusapply_base_test.go
//
// Unit tests for applyStatus's diff BASE — the question of which snapshot the
// next status write is measured against. A reconcile calls applyStatus several
// times in one pass, and agentstatus.WriteOwned sends non-condition fields as a
// resourceVersion-free merge patch, so any field a later call re-sends
// unconditionally overwrites whatever a concurrent writer (the runner) put
// there in between. These tests pin the two halves of the contract: the write
// base advances after every successful write, while the edge-detection snapshot
// does not.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// newStatusBaseFixture builds a one-session fake cluster plus a Reconciler over
// it, and returns the reconcile-scoped context Reconcile itself installs (the
// start-of-reconcile snapshot) along with the live object a reconcile would be
// mutating.
func newStatusBaseFixture(t *testing.T) (context.Context, client.Client, *agentsession.Reconciler, *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
	}
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithObjects(sess).
		Build()

	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sess), &live),
		"read the session the way Reconcile does")

	ctx := agentsession.WithReconcileOriginalForTest(context.Background(), live.DeepCopy())
	return ctx, c, &agentsession.Reconciler{Client: c}, &live
}

// serverPhase reads the phase currently stored on the API server.
func serverPhase(t *testing.T, c client.Client, sess *spiceboxv1alpha1.AgentSession) string {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sess), &got))
	return got.Status.Phase
}

// TestApplyStatus_SecondWriteDoesNotRevertAConcurrentWriter is the regression
// test for the refusal-recovery wedge: a reconcile persisted Phase=Pending,
// started the runner, the runner hit a provider refusal and patched
// Phase=AwaitingRetry, and the reconcile's NEXT applyStatus re-sent Pending —
// because it was still diffing against the reconcile-start snapshot, where
// phase was "". The runner had already exited, so nothing moved the phase
// again: the session sat in Pending forever and the user never got the Retry
// button.
func TestApplyStatus_SecondWriteDoesNotRevertAConcurrentWriter(t *testing.T) {
	ctx, c, r, live := newStatusBaseFixture(t)

	// First write of the reconcile: "" → Pending, exactly what the settings
	// gate's applyEvent(SettingsAccepted) + applyStatus pair does.
	live.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
	require.NoError(t, r.ApplyStatusForTest(ctx, live), "first status write")
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, serverPhase(t, c, live),
		"precondition: the first write must persist Pending")

	// The runner starts, refuses, and parks the session — a status write by a
	// different writer, landing while the same reconcile is still in flight.
	var byRunner spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(live), &byRunner))
	prior := byRunner.DeepCopy()
	byRunner.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry
	byRunner.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionRefusal
	require.NoError(t, c.Status().Patch(ctx, &byRunner, client.MergeFrom(prior)),
		"runner parks the session in AwaitingRetry")

	// Second write of the SAME reconcile (the post-RunnerFactory.Start
	// runner-creating write). It changes only the pod name; phase is untouched
	// since the first write, so phase must not appear in the patch at all.
	live.Status.RunnerPodName = "demo-session-runner"
	require.NoError(t, r.ApplyStatusForTest(ctx, live), "second status write")

	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, serverPhase(t, c, live),
		"a field the reconcile already persisted must not be re-sent over a concurrent writer's newer value")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(live), &got))
	assert.Equal(t, "demo-session-runner", got.Status.RunnerPodName,
		"the second write must still land the field it actually changed")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionRefusal, got.Status.FailureReason,
		"the runner's companion fields must survive too")
}

// TestApplyStatus_ReconcileOriginalStaysAtReconcileStart pins the other half of
// the contract: advancing the write base must NOT advance the edge-detection
// snapshot. conditionBecameTrue and the AwaitingCredentials entry check ask
// "was this already true when I started?", and folding an intervening write
// into that answer would silently stop emitting once-per-transition lifecycle
// events.
func TestApplyStatus_ReconcileOriginalStaysAtReconcileStart(t *testing.T) {
	ctx, _, r, live := newStatusBaseFixture(t)

	live.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
	require.NoError(t, r.ApplyStatusForTest(ctx, live), "status write")

	original := agentsession.ReconcileOriginalForTest(ctx)
	require.NotNil(t, original, "the reconcile snapshot must still be installed")
	assert.Empty(t, original.Status.Phase,
		"the start-of-reconcile snapshot must still report the phase as read, not as written")
}
