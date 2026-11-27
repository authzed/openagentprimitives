package goals_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
)

// renewalAuthority serves group goals and allows everything; managers are the
// goal's own list. deciders records who each group validation was for.
type renewalAuthority struct{ deciders []string }

func (*renewalAuthority) Authorize(context.Context, goals.Actor, bool) error { return nil }
func (*renewalAuthority) Sources(context.Context, goals.Actor) ([]goals.Source, error) {
	return nil, nil
}
func (*renewalAuthority) ReadGoal(context.Context, goals.Actor, goals.Goal) error { return nil }
func (*renewalAuthority) AuthorizeGroup(context.Context, goals.Actor, goals.GroupAccess) error {
	return nil
}
func (*renewalAuthority) CheckManagers(context.Context, goals.Actor, goals.Goal, goals.Change) error {
	return nil
}
func (*renewalAuthority) AuthorizeGoalManager(_ context.Context, a goals.Actor, g goals.Goal) error {
	if !slices.Contains(g.Managers, a.User) {
		return goals.ErrDenied
	}
	return nil
}
func (*renewalAuthority) Validate(context.Context, goals.Goal, goals.ExecutionTerms) error {
	return nil
}
func (*renewalAuthority) VerifyDecision(context.Context, goals.Goal, goals.ExecutionDecision) error {
	return nil
}
func (*renewalAuthority) AuthorizeDispatch(context.Context, goals.Goal) error { return nil }
func (a *renewalAuthority) ValidateGroupExecution(_ context.Context, _ goals.Goal, _ goals.ExecutionTerms, decider string) error {
	a.deciders = append(a.deciders, decider)
	return nil
}
func (*renewalAuthority) ValidateGroupDispatch(context.Context, goals.Goal, goals.ExecutionTerms) error {
	return nil
}

type renewalFixture struct {
	store *sqlstore.Store
	svc   *goals.Service
	auth  *renewalAuthority
	clock *time.Time
	alice goals.Actor
}

var renewalHost = goals.RenewalHost{Session: "ns/run-1", SessionUID: "run-uid-1"}

func newRenewalFixture(t *testing.T) *renewalFixture {
	t.Helper()
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "goals.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := goalsqlite.New(db.DB())
	require.NoError(t, store.Migrate(t.Context()))
	clock := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	f := &renewalFixture{store: store, auth: &renewalAuthority{}, clock: &clock, alice: groupActor("alice")}
	f.svc = &goals.Service{Store: store, Auth: f.auth, ExecutionAuth: f.auth, Now: func() time.Time { return *f.clock }, GroupLinks: func() []string { return []string{"group#member"} }}
	return f
}

// approved creates a group goal managed by alice and bob, approved by alice
// for a daily series of ten runs starting a minute from now.
func (f *renewalFixture) approved(t *testing.T) goals.Goal {
	t.Helper()
	ctx := t.Context()
	g, err := f.svc.Create(ctx, f.alice, goals.CreateRequest{RequestID: "create", Title: "Standup", Outcome: "Summarize"})
	require.NoError(t, err)
	bob := "bob"
	g, err = f.svc.Update(ctx, f.alice, goals.Change{RequestID: "add-bob", ID: g.ID, Revision: g.Revision, Action: "add_manager", Manager: &bob})
	require.NoError(t, err)
	g, err = f.svc.Update(ctx, f.alice, goals.Change{RequestID: "activate", ID: g.ID, Revision: g.Revision, Action: "activate"})
	require.NoError(t, err)
	due := f.clock.Add(time.Minute)
	terms := goals.ExecutionTerms{
		ClassDigest: "class-digest", DueAt: due, ExpiresAt: due.Add(10 * 24 * time.Hour),
		Schedule:          &sessionschedule.Spec{Kind: "daily", Timezone: "UTC", MaxRuns: 10, RunWindowSeconds: 3600},
		Bounds:            goals.ExecutionBounds{DurationSeconds: 300, Turns: 10, Tokens: 10000, ApprovalSeconds: 90},
		AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"summary"},
		ActionApproval: "standing_plan", StandingPlan: &plangate.Plan{Phases: []plangate.Phase{{Label: "post"}}},
		Group: &goals.GroupDestination{Channel: "team", ChannelUID: "channel-uid", MemberGroup: "group:g1#member"},
	}
	g, err = f.svc.RequestExecution(ctx, f.alice, goals.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: "request", Terms: terms})
	require.NoError(t, err)
	g, err = f.svc.DecideExecution(ctx, f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: "approval", Digest: g.Execution.Digest, Owner: "alice", Approved: true, Witness: "test-witness"})
	require.NoError(t, err)
	return g
}

