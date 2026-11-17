package goals

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

// The operator half of a blocked group run: a group run whose leakage card went unanswered past the
// terms' ApprovalSeconds reports blocked and parks Idle (the runner half,
// pkg/agent/runner TestGroupRunApprovalExpiryReportsBlocked). The dispatcher
// then ends the occurrence with that blocked report, and the group's members
// and managers see it in the goal's run history (list_goal_runs).
func TestGroupRunBlockedByExpiredApprovalShowsInRuns(t *testing.T) {
	ctx := context.Background()
	f := newGroupRunFixture(t)
	g, o := f.approve(t)
	d := f.dispatcher()
	sess := f.started(t, d, o)

	// What the runner reports when the leakage approval expires undecided.
	blocked := domain.RunProposal{
		RequestID:   "approval-expired-req-leak-1",
		Status:      "blocked",
		Summary:     "Blocked: the information-sharing approval this run asked for (request req-leak-1) received no decision before its deadline, so the run stopped.",
		Evidence:    []string{"approval request req-leak-1"},
		SubmittedAt: *f.clock,
	}
	_, err := d.RecordGoalResult(ctx, sess, blocked)
	require.NoError(t, err)

	// The runner then parks Idle as agent_work_complete does, and exits.
	sess.Status.Phase = "Idle"
	require.NoError(t, f.k8s.Update(ctx, sess))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: sess.Namespace, Name: agentsession.RunnerPodName(sess), OwnerReferences: []metav1.OwnerReference{{UID: sess.UID}}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	require.NoError(t, f.k8s.Create(ctx, pod))
	*f.clock = f.clock.Add(time.Duration(g.Execution.Terms.Bounds.ApprovalSeconds+6) * time.Second)
	require.NoError(t, d.Tick(ctx))

	for _, viewer := range []string{"bob", "alice"} {
		page, err := f.svc.Runs(ctx, groupRunActor(viewer), g.ID, domain.ListRequest{})
		require.NoError(t, err, viewer)
		require.Len(t, page.Runs, 1, viewer)
		run := page.Runs[0]
		require.NotNil(t, run.Outcome, viewer)
		assert.Equal(t, domain.RunSessionEnded, run.Outcome.Reason, "%s: the run ended on its own report", viewer)
		require.NotNil(t, run.Proposal, viewer)
		assert.Equal(t, "blocked", run.Proposal.Status, "%s sees the run's outcome as blocked", viewer)
		assert.Contains(t, run.Proposal.Summary, "information-sharing approval", viewer)
	}
}
