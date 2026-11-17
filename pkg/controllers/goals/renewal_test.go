package goals

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

// approveSeries approves a group goal for a daily series of ten runs, the
// first a minute from now, authorized for exactly ten days from it.
func (f *groupRunFixture) approveSeries(t *testing.T) domain.Goal {
	t.Helper()
	ctx := context.Background()
	f.n++
	id := func(s string) string { return fmt.Sprintf("%s-%d", s, f.n) }
	g, err := f.svc.Create(ctx, f.alice, domain.CreateRequest{RequestID: id("create"), Title: "Standup", Outcome: "Summarize the day"})
	require.NoError(t, err)
	g, err = f.svc.Update(ctx, f.alice, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: id("activate"), Action: "activate"})
	require.NoError(t, err)
	due := f.clock.Add(time.Minute)
	terms := domain.ExecutionTerms{
		ClassDigest: "class-digest", DueAt: due, ExpiresAt: due.Add(10 * 24 * time.Hour),
		Schedule:          &sessionschedule.Spec{Kind: "daily", Timezone: "UTC", MaxRuns: 10, RunWindowSeconds: 3600},
		Bounds:            domain.ExecutionBounds{DurationSeconds: 300, Turns: 10, Tokens: 10000, ApprovalSeconds: 90},
		AllowedOperations: []string{"respond_to_user", "report_goal_result"}, Evidence: []string{"daily summary"},
		ActionApproval: "standing_plan", StandingPlan: &plangate.Plan{Phases: []plangate.Phase{{Label: "post"}}},
		Group:         &domain.GroupDestination{Channel: groupRunChannel, ChannelUID: "channel-uid", MemberGroup: groupRunOwner},
		AgentIdentity: "bot", AgentIdentityUID: "aid-uid",
	}
	g, err = f.svc.RequestExecution(ctx, f.alice, domain.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: id("request"), Terms: terms})
	require.NoError(t, err)
	g, err = f.svc.DecideExecution(ctx, f.alice.Domain, g.ID, domain.ExecutionDecision{RequestID: id("approval"), Digest: g.Execution.Digest, Owner: "alice", Approved: true, Witness: "test-only-human-witness"})
	require.NoError(t, err)
	_, err = f.store.Schedule(ctx, g)
	require.NoError(t, err)
	return g
}

// runAt ticks the dispatcher just after due and returns the run session
// started for the occurrence due then.
func (f *groupRunFixture) runAt(t *testing.T, d *Dispatcher, due time.Time) *v1.AgentSession {
	t.Helper()
	ctx := context.Background()
	*f.clock = due.Add(time.Second)
	require.NoError(t, d.Tick(ctx))
	var sessions v1.AgentSessionList
	require.NoError(t, f.k8s.List(ctx, &sessions))
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		o, err := f.store.Occurrence(ctx, sess.Spec.GoalExecution.OccurrenceID)
		require.NoError(t, err)
		if o.DueAt.Equal(due) {
			return sess
		}
	}
	require.Failf(t, "no run started", "no session for the run due %s", due)
	return nil
}

func (f *groupRunFixture) renewalRequests(t *testing.T) int {
	t.Helper()
	pending, err := f.store.Pending(context.Background(), 100)
	require.NoError(t, err)
	n := 0
	for _, e := range pending {
		if e.Action == "request_renewal" {
			n++
		}
	}
	return n
}

// A daily series authorized for ten days: the run that leaves less than 72h
// plus the day's wait for the next run asks for renewal, from its own session.
func TestGroupRunRenewalTrigger(t *testing.T) {
	f := newGroupRunFixture(t)
	g := f.approveSeries(t)
	d := f.dispatcher()
	ctx := context.Background()
	first := g.Execution.Terms.DueAt
	expires := g.Execution.Terms.ExpiresAt

	// Day 5 leaves 120h, more than 72h plus the 24h wait for day 6.
	f.runAt(t, d, first.Add(5*24*time.Hour))
	current, err := f.store.Get(ctx, g.Domain, g.ID)
	require.NoError(t, err)
	assert.Nil(t, current.PendingRenewal, "too early: the next run can still ask in time")
	assert.Zero(t, f.renewalRequests(t))

	// Day 6 leaves 96h: by day 7 it would be under 72h.
	sess := f.runAt(t, d, first.Add(6*24*time.Hour))
	require.Equal(t, 96*time.Hour, expires.Sub(first.Add(6*24*time.Hour)))
	current, err = f.store.Get(ctx, g.Domain, g.ID)
	require.NoError(t, err)
	require.NotNil(t, current.PendingRenewal, "this run asks for renewal")
	assert.Equal(t, "team/"+sess.Name, current.PendingRenewal.Session, "the card is hosted in this run's session")
	assert.Equal(t, string(sess.UID), current.PendingRenewal.SessionUID)
	assert.Equal(t, g.Execution, current.Execution, "the approval in force is unchanged")
	assert.Equal(t, 1, f.renewalRequests(t))

	// Day 7 is inside the window too, and still runs on the current approval;
	// it asks nothing more.
	later := f.runAt(t, d, first.Add(7*24*time.Hour))
	require.NotNil(t, later, "a run dispatched while the renewal is pending still validates")
	after, err := f.store.Get(ctx, g.Domain, g.ID)
	require.NoError(t, err)
	assert.Equal(t, current.PendingRenewal, after.PendingRenewal)
	assert.Equal(t, 1, f.renewalRequests(t), "one renewal per expiring approval")
}

