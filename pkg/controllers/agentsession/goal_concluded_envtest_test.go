//go:build integration

// pkg/controllers/agentsession/goal_concluded_envtest_test.go
//
// Reconcile-level tests for a goal session whose run has ended. The dispatcher
// keeps such a session through its inspection window, and activation
// validation refuses it by design; the reconciler must still give it the
// terminal reconciles (chain-head anchoring) while never starting a runner.
package agentsession_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

var errGoalDenied = errors.New("denied")

// stubGoalValidator answers GoalSessionConcluded from fields and counts the
// activation validations, which refuse whenever deny is set.
type stubGoalValidator struct {
	concluded   bool
	verdict     agentsession.GoalRunVerdict
	deny        bool
	validations int
}

func (s *stubGoalValidator) GoalSessionConcluded(context.Context, *spiceboxv1alpha1.AgentSession) (bool, agentsession.GoalRunVerdict, error) {
	return s.concluded, s.verdict, nil
}

func (s *stubGoalValidator) ValidateGoalSession(context.Context, *spiceboxv1alpha1.AgentSession) error {
	s.validations++
	if s.deny {
		return errGoalDenied
	}
	return nil
}

func (s *stubGoalValidator) ConstrainGoalSettings(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, _ *spiceboxv1alpha1.EffectiveSettings) error {
	return s.ValidateGoalSession(ctx, sess)
}

func goalSession(name, class string) *spiceboxv1alpha1.AgentSession {
	sess := validSession(name, class)
	sess.Spec.GoalExecution = &spiceboxv1alpha1.GoalExecutionReference{
		GoalID: "goal-1", OccurrenceID: "occ-1", GoalRevision: 1, ConsentDigest: "digest",
	}
	return sess
}

func TestReconcileConcludedGoalSessionStampsChainHeads(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)
	goals := &stubGoalValidator{}
	r.GoalValidator = goals
	var calls int
	r.AuditChainHeads = func(context.Context, memory.Scope) (map[string]provenance.ChainHead, error) {
		calls++
		return map[string]provenance.ChainHead{"session:default/gc-s": {Seq: 4, LastHash: "cccc"}}, nil
	}

	ac := validClass("gc-ac")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, goalSession("gc-s", "gc-ac")), "create AgentSession")
	reconcileToWork(t, ctx, r, "gc-s")

	// The run ends: the dispatcher records an outcome and retains the session,
	// and activation validation now refuses it.
	goals.concluded, goals.deny = true, true
	key := types.NamespacedName{Namespace: "default", Name: "gc-s"}
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &sess))
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	require.NoError(t, env.Client.Status().Update(ctx, &sess), "mark session Succeeded")

	before := goals.validations
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "reconcile of a concluded goal session must succeed")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &got))
	assert.Equal(t, map[string]string{"session:default/gc-s": "4:cccc"}, got.Status.AuditChainHeads,
		"chain heads anchored for the concluded run")
	assert.Equal(t, 1, calls, "chain heads computed once")
	assert.Equal(t, before, goals.validations, "a concluded session is not validated for activation")
}

func TestReconcileConcludedGoalSessionNeverStartsRunner(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)
	r.GoalValidator = &stubGoalValidator{concluded: true, deny: true}

	ac := validClass("gcn-ac")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, goalSession("gcn-s", "gcn-ac")), "create AgentSession")

	key := types.NamespacedName{Namespace: "default", Name: "gcn-s"}
	for range 3 {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err, "reconcile of a concluded, non-terminal goal session must succeed")
	}

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &sess))
	var pod corev1.Pod
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: agentsession.RunnerPodName(&sess)}, &pod)
	assert.True(t, apierrors.IsNotFound(err), "no runner pod for a concluded goal run; got err=%v", err)
}

func TestReconcileUnboundGoalSessionIsRefused(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)
	r.GoalValidator = &stubGoalValidator{deny: true}

	ac := validClass("gcu-ac")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, goalSession("gcu-s", "gcu-ac")), "create AgentSession")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "gcu-s"}})
	assert.ErrorIs(t, err, errGoalDenied, "a goal session that is not concluded still needs activation validation")
}