func (f *renewalFixture) events(t *testing.T, action string) []goals.Event {
	t.Helper()
	pending, err := f.store.Pending(t.Context(), 100)
	require.NoError(t, err)
	var out []goals.Event
	for _, e := range pending {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestRequestRenewalRecordsASystemAuthoredPendingConsent(t *testing.T) {
	f := newRenewalFixture(t)
	g := f.approved(t)
	require.Equal(t, g.Execution.Terms, *g.LastApprovedTerms, "an approval is remembered for resume")
	*f.clock = g.Execution.Terms.ExpiresAt.Add(-72 * time.Hour)

	renewed, err := f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)
	p := renewed.PendingRenewal
	require.NotNil(t, p)
	assert.Equal(t, g.Revision, renewed.Revision, "a pending renewal leaves the current approval's revision alone")
	assert.Equal(t, g.Execution, renewed.Execution, "the current approval stays in force")
	assert.Equal(t, renewalHost.Session, p.Session, "the card is hosted in the run session")
	assert.Equal(t, renewalHost.SessionUID, p.SessionUID)
	assert.Equal(t, goals.RenewalRequestID(g.Execution.Digest), p.RequestID)
	assert.Equal(t, g.Execution.RequestRevision, p.RequestRevision, "decided in place, at the renewed approval's revision")
	assert.NotEqual(t, g.Execution.Digest, p.Digest)
	assert.Nil(t, p.Decision)
	next, ok, err := sessionschedule.NextNominal(*g.Execution.Terms.Schedule, g.Execution.Terms.DueAt, *f.clock)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, next, p.Terms.DueAt, "a fresh DueAt: the next nominal run")
	assert.Equal(t, next.Add(10*24*time.Hour), p.Terms.ExpiresAt, "the same window length as the current approval")
	assert.NotEmpty(t, p.Terms.ScheduleWindows)
	assert.Equal(t, g.Execution.Terms.StandingPlan, p.Terms.StandingPlan, "the same terms otherwise")

	events := f.events(t, "request_renewal")
	require.Len(t, events, 1)
	assert.Equal(t, "system:operator", events[0].Actor, "a renewal is authored by the operator, not a person")
	assert.Empty(t, events[0].Session, "no session acted")

	require.NoError(t, f.svc.Dispatchable(t.Context(), renewed), "the current approval still dispatches while a renewal is pending")

	_, err = f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, f.clock.Add(time.Hour))
	require.ErrorIs(t, err, goals.ErrConflict, "one renewal per expiring approval")
	assert.Len(t, f.events(t, "request_renewal"), 1)
}

func TestRequestRenewalRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, f *renewalFixture, g goals.Goal) goals.Domain
	}{
		{"private domain: denied", func(_ *testing.T, f *renewalFixture, _ goals.Goal) goals.Domain {
			d := f.alice.Domain
			d.Owner = "alice"
			return d
		}},
		{"paused goal: no approval to renew", func(t *testing.T, f *renewalFixture, g goals.Goal) goals.Domain {
			_, err := f.svc.Update(t.Context(), f.alice, goals.Change{RequestID: "pause", ID: g.ID, Revision: g.Revision, Action: "pause"})
			require.NoError(t, err)
			return f.alice.Domain
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRenewalFixture(t)
			g := f.approved(t)
			d := tc.mutate(t, f, g)
			_, err := f.svc.RequestRenewal(t.Context(), d, g.ID, renewalHost, *f.clock)
			require.Error(t, err)
			assert.Empty(t, f.events(t, "request_renewal"))
		})
	}
}

