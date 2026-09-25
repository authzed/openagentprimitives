package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

func seqSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s1"},
	}
}

// TestApplyEvent_AppendsAndProjects drives a provisioning sequence through
// applyEvent and checks both the durable log and the projected phase.
func TestApplyEvent_AppendsAndProjects(t *testing.T) {
	r := &Reconciler{LifecycleMemory: memory.NewLocal(inmem.NewBackend())}
	sess := seqSession()
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, r.applyEvent(ctx, sess, lifecyclecore.SettingsAccepted{}))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, sess.Status.Phase,
		"SettingsAccepted projects the Pending floor")

	require.NoError(t, r.applyEvent(ctx, sess, lifecyclecore.RunnerClaimed{}))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, sess.Status.Phase,
		"RunnerClaimed projects Running")

	// The folded log reflects exactly what was appended, in order.
	events, err := lifecyclekind.Events(ctx, r.LifecycleMemory, lifecycleScope(sess))
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.IsType(t, lifecyclecore.SettingsAccepted{}, events[0])
	assert.IsType(t, lifecyclecore.RunnerClaimed{}, events[1])
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, derivePhase(lifecyclecore.Fold(events)))
}

// TestApplyEvent_TerminalIsSticky proves a post-terminal event is recorded but
// does not move the phase off the terminal state.
func TestApplyEvent_TerminalIsSticky(t *testing.T) {
	r := &Reconciler{LifecycleMemory: memory.NewLocal(inmem.NewBackend())}
	sess := seqSession()
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, r.applyEvent(ctx, sess, lifecyclecore.RunnerClaimed{}))
	require.NoError(t, r.applyEvent(ctx, sess, lifecyclecore.RunnerCrash{}))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sess.Status.Phase)

	// A later WakeRequested is logged but the phase stays Failed (sticky).
	require.NoError(t, r.applyEvent(ctx, sess, lifecyclecore.WakeRequested{}))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sess.Status.Phase)

	events, err := lifecyclekind.Events(ctx, r.LifecycleMemory, lifecycleScope(sess))
	require.NoError(t, err)
	assert.Len(t, events, 3, "all three transitions are recorded for audit completeness")
}

// TestApplyEvent_NilMemoryFallback proves applyEvent still sets the projected
// phase from the CR's current phase when no durable log is wired (test
// fixtures), without panicking on the nil facade.
func TestApplyEvent_NilMemoryFallback(t *testing.T) {
	r := &Reconciler{} // LifecycleMemory nil
	sess := seqSession()
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials

	require.NoError(t, r.applyEvent(memory.WithSystemApproval(context.Background(), "test"), sess, lifecyclecore.CredsTimeout{}))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sess.Status.Phase,
		"CredsTimeout from AwaitingCredentials projects Failed even without a durable log")
}