// TestReconcileConcludedGoalSessionLeftIdle drives the common way a goal run
// ends: the runner yields Idle and exits, and the dispatcher retains the run.
// Nothing else moves a concluded goal session off Idle, so the reconciler must
// finish it itself, and only when the dispatcher has proven a clean end.
func TestReconcileConcludedGoalSessionLeftIdle(t *testing.T) {
	cases := []struct {
		name       string
		verdict    agentsession.GoalRunVerdict
		held       bool
		parked     bool // the runner exited after AwaitingRetry instead of Idle
		wantPhase  string
		wantEvent  lifecyclecore.Event
		wantAnchor bool
	}{
		{name: "succeeded from Idle: Succeeded, chain heads anchored, not wakeable", verdict: agentsession.GoalRunSucceeded,
			wantPhase: spiceboxv1alpha1.AgentSessionPhaseSucceeded, wantEvent: lifecyclecore.GoalRunConcluded{}, wantAnchor: true},
		{name: "failed from Idle (no report): Failed/GoalRunFailed, chain heads anchored", verdict: agentsession.GoalRunFailed,
			wantPhase: spiceboxv1alpha1.AgentSessionPhaseFailed, wantEvent: lifecyclecore.GoalRunFailed{}, wantAnchor: true},
		{name: "succeeded verdict but parked in AwaitingRetry: Failed, never Succeeded", verdict: agentsession.GoalRunSucceeded, parked: true,
			wantPhase: spiceboxv1alpha1.AgentSessionPhaseFailed, wantEvent: lifecyclecore.GoalRunFailed{}, wantAnchor: true},
		{name: "undecided: stays Idle for the dispatcher to decide", verdict: agentsession.GoalRunUndecided,
			wantPhase: spiceboxv1alpha1.AgentSessionPhaseIdle},
		{name: "succeeded but held: Held and contained, not concluded; evidence outranks cleanup", verdict: agentsession.GoalRunSucceeded, held: true,
			wantPhase: spiceboxv1alpha1.AgentSessionPhaseHeld},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.Shared(t)
			ctx := memory.WithSystemApproval(context.Background(), "test")
			mem := memory.NewLocal(inmem.NewBackend())
			r := newReconciler(t, env)
			r.LifecycleMemory = mem
			goals := &stubGoalValidator{}
			r.GoalValidator = goals
			name := fmt.Sprintf("gci-s-%d", i)
			r.AuditChainHeads = func(context.Context, memory.Scope) (map[string]provenance.ChainHead, error) {
				return map[string]provenance.ChainHead{"session:default/" + name: {Seq: 2, LastHash: "dddd"}}, nil
			}

			ac := validClass(fmt.Sprintf("gci-ac-%d", i))
			require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
			markValid(t, env, ac)
			require.NoError(t, env.Client.Create(ctx, goalSession(name, ac.Name)), "create AgentSession")
			reconcileToWork(t, ctx, r, name)

			// The runner yields and exits; the dispatcher records the outcome.
			key := types.NamespacedName{Namespace: "default", Name: name}
			var sess spiceboxv1alpha1.AgentSession
			require.NoError(t, env.Client.Get(ctx, key, &sess))
			seedLifecycleEvent(t, mem, sess.Namespace, sess.Name, string(sess.UID),
				1, channelevents.SeqBlockEnd, lifecyclecore.IdleYield{})
			sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
			if tc.parked {
				sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry
			}
			require.NoError(t, env.Client.Status().Update(ctx, &sess), "mark the phase the runner left")
			goals.concluded, goals.verdict, goals.deny = true, tc.verdict, true
			if tc.held {
				require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.SessionHold{
					ObjectMeta: metav1.ObjectMeta{Name: name + "-hold", Namespace: "default"},
					Spec: spiceboxv1alpha1.SessionHoldSpec{
						SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: name},
						Reason:     "manual review",
						Source:     "test",
					},
				}), "create SessionHold")
			}

			for range 2 {
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				require.NoError(t, err, "reconcile of a concluded goal session must succeed")
			}

			if tc.held {
				var hold spiceboxv1alpha1.SessionHold
				require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: name + "-hold"}, &hold))
				assert.NotNil(t, hold.Status.TrippedAt, "the hold on a concluded goal session is processed, not skipped")
				var held spiceboxv1alpha1.AgentSession
				require.NoError(t, env.Client.Get(ctx, key, &held))
				err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: agentsession.RunnerPodName(&held)}, &corev1.Pod{})
				assert.True(t, apierrors.IsNotFound(err), "the hold reaps the runner, so the dispatcher can release capacity; got %v", err)
			}
			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, env.Client.Get(ctx, key, &got))
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			assert.False(t, spiceboxv1alpha1.ArchivedBySweep(&got), "a goal run is never parked as wakeable")
			if !tc.wantAnchor {
				assert.Nil(t, got.Status.AuditChainHeads, "no anchor before the run is terminal")
				return
			}
			assert.Equal(t, map[string]string{"session:default/" + name: "2:dddd"}, got.Status.AuditChainHeads,
				"chain heads anchored for the concluded run")
			assert.NotNil(t, got.Status.FinishedAt)
			assert.Equal(t, tc.wantPhase, foldPhase(t, mem, sess.Namespace, sess.Name),
				"the signed lifecycle log folds to the same terminal phase")
			events, err := lifecyclekind.Events(ctx, mem, lifecycleScopeFor(sess.Namespace, sess.Name))
			require.NoError(t, err)
			recorded := 0
			for _, ev := range events {
				if fmt.Sprintf("%T", ev) == fmt.Sprintf("%T", tc.wantEvent) {
					recorded++
				}
			}
			assert.Equal(t, 1, recorded, "%T recorded once across reconciles", tc.wantEvent)
		})
	}
}
