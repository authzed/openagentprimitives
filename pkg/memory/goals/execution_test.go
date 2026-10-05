package goals_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
	goalpostgres "github.com/authzed/openagentprimitives/pkg/memory/goals/postgres"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	mempostgres "github.com/authzed/openagentprimitives/pkg/memory/postgres"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/authzed/openagentprimitives/test/testpostgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A test authority stands in for the future signed interaction/SpiceDB adapter.
// No production binary installs it, and there is no agent-callable approval API.
type executionAuthority struct{ validateErr, decisionErr, grantErr error }

func (a *executionAuthority) Validate(context.Context, goals.Goal, goals.ExecutionTerms) error {
	return a.validateErr
}
func (a *executionAuthority) VerifyDecision(context.Context, goals.Goal, goals.ExecutionDecision) error {
	return a.decisionErr
}
func (a *executionAuthority) AuthorizeDispatch(context.Context, goals.Goal) error { return a.grantErr }

var executionNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func terms(a goals.Actor) goals.ExecutionTerms {
	return goals.ExecutionTerms{ClassDigest: "sha256:class-policy", DueAt: executionNow.Add(time.Minute), ExpiresAt: executionNow.Add(time.Hour),
		Bounds:            goals.ExecutionBounds{DurationSeconds: 300, Turns: 20, Tokens: 10000, ApprovalSeconds: 120},
		AllowedOperations: []string{"private_reminder"}, Evidence: []string{"memory:source-1"},
		Destination: goals.PrivateDestination{Channel: "private-inbox", ChannelUID: "channel-uid", Recipient: a.Domain.Owner}}
}

func executionService(store goals.Store) (*goals.Service, *executionAuthority) {
	a := &executionAuthority{}
	return &goals.Service{Store: store, Auth: allow{}, ExecutionAuth: a, Now: func() time.Time { return executionNow }}, a
}

func approveExecution(t *testing.T, s *goals.Service, a goals.Actor, key string) goals.Goal {
	t.Helper()
	ctx := context.Background()
	g, err := s.Create(ctx, a, goals.CreateRequest{RequestID: key + "-create", Title: "Reminder", Outcome: "Send a private reminder"})
	require.NoError(t, err)
	g, err = s.Update(ctx, a, goals.Change{RequestID: key + "-activate", ID: g.ID, Revision: g.Revision, Action: "activate"})
	require.NoError(t, err)
	g, err = s.RequestExecution(ctx, a, goals.ExecutionRequest{RequestID: key + "-request", ID: g.ID, Revision: g.Revision, Terms: terms(a)})
	require.NoError(t, err)
	g, err = s.DecideExecution(ctx, a.Domain, g.ID, goals.ExecutionDecision{RequestID: key + "-decision", Digest: g.Execution.Digest, Owner: a.Domain.Owner, Approved: true, Witness: "test-signed-human-decision"})
	require.NoError(t, err)
	return g
}