func TestDecideRenewal(t *testing.T) {
	f := newRenewalFixture(t)
	g := f.approved(t)
	*f.clock = g.Execution.Terms.ExpiresAt.Add(-72 * time.Hour)
	renewed, err := f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)
	p := *renewed.PendingRenewal

	require.NoError(t, f.svc.CheckDecision(t.Context(), f.alice.Domain, g.ID, p.Digest, "bob", true), "a manager may decide the renewal")
	require.ErrorIs(t, f.svc.CheckDecision(t.Context(), f.alice.Domain, g.ID, p.Digest, "mallory", true), goals.ErrDenied, "a non-manager may not")

	f.auth.deciders = nil
	decided, err := f.svc.DecideExecution(t.Context(), f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: "renewal-approval", Digest: p.Digest, Owner: "bob", Approved: true, Witness: "test-witness"})
	require.NoError(t, err)
	assert.Equal(t, []string{"bob"}, f.auth.deciders, "the start gate is checked for the renewing manager")
	require.NotNil(t, decided.Execution)
	assert.Equal(t, p.Digest, decided.Execution.Digest, "the renewal becomes the approval")
	assert.Equal(t, "bob", decided.Execution.Decision.Owner, "the clicker is the new approver")
	assert.Nil(t, decided.PendingRenewal)
	assert.Equal(t, p.Terms, *decided.LastApprovedTerms)
	assert.Equal(t, g.Revision, decided.Revision, "a renewal changes consent only: the revision stays")
	assert.Equal(t, g.Execution, decided.PriorExecution, "the renewed approval is kept for the runs it scheduled")

	old, ok := decided.ForConsent(g.Execution.Digest)
	require.True(t, ok)
	require.NoError(t, f.svc.Dispatchable(t.Context(), old), "a run bound to the renewed approval still dispatches")
	*f.clock = p.Terms.DueAt.Add(time.Second)
	require.NoError(t, f.svc.Dispatchable(t.Context(), decided), "the renewed approval dispatches")
}

// A goal keeps one earlier approval beside the one in force. A second renewal
// approved while a run is still bound to the first approval would drop it and
// strand that run, so it is refused until the run has finished.
func TestSecondRenewalWaitsForRunsUnderTheEarlierApproval(t *testing.T) {
	ctx := t.Context()
	f := newRenewalFixture(t)
	g := f.approved(t)
	first := g.Execution.Digest
	*f.clock = g.Execution.Terms.DueAt.Add(time.Second)
	_, err := f.store.Schedule(ctx, g)
	require.NoError(t, err)
	due, err := f.store.Due(ctx, *f.clock, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	running, err := f.store.Claim(ctx, goals.ClaimRequest{ID: due[0].ID, Worker: "operator", Now: *f.clock, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 4})
	require.NoError(t, err)

	approveRenewal := func(requestID string) (goals.Goal, string, error) {
		t.Helper()
		pending, err := f.svc.RequestRenewal(ctx, f.alice.Domain, g.ID, renewalHost, *f.clock)
		require.NoError(t, err)
		digest := pending.PendingRenewal.Digest
		decided, err := f.svc.DecideExecution(ctx, f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: requestID, Digest: digest, Owner: "bob", Approved: true, Witness: "test-witness"})
		return decided, digest, err
	}
	governed := func(msg string) {
		t.Helper()
		stored, err := f.store.Get(ctx, g.Domain, g.ID)
		require.NoError(t, err)
		view, ok := stored.ForConsent(first)
		require.True(t, ok, msg)
		assert.NoError(t, f.svc.DispatchOccurrence(ctx, view, running), msg)
	}

	renewed, _, err := approveRenewal("renewal-1")
	require.NoError(t, err, "nothing is bound to an earlier approval yet")
	_, err = f.store.Schedule(ctx, renewed)
	require.NoError(t, err)
	governed("the first renewal keeps the approval the run started under")

	pending, err := f.svc.RequestRenewal(ctx, f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err, "a second renewal may be asked for")
	second := goals.ExecutionDecision{RequestID: "renewal-2", Digest: pending.PendingRenewal.Digest, Owner: "bob", Approved: true, Witness: "test-witness"}
	require.ErrorIs(t, f.svc.CheckDecision(ctx, f.alice.Domain, g.ID, second.Digest, "bob", true), goals.ErrConflict, "refused before any decision is recorded")
	_, err = f.svc.DecideExecution(ctx, f.alice.Domain, g.ID, second)
	require.ErrorIs(t, err, goals.ErrConflict, "a run is still bound to the first approval")
	governed("the refused renewal left the running occurrence governed")
	require.NoError(t, f.svc.CheckDecision(ctx, f.alice.Domain, g.ID, second.Digest, "bob", false), "declining drops nothing, so it is not refused")

	_, err = f.store.Finish(ctx, running, goals.OccurrenceCancelled, *f.clock)
	require.NoError(t, err)
	decided, err := f.svc.DecideExecution(ctx, f.alice.Domain, g.ID, second)
	require.NoError(t, err, "approved once nothing runs under the first approval")
	assert.Equal(t, second.Digest, decided.Execution.Digest)
	assert.Equal(t, renewed.Execution.Digest, decided.PriorExecution.Digest)
}

