package goals

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
)

// acceptingRuns stands in for the dispatcher: the run session is valid, and
// results and terms are recorded or answered without further checks, so these
// tests see only what the server itself asks.
type acceptingRuns struct {
	proposals []core.RunProposal
}

func (*acceptingRuns) ValidateGoalSession(context.Context, *v1.AgentSession) error { return nil }
func (a *acceptingRuns) RecordGoalResult(_ context.Context, _ *v1.AgentSession, p core.RunProposal) (core.Occurrence, error) {
	a.proposals = append(a.proposals, p)
	return core.Occurrence{ID: "recorded"}, nil
}

// groupRunServer is newGroupRun with the run session served over HTTP under
// its own token. The run session carries no started-by annotation, as the
// dispatcher creates it.
func groupRunServer(t *testing.T) (*groupRun, *acceptingRuns) {
	t.Helper()
	r := newGroupRun(t)
	runs := &acceptingRuns{}
	r.c.f.s.ExecutionSessions = runs
	o, err := r.c.store.Occurrence(operatorCtx(), r.sess.Spec.GoalExecution.OccurrenceID)
	require.NoError(t, err)
	r.sess.Spec.GoalExecution = &v1.GoalExecutionReference{GoalID: o.GoalID, OccurrenceID: o.ID, GoalRevision: o.GoalRevision, ConsentDigest: o.ConsentDigest}
	binding := &v1.ChannelBinding{Name: "team-chat", Kind: "grouptest", Key: "group-run:1", External: map[string]string{"group": "g1"}}
	r.sess.Spec.InputChannel, r.sess.Spec.OutputChannel = binding, binding.DeepCopy()
	require.NoError(t, r.c.f.s.Reader.(client.Client).Create(context.Background(), r.sess))
	r.c.f.s.Tokens.Set(memory.NamespacedName{Namespace: groupNS, Name: "run"}, "run-token", "")
	r.c.f.auth.interactChecks, r.c.f.auth.checkedUsers = 0, nil
	return r, runs
}

func TestGroupRunAuthorizePlanSkipsStarterInteract(t *testing.T) {
	r, _ := groupRunServer(t)
	status, res := r.c.f.callSession(t, "run-token", "run", core.Request{Operation: "authorize_plan", PlanApproval: &core.PlanApprovalRequest{Digest: r.plan.Digest(), Phase: 0}})
	require.Equal(t, 200, status)
	require.NotNil(t, res.PlanApproval, "the standing plan covers the run")
	assert.Equal(t, r.c.alice, res.PlanApproval.Approver)
	assert.Zero(t, r.c.f.auth.interactChecks, "a group run has no started-by user to check interact for")
}

func TestGroupRunReportCarriesRunSourcesWithoutActingUser(t *testing.T) {
	r, runs := groupRunServer(t)
	taint, err := json.Marshal(infoleakagetaint.TaintRecord{ResourceType: "github_repo", ResourceID: "acme/app", Permission: "read"})
	require.NoError(t, err)
	_, err = r.log.Put(memory.WithSystemApproval(context.Background(), "test"), memory.Entry{Scope: memory.Scope{Kind: "session", ID: groupNS + "/run"}, Kind: infoleakagetaint.KindName, ID: "ilt-run", CreatedAt: time.Now().UTC(), Content: taint})
	require.NoError(t, err)
	status, _ := r.c.f.callSession(t, "run-token", "run", core.Request{Operation: "report_execution_result", Proposal: core.RunProposal{RequestID: "result", Status: "reported_success", Summary: "posted", Evidence: []string{"message"}}})
	require.Equal(t, 200, status)
	require.Len(t, runs.proposals, 1)
	assert.Equal(t, []core.Source{{ResourceType: "github_repo", ResourceID: "acme/app", Permission: "read"}}, runs.proposals[0].Sources, "what the run read travels with its result")
	assert.Zero(t, r.c.f.auth.interactChecks)
	assert.NotContains(t, r.c.f.auth.checkedUsers, "", "no source is checked as a user the run does not have")
}

// TestGroupRunPlanRefusedAfterMemberPause is the goal server's half of the
// dispatcher's TestGroupRunPausedByMemberMidFlight: a member's pause clears
// the goal's Execution, so the run's next plan derivation is refused rather
// than covered.
func TestGroupRunPlanRefusedAfterMemberPause(t *testing.T) {
	r, _ := groupRunServer(t)
	g := r.c.stored(t)
	r.c.f.turnProof(t, r.c.bob, "r-bob")
	status, _, body := r.c.f.as(t, "r-bob", groupUpdate("pause", g, "pause"))
	require.Equal(t, 200, status, body)
	require.Nil(t, r.c.stored(t).Execution)
	status, res := r.c.f.callSession(t, "run-token", "run", core.Request{Operation: "authorize_plan", PlanApproval: &core.PlanApprovalRequest{Digest: r.plan.Digest(), Phase: 0}})
	assert.NotEqual(t, 200, status)
	assert.Nil(t, res.PlanApproval)
}