func TestExecutionConsentSeparatesApprovalAndGrant(t *testing.T) {
	ctx := context.Background()
	s, auth := executionService(openSQL(t, filepath.Join(t.TempDir(), "consent.db")))
	a := actor()
	g := create(t, s, "create")
	g, err := s.Update(ctx, a, goals.Change{RequestID: "active", ID: g.ID, Revision: g.Revision, Action: "activate"})
	require.NoError(t, err)
	r := goals.ExecutionRequest{RequestID: "request", ID: g.ID, Revision: g.Revision, Terms: terms(a)}
	pending, err := s.RequestExecution(ctx, a, r)
	require.NoError(t, err)
	replay, err := s.RequestExecution(ctx, a, r)
	require.NoError(t, err)
	assert.Equal(t, pending, replay)
	require.ErrorIs(t, s.Dispatchable(ctx, pending), goals.ErrDenied)
	d := goals.ExecutionDecision{RequestID: "decision", Digest: pending.Execution.Digest, Owner: a.Domain.Owner, Approved: true, Witness: "signed"}
	for _, bad := range []goals.ExecutionDecision{
		{RequestID: "wrong-owner", Digest: d.Digest, Owner: "bob", Approved: true, Witness: "signed"},
		{RequestID: "wrong-digest", Digest: "different", Owner: d.Owner, Approved: true, Witness: "signed"},
		{RequestID: "no-witness", Digest: d.Digest, Owner: d.Owner, Approved: true},
	} {
		_, err = s.DecideExecution(ctx, a.Domain, g.ID, bad)
		require.ErrorIs(t, err, goals.ErrDenied)
	}
	auth.decisionErr = goals.ErrDenied
	_, err = s.DecideExecution(ctx, a.Domain, g.ID, d)
	require.ErrorIs(t, err, goals.ErrDenied)
	auth.decisionErr = nil
	approved, err := s.DecideExecution(ctx, a.Domain, g.ID, d)
	require.NoError(t, err)
	replay, err = s.DecideExecution(ctx, a.Domain, g.ID, d)
	require.NoError(t, err)
	assert.Equal(t, approved, replay)
	s.Now = func() time.Time { return executionNow.Add(2 * time.Minute) }
	auth.grantErr = goals.ErrDenied
	require.ErrorIs(t, s.Dispatchable(ctx, approved), goals.ErrDenied)
	auth.grantErr = nil
	require.NoError(t, s.Dispatchable(ctx, approved))
	auth.validateErr = goals.ErrDenied
	require.ErrorIs(t, s.Dispatchable(ctx, approved), goals.ErrDenied)
	auth.validateErr = nil
	s.Now = func() time.Time { return approved.Execution.Terms.ExpiresAt }
	require.ErrorIs(t, s.Dispatchable(ctx, approved), goals.ErrDenied)
	s.Now = func() time.Time { return executionNow.Add(2 * time.Minute) }
	paused, err := s.Update(ctx, a, goals.Change{ID: g.ID, Revision: approved.Revision, RequestID: "pause", Action: "pause"})
	require.NoError(t, err)
	assert.Nil(t, paused.Execution)
	require.ErrorIs(t, s.Dispatchable(ctx, approved), goals.ErrDenied, "a stale approved snapshot cannot authorize activation")
	resumed, err := s.Update(ctx, a, goals.Change{ID: g.ID, Revision: paused.Revision, RequestID: "resume", Action: "resume"})
	require.NoError(t, err)
	require.ErrorIs(t, s.Dispatchable(ctx, resumed), goals.ErrDenied)
	events, err := s.Store.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, events, 6)
	assert.Equal(t, "execution_decision", events[3].Action)
	assert.Equal(t, d.Witness, events[3].Goal.Execution.Decision.Witness)
}

func TestExecutionRequestRequiresFinitePrivateWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*goals.ExecutionTerms)
	}{
		{"no duration", func(t *goals.ExecutionTerms) { t.Bounds.DurationSeconds = 0 }},
		{"no tokens", func(t *goals.ExecutionTerms) { t.Bounds.Tokens = 0 }},
		{"no turns", func(t *goals.ExecutionTerms) { t.Bounds.Turns = 0 }},
		{"no approval timeout", func(t *goals.ExecutionTerms) { t.Bounds.ApprovalSeconds = 0 }},
		{"approval beyond duration", func(t *goals.ExecutionTerms) { t.Bounds.ApprovalSeconds = 301 }},
		{"past due", func(t *goals.ExecutionTerms) { t.DueAt = executionNow.Add(-time.Second) }},
		{"expiry before due", func(t *goals.ExecutionTerms) { t.ExpiresAt = executionNow }},
		{"unbounded expiry", func(t *goals.ExecutionTerms) { t.ExpiresAt = executionNow.Add(31 * 24 * time.Hour) }},
		{"foreign recipient", func(t *goals.ExecutionTerms) { t.Destination.Recipient = "bob" }},
		{"unversioned route", func(t *goals.ExecutionTerms) { t.Destination.ChannelUID = "" }},
		{"unversioned class", func(t *goals.ExecutionTerms) { t.ClassDigest = "" }},
		{"no evidence", func(t *goals.ExecutionTerms) { t.Evidence = nil }},
		{"no operations", func(t *goals.ExecutionTerms) { t.AllowedOperations = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := executionService(inmem.New())
			g := approveExecution(t, s, actor(), "seed")
			v := terms(actor())
			tc.change(&v)
			_, err := s.RequestExecution(context.Background(), actor(), goals.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: "bad", Terms: v})
			require.ErrorIs(t, err, goals.ErrInvalid)
		})
	}
	s := service(inmem.New())
	_, err := s.RequestExecution(context.Background(), actor(), goals.ExecutionRequest{})
	require.ErrorIs(t, err, goals.ErrDenied)
	_, err = s.DecideExecution(context.Background(), actor().Domain, "goal", goals.ExecutionDecision{})
	require.ErrorIs(t, err, goals.ErrDenied)
	ephemeral, _ := executionService(inmem.New())
	g := approveExecution(t, ephemeral, actor(), "ephemeral")
	ephemeral.Now = func() time.Time { return executionNow.Add(2 * time.Minute) }
	require.ErrorIs(t, ephemeral.Dispatchable(context.Background(), g), goals.ErrDenied, "ephemeral storage cannot authorize dispatch")
}