// Once the earlier approval has run out nothing can run under it, so it no
// longer holds a second renewal back.
func TestSecondRenewalIsNotHeldByAnExpiredEarlierApproval(t *testing.T) {
	ctx := t.Context()
	f := newRenewalFixture(t)
	g := f.approved(t)
	*f.clock = g.Execution.Terms.DueAt.Add(time.Second)
	_, err := f.store.Schedule(ctx, g)
	require.NoError(t, err)
	due, err := f.store.Due(ctx, *f.clock, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	_, err = f.store.Claim(ctx, goals.ClaimRequest{ID: due[0].ID, Worker: "operator", Now: *f.clock, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 4})
	require.NoError(t, err)

	pending, err := f.svc.RequestRenewal(ctx, f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)
	renewed, err := f.svc.DecideExecution(ctx, f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: "renewal-1", Digest: pending.PendingRenewal.Digest, Owner: "bob", Approved: true, Witness: "test-witness"})
	require.NoError(t, err)

	*f.clock = g.Execution.Terms.ExpiresAt.Add(time.Second)
	require.True(t, goals.ApprovalInForce(renewed, *f.clock), "the renewal outlasts the approval it renewed")
	pending, err = f.svc.RequestRenewal(ctx, f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)
	_, err = f.svc.DecideExecution(ctx, f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: "renewal-2", Digest: pending.PendingRenewal.Digest, Owner: "bob", Approved: true, Witness: "test-witness"})
	require.NoError(t, err, "the claimed run's approval has expired: it cannot run, and holds nothing back")
}

func TestManagerChangesKeepTheApprovalDispatchable(t *testing.T) {
	f := newRenewalFixture(t)
	g := f.approved(t)
	*f.clock = g.Execution.Terms.DueAt.Add(time.Second)
	carol, bob := "carol", "bob"
	added, err := f.svc.Update(t.Context(), f.alice, goals.Change{RequestID: "add-carol", ID: g.ID, Revision: g.Revision, Action: "add_manager", Manager: &carol})
	require.NoError(t, err)
	assert.Equal(t, g.Revision, added.Revision)
	require.NoError(t, f.svc.Dispatchable(t.Context(), added), "adding a manager does not stop the approved goal")
	removed, err := f.svc.Update(t.Context(), f.alice, goals.Change{RequestID: "remove-bob", ID: g.ID, Revision: added.Revision, Action: "remove_manager", Manager: &bob})
	require.NoError(t, err)
	require.NoError(t, f.svc.Dispatchable(t.Context(), removed), "nor does removing one")
	assert.Len(t, f.events(t, "add_manager"), 2, "a manager change is still audited (bob, then carol)")

}

func TestDeclineRenewalKeepsTheCurrentApproval(t *testing.T) {
	f := newRenewalFixture(t)
	g := f.approved(t)
	*f.clock = g.Execution.Terms.ExpiresAt.Add(-72 * time.Hour)
	renewed, err := f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)
	p := *renewed.PendingRenewal

	declined, err := f.svc.DecideExecution(t.Context(), f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: "renewal-decline", Digest: p.Digest, Owner: "bob", Approved: false, Witness: "test-witness"})
	require.NoError(t, err)
	assert.Equal(t, g.Revision, declined.Revision)
	assert.Equal(t, g.Execution, declined.Execution, "declining a renewal does not revoke the current approval")
	require.NotNil(t, declined.PendingRenewal)
	require.NotNil(t, declined.PendingRenewal.Decision)
	assert.False(t, declined.PendingRenewal.Decision.Approved)
	require.NoError(t, f.svc.Dispatchable(t.Context(), declined))

	again, err := f.svc.DecideExecution(t.Context(), f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: "renewal-decline", Digest: p.Digest, Owner: "bob", Approved: false, Witness: "test-witness"})
	require.NoError(t, err, "an identical retry is accepted")
	assert.Equal(t, declined.PendingRenewal, again.PendingRenewal)

	_, err = f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, f.clock.Add(time.Hour))
	require.ErrorIs(t, err, goals.ErrConflict, "a declined renewal is not asked again for the same approval")
}