// TestFold_CompletedRetry_RawPendingButReconciledToIdle documents the known
// same-turn provider-error-retry ordering gap and proves the CR-visible phase is
// still correct. A retry replays the SAME turn (no new user turn), so the pre- and
// post-retry terminals are both stamped at (turn, END) while the operator's
// WakeRequested — anchored at (turn, END) in the operator_post region — outranks
// both. The raw fold therefore lands on WakeRequested last and projects Pending
// even though the retry completed. The OrderKey cannot finely order this (see the
// same-turn-retry note on operatorOrderKey); reconcilePhase corrects it because
// the runner writes Idle directly and the guard refuses to demote it to a
// bootstrap Pending. The companion idle-wake case (a new user turn) still folds to
// Running, proving the key stays correct for a genuine wake.
func TestFold_CompletedRetry_RawPendingButReconciledToIdle(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	base := time.Now().UTC().Truncate(time.Second)
	const uid = "uid-retry"

	runnerKey := func(turn, block int) lifecyclekind.OrderKey {
		return lifecyclekind.OrderKey{
			Seq: channelevents.PackSeq(turn, block), Region: string(lifecyclecore.RegionRunner), SessionUID: uid,
		}
	}
	opPostKey := func(turn int) lifecyclekind.OrderKey {
		return lifecyclekind.OrderKey{
			Seq:    channelevents.PackSeq(turn, channelevents.SeqBlockEnd),
			Region: string(lifecyclecore.RegionOperatorPost), SessionUID: uid,
		}
	}
	type step struct {
		ev  lifecyclecore.Event
		at  time.Time
		key lifecyclekind.OrderKey
	}
	rawFold := func(t *testing.T, steps []step) string {
		t.Helper()
		m := memory.NewLocal(inmem.NewBackend())
		scope := memory.Scope{Kind: "session", ID: "default/retry"}
		for _, s := range steps {
			require.NoError(t, lifecyclekind.Append(ctx, m, scope, s.ev, s.at, s.key), "append %T", s.ev)
		}
		events, err := lifecyclekind.Events(ctx, m, scope)
		require.NoError(t, err, "Events")
		require.Len(t, events, len(steps))
		return derivePhase(lifecyclecore.Fold(events))
	}

	t.Run("same-turn retry: raw fold Pending, reconcilePhase corrects to Idle", func(t *testing.T) {
		// Turn 3 runs, errors, retries the SAME turn 3, and completes to Idle.
		raw := rawFold(t, []step{
			{lifecyclecore.RunnerClaimed{}, base, runnerKey(3, channelevents.SeqBlockStart)},                                      // attempt 1 claim
			{lifecyclecore.AgentWorkComplete{Kubectl: false}, base.Add(1 * time.Second), runnerKey(3, channelevents.SeqBlockEnd)}, // attempt 1 terminal
			{lifecyclecore.WakeRequested{}, base.Add(2 * time.Second), opPostKey(3)},                                              // operator retry wake -> Pending
			{lifecyclecore.RunnerClaimed{}, base.Add(3 * time.Second), runnerKey(3, channelevents.SeqBlockStart)},                 // attempt 2 claim, SAME turn
			{lifecyclecore.AgentWorkComplete{Kubectl: false}, base.Add(4 * time.Second), runnerKey(3, channelevents.SeqBlockEnd)}, // attempt 2 terminal
		})
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, raw,
			"same-turn retry raw-folds to Pending: WakeRequested outranks both same-turn terminals")

		// The runner wrote Idle directly (WriteIdle); reconcilePhase must not demote it.
		got := reconcilePhase(raw, spiceboxv1alpha1.AgentSessionPhaseIdle)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got,
			"reconcilePhase corrects the raw Pending to the runner-written Idle")
	})

	t.Run("idle-wake to a new turn: folds to Running (key stays correct)", func(t *testing.T) {
		// Turn 3 rests Idle; a NEW user message wakes turn 4, whose claim carries a
		// higher Seq than the turn-3 WakeRequested, so it folds to Running.
		raw := rawFold(t, []step{
			{lifecyclecore.RunnerClaimed{}, base, runnerKey(3, channelevents.SeqBlockStart)},
			{lifecyclecore.AgentWorkComplete{Kubectl: false}, base.Add(1 * time.Second), runnerKey(3, channelevents.SeqBlockEnd)},
			{lifecyclecore.WakeRequested{}, base.Add(2 * time.Second), opPostKey(3)},
			{lifecyclecore.RunnerClaimed{}, base.Add(3 * time.Second), runnerKey(4, channelevents.SeqBlockStart)}, // NEW turn 4
		})
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, raw,
			"idle-wake bumps memTurnIndex, so the wake claim sorts after WakeRequested -> Running")
	})
}

func TestConditionBecameTrue(t *testing.T) {
	condTrue := func() *spiceboxv1alpha1.AgentSession {
		s := seqSession()
		s.Status.Conditions = []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentSessionConditionSettingsAccepted, Status: metav1.ConditionTrue,
		}}
		return s
	}
	t.Run("absent→True is the edge", func(t *testing.T) {
		assert.True(t, conditionBecameTrue(seqSession(), condTrue(), spiceboxv1alpha1.AgentSessionConditionSettingsAccepted))
	})
	t.Run("True→True is not the edge", func(t *testing.T) {
		assert.False(t, conditionBecameTrue(condTrue(), condTrue(), spiceboxv1alpha1.AgentSessionConditionSettingsAccepted))
	})
	t.Run("nil original treats current True as the edge", func(t *testing.T) {
		assert.True(t, conditionBecameTrue(nil, condTrue(), spiceboxv1alpha1.AgentSessionConditionSettingsAccepted))
	})
	t.Run("not True now is never the edge", func(t *testing.T) {
		assert.False(t, conditionBecameTrue(nil, seqSession(), spiceboxv1alpha1.AgentSessionConditionSettingsAccepted))
	})
}