func durableFixtures() []struct {
	name string
	new  func(*testing.T) goals.Store
} {
	return []struct {
		name string
		new  func(*testing.T) goals.Store
	}{
		{"sqlite", func(t *testing.T) goals.Store { return openSQL(t, filepath.Join(t.TempDir(), "execution.db")) }},
		{"postgres", func(t *testing.T) goals.Store {
			c, err := mempostgres.NewClient(context.Background(), testpostgres.URI(t))
			require.NoError(t, err)
			t.Cleanup(c.Close)
			s := goalpostgres.New(c.Pool())
			require.NoError(t, s.Migrate(context.Background()))
			t.Cleanup(func() {
				for _, q := range []string{"DELETE FROM oap_goal_execution_events", "DELETE FROM oap_goal_occurrences", "DELETE FROM oap_goal_events", "DELETE FROM oap_goal_receipts", "DELETE FROM oap_goals"} {
					_, err := c.Pool().Exec(context.Background(), q)
					assert.NoError(t, err)
				}
			})
			return s
		}},
	}
}

func TestOccurrenceStorageContract(t *testing.T) {
	for _, fixture := range durableFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.new(t)
			ledger := store.(goals.OccurrenceStore)
			s, _ := executionService(store)
			a := actor()
			g := approveExecution(t, s, a, "first")
			o, err := ledger.Schedule(ctx, g)
			require.NoError(t, err)
			repeated, err := ledger.Schedule(ctx, g)
			require.NoError(t, err)
			assert.Equal(t, o, repeated)
			assert.NotEmpty(t, o.SessionName)
			due, err := ledger.Due(ctx, executionNow, 10)
			require.NoError(t, err)
			assert.Empty(t, due)
			now := executionNow.Add(2 * time.Minute)
			due, err = ledger.Due(ctx, now, 10)
			require.NoError(t, err)
			require.Len(t, due, 1)
			r := goals.ClaimRequest{ID: o.ID, Worker: "one", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 1}
			claimed, err := ledger.Claim(ctx, r)
			require.NoError(t, err)
			assert.EqualValues(t, 1, claimed.Fence)
			r.Worker = "two"
			_, err = ledger.Claim(ctx, r)
			require.ErrorIs(t, err, goals.ErrConflict)
			running, err := ledger.Attach(ctx, claimed, "session-uid", now)
			require.NoError(t, err)
			_, err = ledger.Attach(ctx, running, "replacement-uid", now)
			require.ErrorIs(t, err, goals.ErrConflict)
			renewed, err := ledger.Renew(ctx, running, now.Add(10*time.Second), time.Minute)
			require.NoError(t, err)
			_, err = ledger.Finish(ctx, renewed, goals.OccurrenceSucceeded, renewed.LeaseUntil)
			require.ErrorIs(t, err, goals.ErrConflict)
			r.Now = renewed.LeaseUntil
			takeover, err := ledger.Claim(ctx, r)
			require.NoError(t, err)
			assert.Equal(t, renewed.SessionUID, takeover.SessionUID)
			assert.Equal(t, o.SessionName, takeover.SessionName)
			assert.EqualValues(t, 2, takeover.Fence)
			for _, stale := range []goals.Occurrence{claimed, running, renewed} {
				_, err = ledger.Attach(ctx, stale, "session-uid", r.Now)
				require.ErrorIs(t, err, goals.ErrConflict)
				_, err = ledger.Renew(ctx, stale, r.Now, time.Minute)
				require.ErrorIs(t, err, goals.ErrConflict)
				_, err = ledger.Finish(ctx, stale, goals.OccurrenceSucceeded, r.Now)
				require.ErrorIs(t, err, goals.ErrConflict)
			}
			second := approveExecution(t, s, a, "second")
			next, err := ledger.Schedule(ctx, second)
			require.NoError(t, err)
			unknown, err := ledger.Finish(ctx, takeover, goals.OccurrenceUnknown, r.Now)
			require.NoError(t, err)
			r.ID = next.ID
			_, err = ledger.Claim(ctx, r)
			require.ErrorIs(t, err, goals.ErrConflict, "unknown effect must keep its capacity reservation")
			_, err = ledger.Finish(ctx, unknown, goals.OccurrenceFailed, r.Now)
			require.NoError(t, err)
			next, err = ledger.Claim(ctx, r)
			require.NoError(t, err)
			_, err = ledger.Finish(ctx, next, goals.OccurrenceSucceeded, r.Now)
			require.ErrorIs(t, err, goals.ErrInvalid, "no session means no successful outcome")
			_, err = ledger.Finish(ctx, next, goals.OccurrenceCancelled, r.Now)
			require.NoError(t, err)
			third := approveExecution(t, s, a, "third")
			pausedOccurrence, err := ledger.Schedule(ctx, third)
			require.NoError(t, err)
			_, err = s.Update(ctx, a, goals.Change{ID: third.ID, Revision: third.Revision, RequestID: "pause-third", Action: "pause"})
			require.NoError(t, err)
			r.ID = pausedOccurrence.ID
			_, err = ledger.Claim(ctx, r)
			require.ErrorIs(t, err, goals.ErrConflict)
			due, err = ledger.Due(ctx, r.Now, 10)
			require.NoError(t, err)
			assert.Empty(t, due)
		})
	}
}

func TestSQLiteClaimRacesAndRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	stores := []goals.Store{openSQL(t, path), openSQL(t, path)}
	s, _ := executionService(stores[0])
	g := approveExecution(t, s, actor(), "race")
	ledger := stores[0].(goals.OccurrenceStore)
	o, err := ledger.Schedule(ctx, g)
	require.NoError(t, err)
	now := executionNow.Add(2 * time.Minute)
	start := make(chan struct{})
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := stores[i%2].(goals.OccurrenceStore).Claim(ctx, goals.ClaimRequest{ID: o.ID, Worker: fmt.Sprintf("worker-%d", i), Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 1})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, goals.ErrConflict)
		}
	}
	assert.Equal(t, 1, wins)
	claimed, err := ledger.Occurrence(ctx, o.ID)
	require.NoError(t, err)
	claimed, err = ledger.Attach(ctx, claimed, "live-session", now)
	require.NoError(t, err)
	reopened := openSQL(t, path).(goals.OccurrenceStore)
	got, err := reopened.Occurrence(ctx, o.ID)
	require.NoError(t, err)
	assert.Equal(t, claimed, got)
	got, err = reopened.Claim(ctx, goals.ClaimRequest{ID: o.ID, Worker: "restarted", Now: claimed.LeaseUntil, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 1})
	require.NoError(t, err)
	assert.Equal(t, "live-session", got.SessionUID)
	assert.Equal(t, claimed.SessionName, got.SessionName)
}

func TestDueQueueFairnessAndCapacityBoundaries(t *testing.T) {
	for _, fixture := range durableFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.new(t)
			ledger := store.(goals.OccurrenceStore)
			s, _ := executionService(store)
			a := actor()
			now := executionNow.Add(2 * time.Minute)
			for i := range 8 {
				g := approveExecution(t, s, a, fmt.Sprintf("backlog-%d", i))
				_, err := ledger.Schedule(ctx, g)
				require.NoError(t, err)
			}
			b := a
			b.Domain.Owner = "bob"
			g := approveExecution(t, s, b, "bob")
			bob, err := ledger.Schedule(ctx, g)
			require.NoError(t, err)
			due, err := ledger.Due(ctx, now, 2)
			require.NoError(t, err)
			require.Len(t, due, 2)
			owners := []string{due[0].Domain.Owner, due[1].Domain.Owner}
			assert.ElementsMatch(t, []string{a.Domain.Owner, b.Domain.Owner}, owners)
			r := goals.ClaimRequest{ID: bob.ID, Worker: "worker", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 1}
			_, err = ledger.Claim(ctx, r)
			require.NoError(t, err)
			for _, o := range due {
				if o.Domain.Owner == a.Domain.Owner {
					r.ID = o.ID
					_, err = ledger.Claim(ctx, r)
					require.ErrorIs(t, err, goals.ErrConflict, "class capacity spans owners")
					r.ClassLimit = 2
					_, err = ledger.Claim(ctx, r)
					require.NoError(t, err)
				}
			}
		})
	}
}