func TestOneTimeGroupRunAsksNoRenewal(t *testing.T) {
	f := newGroupRunFixture(t)
	g, o := f.approve(t)
	// The approval ends within 72h of the run, but there is no next run.
	f.started(t, f.dispatcher(), o)
	current, err := f.store.Get(context.Background(), g.Domain, g.ID)
	require.NoError(t, err)
	assert.Nil(t, current.PendingRenewal)
	assert.Zero(t, f.renewalRequests(t))
}

// Approving a renewal supersedes nothing. The run that hosts the card
// finishes on the approval it started under, and the next run uses the
// renewal.
func TestApprovedRenewalLeavesRunsInFlightAndRebindsTheNext(t *testing.T) {
	f := newGroupRunFixture(t)
	g := f.approveSeries(t)
	d := f.dispatcher()
	ctx := context.Background()
	first := g.Execution.Terms.DueAt

	day6 := first.Add(6 * 24 * time.Hour)
	sess := f.runAt(t, d, day6)
	pending, err := f.store.Get(ctx, g.Domain, g.ID)
	require.NoError(t, err)
	require.NotNil(t, pending.PendingRenewal)
	renewal := pending.PendingRenewal.Digest

	*f.clock = day6.Add(30 * time.Second)
	decided, err := f.svc.DecideExecution(ctx, g.Domain, g.ID, domain.ExecutionDecision{RequestID: "renewal-approval", Digest: renewal, Owner: "alice", Approved: true, Witness: "test-only-human-witness"})
	require.NoError(t, err)
	require.Equal(t, g.Revision, decided.Revision)
	_, err = f.store.Schedule(ctx, decided) // what the outbox does on execution_decision
	require.NoError(t, err)

	*f.clock = day6.Add(time.Minute)
	require.NoError(t, d.Tick(ctx))
	o, err := f.store.Occurrence(ctx, sess.Spec.GoalExecution.OccurrenceID)
	require.NoError(t, err)
	assert.Nil(t, o.Outcome, "the run in flight is not superseded")
	assert.Equal(t, g.Execution.Digest, o.ConsentDigest, "it keeps the approval it started under")
	require.NoError(t, d.ValidateGoalSession(ctx, sess))
	reported, err := d.RecordGoalResult(ctx, sess, domain.RunProposal{RequestID: "result", Status: "reported_success", Summary: "Posted the summary", Evidence: []string{"message link"}})
	require.NoError(t, err, "its result is accepted")
	require.NotNil(t, reported.Proposal)

	next := f.runAt(t, d, first.Add(7*24*time.Hour))
	assert.Equal(t, renewal, next.Spec.GoalExecution.ConsentDigest, "the next run uses the renewal")
	require.NoError(t, d.ValidateGoalSession(ctx, next))
}

// A manager change neither supersedes the approval nor its runs.
func TestManagerChangesKeepGroupRunsDispatching(t *testing.T) {
	f := newGroupRunFixture(t)
	g := f.approveSeries(t)
	d := f.dispatcher()
	ctx := context.Background()
	first := g.Execution.Terms.DueAt

	bob := "bob"
	g, err := f.svc.Update(ctx, f.alice, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "add-bob", Action: "add_manager", Manager: &bob})
	require.NoError(t, err)
	sess := f.runAt(t, d, first)
	require.NoError(t, d.ValidateGoalSession(ctx, sess), "a run after adding a manager dispatches")

	alice := "alice"
	_, err = f.svc.Update(ctx, groupRunActor("bob"), domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "remove-alice", Action: "remove_manager", Manager: &alice})
	require.NoError(t, err)
	require.NoError(t, d.ValidateGoalSession(ctx, sess), "removing the approver does not stop the run in flight")
	next := f.runAt(t, d, first.Add(24*time.Hour))
	require.NoError(t, d.ValidateGoalSession(ctx, next), "nor the next one")
}
