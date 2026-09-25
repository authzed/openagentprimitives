package agentsession

// terminal_log_dedup_test.go proves that failure-path event sites
// are edge-gated: calling markBootFailed (or a passthrough CredsTimeout site)
// on a session that is already in phase=Failed must NOT re-append the same
// transition event to the signed lifecycle log. The event must appear exactly
// once no matter how many times the reconcile loop revisits the terminal session.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// failureSession is a minimal AgentSession for failure-path dedup tests.
func failureSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fail-dedup"},
	}
}

// failureReconciler builds a Reconciler with both a fake k8s client (so
// applyStatus can do its Get + Patch writes) and a LifecycleMemory backend (so
// the signed lifecycle log is durable and inspectable).
func failureReconciler(t *testing.T, sess *spiceboxv1alpha1.AgentSession) *Reconciler {
	t.Helper()
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	return &Reconciler{
		Client:          c,
		APIReader:       c,
		LifecycleMemory: memory.NewLocal(inmem.NewBackend()),
	}
}

// TestMarkBootFailed_LogAppendedOnce proves that markBootFailed appends the
// ProvablyUnschedulable transition event exactly once. On the first call the
// session is not yet terminal, so the event is appended and phase=Failed is
// projected. On the second call (same session, now already Failed) the guard
// in markBootFailed must skip the applyEvent call — the log must still contain
// exactly one event after both calls complete.
func TestMarkBootFailed_LogAppendedOnce(t *testing.T) {
	sess := failureSession()
	r := failureReconciler(t, sess)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// First call: session is not yet terminal; event fires and phase becomes Failed.
	_, err := r.markBootFailed(ctx, sess, "SidecarBootFailed", "sidecar never became Ready")
	require.NoError(t, err, "first markBootFailed must succeed")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sess.Status.Phase,
		"session must be Failed after first markBootFailed")

	// Re-read the session to simulate the start of a second reconcile (the
	// controller always fetches a fresh copy of the object at reconcile entry,
	// which carries the current ResourceVersion and the now-Failed phase).
	require.NoError(t, r.Client.Get(ctx, client.ObjectKeyFromObject(sess), sess),
		"re-read session before second reconcile")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sess.Status.Phase,
		"re-read confirms phase=Failed is persisted in the fake client")

	// Second call: session is already terminal; the guard must prevent re-appending.
	_, err = r.markBootFailed(ctx, sess, "SidecarBootFailed", "sidecar never became Ready")
	require.NoError(t, err, "second markBootFailed (already Failed) must succeed")

	// The signed lifecycle log must contain the ProvablyUnschedulable event exactly
	// once — unbounded growth on repeated reconciles of a terminal session is the
	// bug this test pins.
	events, err := lifecyclekind.Events(ctx, r.LifecycleMemory, lifecycleScope(sess))
	require.NoError(t, err)
	require.Len(t, events, 1,
		"ProvablyUnschedulable must be logged exactly once, not once per reconcile")
	assert.IsType(t, lifecyclecore.ProvablyUnschedulable{}, events[0])
}
