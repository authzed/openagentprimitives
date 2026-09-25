package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// incarnation returns an AgentSession at the fixed namespace/name a webhook
// trigger derives deterministically, distinguished only by uid — the shape a
// delete-and-redeliver produces.
func incarnation(t *testing.T, uid string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "demo-reviewbot-gh-0932",
			UID:       types.UID(uid),
		},
	}
}

// TestFold_RetryAfterDeleteDoesNotInheritThePriorTerminal is the regression for
// a redelivered webhook.
//
// A trigger derives one session name per pull request, so a redelivery — or a
// later push — re-creates the session under the name the failed attempt used.
// The durable lifecycle log is keyed by that name and, being append-only, is
// PERMANENT: it outlives the CR and its deletion by design. The fresh CR (new
// uid, empty status) must therefore fold to the bootstrap floor and run, not
// adopt the terminal event of the instance it replaced.
func TestFold_RetryAfterDeleteDoesNotInheritThePriorTerminal(t *testing.T) {
	r := &Reconciler{LifecycleMemory: memory.NewLocal(inmem.NewBackend())}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Attempt 1 fails and the CR is deleted; its signed log stays in the scope.
	failed := incarnation(t, "uid-attempt-1")
	require.NoError(t, r.applyEvent(ctx, failed, lifecyclecore.RunnerClaimed{}))
	require.NoError(t, r.applyEvent(ctx, failed, lifecyclecore.RunnerCrash{}))
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, failed.Status.Phase,
		"precondition: the first attempt reached a terminal phase")

	// The redelivery creates a genuinely new object: same name, new uid, no status.
	retried := incarnation(t, "uid-attempt-2")

	state, err := r.foldLifecycle(ctx, retried)
	require.NoError(t, err, "fold the shared scope on behalf of the new instance")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, derivePhase(state),
		"a session that has never taken a turn folds to Pending, not the deleted instance's Failed")

	require.NoError(t, r.applyEvent(ctx, retried, lifecyclecore.SettingsAccepted{}))
	require.NoError(t, r.applyEvent(ctx, retried, lifecyclecore.RunnerClaimed{}))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, retried.Status.Phase,
		"the retry actually runs instead of exiting on an inherited terminal")
}

// TestFold_SameInstanceStillContinues is the regression direction that matters
// as much: a live session keeps folding its own events, so a second push to the
// same pull request continues the review rather than starting a second one.
func TestFold_SameInstanceStillContinues(t *testing.T) {
	r := &Reconciler{LifecycleMemory: memory.NewLocal(inmem.NewBackend())}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	sess := incarnation(t, "uid-attempt-1")
	require.NoError(t, r.applyEvent(ctx, sess, lifecyclecore.RunnerClaimed{}))
	require.NoError(t, r.applyEvent(ctx, sess, lifecyclecore.AgentWorkComplete{Kubectl: false}))
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, sess.Status.Phase)

	// A fresh Reconciler with no in-process state — an operator bounce — re-folds
	// the SAME instance's log and reconstructs its phase.
	bounced := &Reconciler{LifecycleMemory: r.LifecycleMemory}
	state, err := bounced.foldLifecycle(ctx, incarnation(t, "uid-attempt-1"))
	require.NoError(t, err)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, derivePhase(state),
		"the instance that wrote the log still folds it")
}

// TestFold_UnstampedLogStillFolds keeps an in-flight upgrade safe: entries
// written before the ordering key carried an instance identity have none, and
// dropping them would demote a live session mid-upgrade.
func TestFold_UnstampedLogStillFolds(t *testing.T) {
	r := &Reconciler{LifecycleMemory: memory.NewLocal(inmem.NewBackend())}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// A session with no uid at all stamps no uid on what it appends.
	legacy := incarnation(t, "")
	require.NoError(t, r.applyEvent(ctx, legacy, lifecyclecore.RunnerClaimed{}))

	state, err := r.foldLifecycle(ctx, incarnation(t, "uid-after-upgrade"))
	require.NoError(t, err)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, derivePhase(state),
		"unattributed events still fold for whichever instance reads them")
}