func TestPauseClearsAPendingRenewalAndKeepsTheLastApprovedTerms(t *testing.T) {
	f := newRenewalFixture(t)
	g := f.approved(t)
	*f.clock = g.Execution.Terms.ExpiresAt.Add(-72 * time.Hour)
	g, err := f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)
	paused, err := f.svc.Update(t.Context(), f.alice, goals.Change{RequestID: "pause", ID: g.ID, Revision: g.Revision, Action: "pause"})
	require.NoError(t, err)
	assert.Nil(t, paused.Execution)
	assert.Nil(t, paused.PendingRenewal, "pause withdraws a pending renewal with the approval")
	assert.Equal(t, g.LastApprovedTerms, paused.LastApprovedTerms)
}

func TestApprovalExpiredIsDerivedOnRead(t *testing.T) {
	f := newRenewalFixture(t)
	g := f.approved(t)
	got, err := f.svc.Get(t.Context(), f.alice, g.ID)
	require.NoError(t, err)
	assert.Empty(t, got.BlockedReason)

	*f.clock = g.Execution.Terms.ExpiresAt
	got, err = f.svc.Get(t.Context(), f.alice, g.ID)
	require.NoError(t, err)
	assert.Equal(t, goals.BlockedApprovalExpired, got.BlockedReason)
	page, err := f.svc.List(t.Context(), f.alice, goals.ListRequest{})
	require.NoError(t, err)
	require.Len(t, page.Goals, 1)
	assert.Equal(t, goals.BlockedApprovalExpired, page.Goals[0].BlockedReason)
	stored, err := f.store.Get(t.Context(), f.alice.Domain, g.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.BlockedReason, "the reason is derived, never stored")
	require.ErrorIs(t, f.svc.Dispatchable(t.Context(), stored), goals.ErrDenied, "an expired approval does not dispatch")
}

func TestRenewedTermsCapsTheWindowAt90Days(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	terms := goals.ExecutionTerms{DueAt: now.Add(-66 * 24 * time.Hour), ExpiresAt: now.Add(29 * 24 * time.Hour),
		Schedule: &sessionschedule.Spec{Kind: "daily", Timezone: "UTC", MaxRuns: 100, RunWindowSeconds: 60}}
	got, err := goals.RenewedTerms(terms, now)
	require.NoError(t, err)
	assert.Equal(t, now.Add(24*time.Hour), got.DueAt)
	assert.Equal(t, now.Add(90*24*time.Hour), got.ExpiresAt)

	terms.Schedule = nil
	_, err = goals.RenewedTerms(terms, now)
	require.ErrorIs(t, err, goals.ErrInvalid, "a one-time approval has no next run to renew")
}

func TestRenewExecutionByAManager(t *testing.T) {
	f := newRenewalFixture(t)
	g := f.approved(t)
	*f.clock = g.Execution.Terms.DueAt.Add(time.Second)
	// The operator's renewal, whose card lapsed with its run.
	g, err := f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)

	bob := groupActor("bob")
	bob.SessionUID = "s-uid"
	_, err = f.svc.RenewExecution(t.Context(), groupActor("mallory"), goals.ExecutionRequest{RequestID: "m", ID: g.ID, Revision: g.Revision})
	require.ErrorIs(t, err, goals.ErrDenied, "only a manager may ask")
	_, err = f.svc.RenewExecution(t.Context(), bob, goals.ExecutionRequest{RequestID: "t", ID: g.ID, Revision: g.Revision, Terms: goals.ExecutionTerms{ClassDigest: "x"}})
	require.ErrorIs(t, err, goals.ErrInvalid, "a renewal names no terms of its own")

	renewed, err := f.svc.RenewExecution(t.Context(), bob, goals.ExecutionRequest{RequestID: "renew", ID: g.ID, Revision: g.Revision})
	require.NoError(t, err)
	assert.Equal(t, g.Revision, renewed.Revision)
	assert.Equal(t, g.Execution, renewed.Execution, "the approval in force is untouched")
	require.NotNil(t, renewed.PendingRenewal)
	assert.Equal(t, bob.Session, renewed.PendingRenewal.Session, "hosted in the manager's own session")
	assert.NotEqual(t, g.PendingRenewal.Digest, renewed.PendingRenewal.Digest, "it replaces the lapsed operator renewal")
	assert.False(t, renewed.PendingRenewal.SystemRenewal())
	assert.Contains(t, f.auth.deciders, "bob", "the start gate is checked for the requester")
	require.NoError(t, f.svc.Dispatchable(t.Context(), renewed), "the approval in force still dispatches")

	again, err := f.svc.RenewExecution(t.Context(), bob, goals.ExecutionRequest{RequestID: "renew", ID: g.ID, Revision: g.Revision})
	require.NoError(t, err, "a retry replays")
	assert.Equal(t, renewed.PendingRenewal, again.PendingRenewal)

	decided, err := f.svc.DecideExecution(t.Context(), f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: "approve", Digest: renewed.PendingRenewal.Digest, Owner: "alice", Approved: true, Witness: "test-witness"})
	require.NoError(t, err)
	assert.Equal(t, g.Revision, decided.Revision, "approved in place")
	assert.Equal(t, renewed.PendingRenewal.Digest, decided.Execution.Digest)

	*f.clock = decided.Execution.Terms.ExpiresAt
	_, err = f.svc.RenewExecution(t.Context(), bob, goals.ExecutionRequest{RequestID: "late", ID: g.ID, Revision: decided.Revision})
	require.ErrorIs(t, err, goals.ErrInvalid, "nothing in force to renew")
}