func TestConcurrentOccurrencesReserveCapacityAtomically(t *testing.T) {
	for _, fixture := range durableFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.new(t)
			s, _ := executionService(store)
			ledger := store.(goals.OccurrenceStore)
			var ids []string
			for i := range 12 {
				g := approveExecution(t, s, actor(), fmt.Sprintf("capacity-%d", i))
				o, err := ledger.Schedule(ctx, g)
				require.NoError(t, err)
				ids = append(ids, o.ID)
			}
			start := make(chan struct{})
			results := make(chan error, len(ids))
			var wg sync.WaitGroup
			for i, id := range ids {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := ledger.Claim(ctx, goals.ClaimRequest{ID: id, Worker: fmt.Sprintf("capacity-worker-%d", i), Now: executionNow.Add(2 * time.Minute), Lease: time.Minute, OwnerLimit: 1, ClassLimit: 12})
					results <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			wins := 0
			for err := range results {
				if err == nil {
					wins++
				} else {
					require.ErrorIs(t, err, goals.ErrConflict)
				}
			}
			assert.Equal(t, 1, wins, "capacity must be reserved across occurrences, not just per occurrence")
		})
	}
}

func TestOccurrenceAuditIsAtomicAndRecoverable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.db")
	client, err := memsqlite.NewClient(path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	store := goalsqlite.New(client.DB())
	require.NoError(t, store.Migrate(ctx))
	s, _ := executionService(store)
	g := approveExecution(t, s, actor(), "audit")
	o, err := store.Schedule(ctx, g)
	require.NoError(t, err)
	// A failed audit insert must roll back the claim and capacity reservation.
	_, err = client.DB().ExecContext(ctx, `CREATE TRIGGER fail_goal_claim_audit BEFORE INSERT ON oap_goal_execution_events WHEN NEW.payload LIKE '%"execution_claimed"%' BEGIN SELECT RAISE(ABORT,'test audit failure'); END`)
	require.NoError(t, err)
	r := goals.ClaimRequest{ID: o.ID, Worker: "worker", Now: executionNow.Add(2 * time.Minute), Lease: time.Minute, OwnerLimit: 1, ClassLimit: 1}
	_, err = store.Claim(ctx, r)
	require.ErrorContains(t, err, "test audit failure")
	got, err := store.Occurrence(ctx, o.ID)
	require.NoError(t, err)
	assert.Equal(t, o, got)
	_, err = client.DB().ExecContext(ctx, `DROP TRIGGER fail_goal_claim_audit`)
	require.NoError(t, err)
	claimed, err := store.Claim(ctx, r)
	require.NoError(t, err)
	events, err := store.Pending(ctx, 100)
	require.NoError(t, err)
	var audit []goals.Event
	for _, e := range events {
		if e.Occurrence != nil {
			audit = append(audit, e)
		}
	}
	require.Len(t, audit, 2)
	assert.Equal(t, "execution_scheduled", audit[0].Action)
	assert.Equal(t, "execution_claimed", audit[1].Action)
	assert.Equal(t, claimed, *audit[1].Occurrence)
	id := audit[1].ID
	envelope := []byte(`{"signed":"persisted dispatch audit"}`)
	require.ErrorIs(t, store.Published(ctx, id), goals.ErrConflict)
	require.NoError(t, store.SaveEnvelope(ctx, id, envelope))
	require.ErrorIs(t, store.SaveEnvelope(ctx, id, []byte(`{"signed":"changed"}`)), goals.ErrConflict)
	reopened := openSQL(t, path)
	saved, err := reopened.Envelope(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, string(envelope), string(saved))
	require.NoError(t, reopened.Published(ctx, id))
	events, err = reopened.Pending(ctx, 100)
	require.NoError(t, err)
	for _, e := range events {
		assert.NotEqual(t, id, e.ID)
	}
}

func TestGoalV1MigrationPreservesStateAndRejectsNewerSchemas(t *testing.T) {
	ctx := context.Background()
	client, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "migration.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	store := goalsqlite.New(client.DB())
	require.NoError(t, store.Migrate(ctx))
	g := create(t, service(store), "existing-v1-goal")
	// Reconstruct the previous schema: existing management rows, no ledger.
	for _, q := range []string{`DROP TABLE oap_goal_execution_events`, `DROP TABLE oap_goal_occurrences`, `DROP TABLE oap_goal_dispatch_lock`, `DELETE FROM oap_goal_schema`, `INSERT INTO oap_goal_schema(version) VALUES(1)`} {
		_, err = client.DB().ExecContext(ctx, q)
		require.NoError(t, err)
	}
	require.NoError(t, store.Migrate(ctx))
	require.NoError(t, store.Migrate(ctx))
	got, err := store.Get(ctx, actor().Domain, g.ID)
	require.NoError(t, err)
	assert.Equal(t, g, got)
	events, err := store.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, events, 1)
	_, err = client.DB().ExecContext(ctx, `INSERT INTO oap_goal_schema(version) VALUES(3)`)
	require.NoError(t, err)
	require.ErrorContains(t, store.Migrate(ctx), "newer than this operator")
}
