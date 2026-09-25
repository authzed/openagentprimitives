package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// retryTTLScheme builds a runtime.Scheme with AgentSession registered.
func retryTTLScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

// newTTLReconciler builds a Reconciler for TTL tests with a frozen clock and
// an in-memory lifecycle store.
func newTTLReconciler(t *testing.T, now time.Time, sessions ...spiceboxv1alpha1.AgentSession) (*Reconciler, *memory.Local, client.Client) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	b := fakeclient.NewClientBuilder().WithScheme(retryTTLScheme(t))
	for i := range sessions {
		o := sessions[i]
		b = b.WithObjects(&o).WithStatusSubresource(&o)
	}
	cli := b.Build()
	r := &Reconciler{
		Client:          cli,
		LifecycleMemory: mem,
		Now:             func() time.Time { return now },
	}
	return r, mem, cli
}

// awaitingRetrySess returns an AgentSession in AwaitingRetry phase.
func awaitingRetrySess(name string) spiceboxv1alpha1.AgentSession {
	return spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:         spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry,
			RetryAttempts: 1,
		},
	}
}

// TestCheckRetryTTL_FirstObservation_StampsAnnotationAndRequeues verifies
// that the first reconcile of an AwaitingRetry session (no annotation yet)
// stamps AnnotationAwaitingRetrySince and returns RequeueAfter = full TTL.
func TestCheckRetryTTL_FirstObservation_StampsAnnotationAndRequeues(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	sess := awaitingRetrySess("sess")
	r, _, cli := newTTLReconciler(t, now, sess)

	sessPtr := sess.DeepCopy()
	done, result, err := r.checkRetryTTL(memory.WithSystemApproval(context.Background(), "test"), sessPtr)
	require.NoError(t, err)
	assert.True(t, done, "first observation must return done=true to halt the reconcile path")
	assert.Equal(t, defaultRetryTTL, result.RequeueAfter,
		"requeue-after must equal the full TTL on first observation")

	// Verify the annotation was persisted to the fake store.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(memory.WithSystemApproval(context.Background(), "test"),
		types.NamespacedName{Name: "sess", Namespace: "default"}, &got))
	assert.NotEmpty(t, got.Annotations[spiceboxv1alpha1.AnnotationAwaitingRetrySince],
		"awaiting-retry-since annotation must be written to the CR")
}

// TestCheckRetryTTL_WithinWindow_RequeuesWithRemainder verifies that a
// session within the TTL window gets a requeue-after of the remaining time.
func TestCheckRetryTTL_WithinWindow_RequeuesWithRemainder(t *testing.T) {
	entry := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	elapsed := 10 * time.Minute
	now := entry.Add(elapsed)

	sess := awaitingRetrySess("sess")
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationAwaitingRetrySince: entry.UTC().Format(time.RFC3339Nano),
	}
	r, _, _ := newTTLReconciler(t, now, sess)

	sessPtr := sess.DeepCopy()
	done, result, err := r.checkRetryTTL(memory.WithSystemApproval(context.Background(), "test"), sessPtr)
	require.NoError(t, err)
	assert.True(t, done, "within-window path must return done=true (requeue controls timing)")
	expected := defaultRetryTTL - elapsed
	assert.Equal(t, expected, result.RequeueAfter,
		"requeue-after must be the remaining window, not the full TTL")
}

// TestCheckRetryTTL_TTLElapsed_EmitsEventAndFails verifies that once the TTL
// window has elapsed, checkRetryTTL emits RetryTTLExpired and fails the session.
func TestCheckRetryTTL_TTLElapsed_EmitsEventAndFails(t *testing.T) {
	entry := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	now := entry.Add(defaultRetryTTL + time.Second) // 1 second past the TTL

	sess := awaitingRetrySess("sess")
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationAwaitingRetrySince: entry.UTC().Format(time.RFC3339Nano),
	}
	r, mem, _ := newTTLReconciler(t, now, sess)

	sessPtr := sess.DeepCopy()
	done, result, err := r.checkRetryTTL(memory.WithSystemApproval(context.Background(), "test"), sessPtr)
	require.NoError(t, err)
	assert.True(t, done, "TTL-elapsed path must return done=true")
	assert.Zero(t, result.RequeueAfter, "terminal transition: no requeue-after")

	// Phase must be Failed with the RetryTimeout reason.
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sessPtr.Status.Phase,
		"session must transition to Failed after TTL expiry")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionRetryTimeout, sessPtr.Status.FailureReason,
		"failure reason must be RetryTimeout")
	assert.NotNil(t, sessPtr.Status.FinishedAt, "finishedAt must be stamped on terminal transition")

	// RetryTTLExpired must appear in the lifecycle log.
	events, err := lifecyclekind.Events(memory.WithSystemApproval(context.Background(), "test"), mem, lifecycleScope(sessPtr))
	require.NoError(t, err)
	found := false
	for _, e := range events {
		if _, ok := e.(lifecyclecore.RetryTTLExpired); ok {
			found = true
		}
	}
	assert.True(t, found, "RetryTTLExpired must be recorded in the lifecycle log")
}

// TestCheckRetryTTL_BudgetExhaustedInFold_AppliesTerminalPhase verifies that
// if the lifecycle fold shows PhaseFailed (6 ProviderErrors, past maxRetry=5),
// checkRetryTTL applies the terminal phase even though the CR shows AwaitingRetry.
func TestCheckRetryTTL_BudgetExhaustedInFold_AppliesTerminalPhase(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	sess := awaitingRetrySess("sess")
	r, mem, _ := newTTLReconciler(t, now, sess)

	// Seed 6 ProviderError events — one past the maxRetry=5 cap.
	ctx := memory.WithSystemApproval(context.Background(), "test")
	scope := lifecycleScope(&spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess", Namespace: "default"},
	})
	for i := 0; i < 6; i++ {
		require.NoError(t, lifecyclekind.Append(ctx, mem, scope, lifecyclecore.ProviderError{}, now, lifecyclekind.OrderKey{}),
			"seed ProviderError event %d", i+1)
	}

	sessPtr := sess.DeepCopy()
	done, result, err := r.checkRetryTTL(ctx, sessPtr)
	require.NoError(t, err)
	assert.True(t, done, "budget-exhausted fold path must return done=true")
	assert.Zero(t, result.RequeueAfter, "terminal phase: no requeue-after")

	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sessPtr.Status.Phase,
		"fold must project PhaseFailed when budget is exhausted")
	assert.Equal(t, "RetryBudgetExhausted", sessPtr.Status.FailureReason,
		"failure reason must match the lifecycle fold")
}