func TestRequestExecutionReplacesRenewals(t *testing.T) {
	f := newRenewalFixture(t)
	g := f.approved(t)
	*f.clock = g.Execution.Terms.DueAt.Add(time.Second)
	g, err := f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)
	g, err = f.svc.DecideExecution(t.Context(), f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: "approve", Digest: g.PendingRenewal.Digest, Owner: "alice", Approved: true, Witness: "test-witness"})
	require.NoError(t, err)
	require.NotNil(t, g.PriorExecution)
	g, err = f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)
	require.NotNil(t, g.PendingRenewal)

	terms := g.Execution.Terms
	terms.DueAt, terms.ExpiresAt = f.clock.Add(time.Hour), f.clock.Add(5*24*time.Hour)
	replaced, err := f.svc.RequestExecution(t.Context(), f.alice, goals.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: "new-terms", Terms: terms})
	require.NoError(t, err)
	assert.Nil(t, replaced.PendingRenewal, "a request replaces the approval and its pending renewal")
	assert.Nil(t, replaced.PriorExecution, "and the approval a renewal replaced")
}

// pausingStore pauses the goal just before the next in-place commit, from
// another writer, as if the pause raced a renewal decision.
type pausingStore struct {
	*sqlstore.Store
	pause func()
}

func (p *pausingStore) CommitInPlace(ctx context.Context, m goals.Mutation) (goals.Goal, error) {
	if pause := p.pause; pause != nil {
		p.pause = nil
		pause()
	}
	return p.Store.CommitInPlace(ctx, m)
}

func TestRenewalDecisionLosesARaceWithPause(t *testing.T) {
	f := newRenewalFixture(t)
	g := f.approved(t)
	*f.clock = g.Execution.Terms.DueAt.Add(time.Second)
	g, err := f.svc.RequestRenewal(t.Context(), f.alice.Domain, g.ID, renewalHost, *f.clock)
	require.NoError(t, err)

	racing := *f.svc
	racing.Store = &pausingStore{Store: f.store, pause: func() {
		_, err := f.svc.Update(t.Context(), f.alice, goals.Change{RequestID: "pause", ID: g.ID, Revision: g.Revision, Action: "pause"})
		require.NoError(t, err)
	}}
	_, err = racing.DecideExecution(t.Context(), f.alice.Domain, g.ID, goals.ExecutionDecision{RequestID: "approve", Digest: g.PendingRenewal.Digest, Owner: "alice", Approved: true, Witness: "test-witness"})
	require.ErrorIs(t, err, goals.ErrConflict, "the decision read the goal before the pause")

	stored, err := f.store.Get(t.Context(), g.Domain, g.ID)
	require.NoError(t, err)
	assert.Equal(t, goals.Paused, stored.State)
	assert.Nil(t, stored.Execution, "the pause's withdrawal stands")
	assert.Nil(t, stored.PendingRenewal)
}
