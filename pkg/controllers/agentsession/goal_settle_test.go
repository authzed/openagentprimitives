// pkg/controllers/agentsession/goal_settle_test.go
//
// White-box Reconcile tests for the requeue an Idle goal session needs: the
// goal dispatcher records and retains a run only in its own store, so no watch
// event tells the reconciler to conclude it.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// settleGoalValidator reports a fixed verdict and admits activation.
type settleGoalValidator struct{ concluded bool }

func (v settleGoalValidator) GoalSessionConcluded(context.Context, *spiceboxv1alpha1.AgentSession) (bool, GoalRunVerdict, error) {
	return v.concluded, GoalRunUndecided, nil
}
func (settleGoalValidator) ValidateGoalSession(context.Context, *spiceboxv1alpha1.AgentSession) error {
	return nil
}
func (settleGoalValidator) ConstrainGoalSettings(context.Context, *spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.EffectiveSettings) error {
	return nil
}

func TestReconcileIdleGoalSessionPollsForDispatcherVerdict(t *testing.T) {
	const sleepAfter = 10 * time.Minute
	for _, tc := range []struct {
		name      string
		goal      bool
		concluded bool
		want      time.Duration
	}{
		{name: "running goal run left Idle: re-checked within goalSettlePoll", goal: true, want: goalSettlePoll},
		{name: "concluded goal run still Idle: re-checked within goalSettlePoll", goal: true, concluded: true, want: goalSettlePoll},
		{name: "ordinary Idle session: requeue at its sleep deadline, unchanged", want: sleepAfter - time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
			lastIdleAt := metav1.NewTime(now.Add(-time.Minute))
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{
					Name: "s1", Namespace: "default", UID: "uid-1",
					Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
				},
				Spec: spiceboxv1alpha1.AgentSessionSpec{
					Class:        "cls",
					Prompt:       spiceboxv1alpha1.PromptSource{Inline: "do the task"},
					InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "chan-1", Kind: "fake", Key: "thread-1"},
				},
				Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle, LastIdleAt: &lastIdleAt},
			}
			if tc.goal {
				sess.Spec.GoalExecution = &spiceboxv1alpha1.GoalExecutionReference{GoalID: "goal-1", OccurrenceID: "occ-1", GoalRevision: 1, ConsentDigest: "digest"}
			}
			runner := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "default"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: RunnerContainerName, Image: "runner:dev"}}},
			}
			c := buildFakeClient(t, sess, provisioningGateClass(), runner)
			r := &Reconciler{
				Client:                   c,
				APIReader:                c,
				Tokens:                   tokens.NewRegistry(),
				Memory:                   memory.NewLocal(inmem.NewBackend()),
				Now:                      func() time.Time { return now },
				DefaultSessionSleepAfter: sleepAfter,
				GoalValidator:            settleGoalValidator{concluded: tc.concluded},
			}
			r.RunnerFactory = &PodRunnerFactory{Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222"}

			ctx := memory.WithSystemApproval(context.Background(), "test")
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}})
			require.NoError(t, err, "Reconcile")
			assert.Equal(t, tc.want, res.RequeueAfter)
		})
	}
}
