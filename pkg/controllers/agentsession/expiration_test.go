// pkg/controllers/agentsession/expiration_test.go
//
// White-box unit tests (fake client + injected clock) for reconcileExpiration:
// a non-terminal AgentSession whose wall-clock lifetime (now - status.StartedAt)
// exceeds budget.sessionExpiration is transitioned to phase=Failed
// (SessionExpired), even while Idle/asleep. Mirrors sleep_reconcile_test.go's
// fixture pattern; reconcileExpiration is exercised directly (white-box)
// rather than through the full Reconcile.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// expirationFixture wires a reconciler over a fake client pre-loaded with an
// Idle AgentSession whose Status.StartedAt and
// Status.EffectiveSettings.Budget.SessionExpiration are set relative to the
// fixture's injected clock.
type expirationFixture struct {
	r    *Reconciler
	c    client.Client
	sess *spiceboxv1alpha1.AgentSession
	now  time.Time
}

// newExpirationFixture builds an Idle AgentSession started startedDelta
// relative to the fixture's injected clock (e.g. -time.Hour = started an hour
// ago) with the given session-expiration cap (0 = no cap). startedAt nil
// (startedDelta == noStartedAt) leaves Status.StartedAt unset.
const noStartedAt = time.Duration(1<<63 - 1) // sentinel: "leave StartedAt nil"

func newExpirationFixture(t *testing.T, startedDelta time.Duration, expiration time.Duration) expirationFixture {
	t.Helper()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "cls"},
	}
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
		EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{
			Budget: spiceboxv1alpha1.BudgetConfig{
				SessionExpiration: metav1.Duration{Duration: expiration},
			},
		},
	}
	if startedDelta != noStartedAt {
		startedAt := metav1.NewTime(now.Add(startedDelta))
		sess.Status.StartedAt = &startedAt
	}

	c := buildFakeClient(t, sess)
	r := &Reconciler{
		Client:    c,
		APIReader: c,
		Tokens:    tokens.NewRegistry(),
		Memory:    memory.NewLocal(inmem.NewBackend()),
		Now:       func() time.Time { return now },
	}
	return expirationFixture{r: r, c: c, sess: sess, now: now}
}

func (f expirationFixture) getSession(t *testing.T) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(memory.WithSystemApproval(context.Background(), "test"), client.ObjectKey{Namespace: "default", Name: "s1"}, &got))
	return &got
}

func TestReconcileExpirationPastDeadlineTransitionsToFailed(t *testing.T) {
	f := newExpirationFixture(t, -time.Hour, 30*time.Minute) // started 1h ago, cap 30m: expired

	transitioned, requeue, err := f.r.reconcileExpiration(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.True(t, transitioned, "past-deadline session should transition to Failed")
	assert.Equal(t, time.Duration(0), requeue)

	got := f.getSession(t)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionExpired, got.Status.FailureReason)
	assert.NotNil(t, got.Status.FinishedAt, "FinishedAt stamped")
}

func TestReconcileExpirationBeforeDeadlineRequeues(t *testing.T) {
	f := newExpirationFixture(t, -10*time.Minute, time.Hour) // started 10m ago, cap 1h: ~50m left

	transitioned, requeue, err := f.r.reconcileExpiration(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.False(t, transitioned, "before-deadline session should not transition")
	assert.Greater(t, requeue, 40*time.Minute)

	got := f.getSession(t)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "unchanged")
}

func TestReconcileExpirationNoCapIsNoOp(t *testing.T) {
	f := newExpirationFixture(t, -time.Hour, 0) // no cap set, despite being started long ago

	transitioned, requeue, err := f.r.reconcileExpiration(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.False(t, transitioned)
	assert.Equal(t, time.Duration(0), requeue)
}

func TestReconcileExpirationNilStartedAtIsNoOp(t *testing.T) {
	f := newExpirationFixture(t, noStartedAt, 30*time.Minute) // cap set, but never started

	transitioned, requeue, err := f.r.reconcileExpiration(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.False(t, transitioned)
	assert.Equal(t, time.Duration(0), requeue)
}
