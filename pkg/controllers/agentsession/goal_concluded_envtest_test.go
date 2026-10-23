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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

var errGoalDenied = errors.New("denied")

// stubGoalValidator answers GoalSessionConcluded from a field and counts the
// activation validations, which refuse whenever deny is set.
type stubGoalValidator struct {
	concluded   bool
	deny        bool
	validations int
}

func (s *stubGoalValidator) GoalSessionConcluded(context.Context, *spiceboxv1alpha1.AgentSession) (bool, error) {
	return s.concluded, nil
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
