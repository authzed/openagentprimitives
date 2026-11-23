package goals_test

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	goalsql "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	mempostgres "github.com/authzed/openagentprimitives/pkg/memory/postgres"
	eventsql "github.com/authzed/openagentprimitives/pkg/memory/sessionevents/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/authzed/openagentprimitives/test/testpostgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func bridgeDB(t *testing.T, backend string) *sql.DB {
	t.Helper()
	if backend == "sqlite" {
		c, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "bridge.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, c.Close()) })
		return c.DB()
	}
	uri := testpostgres.URI(t)
	admin, err := mempostgres.NewClient(context.Background(), uri)
	require.NoError(t, err)
	schema := "bridge_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Pool().Exec(context.Background(), "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Pool().Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, err)
		admin.Close()
	})
	u, err := url.Parse(uri)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	c, err := mempostgres.NewClient(context.Background(), u.String())
	require.NoError(t, err)
	t.Cleanup(c.Close)
	db := stdlib.OpenDBFromPool(c.Pool())
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

type eventAccess struct{ denied bool }

func (a *eventAccess) Check(context.Context, string, sessionevents.Source, []sessionevents.Dependency) error {
	if a.denied {
		return sessionevents.ErrDenied
	}
	return nil
}
func approveWatch(t *testing.T, s *goals.Service, key string) goals.Goal {
	t.Helper()
	ctx := context.Background()
	g, err := s.Create(ctx, actor(), goals.CreateRequest{RequestID: key + "-create", Title: "Watch", Outcome: "Report matching events privately"})
	require.NoError(t, err)
	g, err = s.Update(ctx, actor(), goals.Change{ID: g.ID, Revision: g.Revision, RequestID: key + "-activate", Action: "activate"})
	require.NoError(t, err)
	tm := terms(actor())
	tm.Event = &goals.EventWatch{Source: sessionevents.Source{Kind: "native", Namespace: "team", ID: "team/source", UID: "source-uid"}, Predicate: sessionevents.Predicate{Kind: "trip.changed", Subject: "trip-1"}, MaxRuns: 2, RunWindowSeconds: 300, Timezone: "UTC", Burst: "skip_pending"}
	g, err = s.RequestExecution(ctx, actor(), goals.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: key + "-request", Terms: tm})
	require.NoError(t, err)
	g, err = s.DecideExecution(ctx, g.Domain, g.ID, goals.ExecutionDecision{RequestID: key + "-approve", Digest: g.Execution.Digest, Owner: g.Domain.Owner, Approved: true, Witness: "test-human-decision"})
	require.NoError(t, err)
	return g
}
func TestEventGoalBridge(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			db := bridgeDB(t, backend)
			store := goalsql.New(db, backend == "postgres")
			require.NoError(t, store.Migrate(ctx))
			events := eventsql.New(db, backend == "postgres")
			require.NoError(t, events.Migrate(ctx))
			s, auth := executionService(store)
			access := &eventAccess{}
			e := &goals.EventExecution{Service: s, Sources: access}
			triggers := &sessionevents.Triggers{Store: events, Observations: events, Authority: e}
			e.Triggers = triggers
			s.Events = e
			g := approveWatch(t, s, "watch")
			require.NoError(t, e.Activate(ctx, g), "consent can activate a future watch")
			sub, err := goals.EventSubscription(g)
			require.NoError(t, err)
			_, err = store.Schedule(ctx, g)
			require.ErrorIs(t, err, goals.ErrDenied, "event watches cannot manufacture calendar occurrences")
			alias := sub
			alias.ID = "second-budget"
			_, err = triggers.Activate(ctx, alias)
			require.ErrorIs(t, err, goals.ErrDenied, "one consent has one watch identity")
			now := g.Execution.Terms.DueAt.Add(time.Second)
			s.Now = func() time.Time { return now }
			input := sessionevents.Input{Publisher: "system:channelsd", Sequence: 1, Observation: sessionevents.Observation{Source: sub.Source, EventID: "change-1", Kind: "trip.changed", Subject: "trip-1", ObservedAt: now, Data: []byte(`{"status":"delayed"}`), Dependencies: []sessionevents.Dependency{{ResourceType: "agentsession", ResourceID: "team/source", Permission: "view_memory"}}}}
			_, err = events.Ingest(ctx, input, 0)
			require.NoError(t, err)
			// Intake committed before the router existed. Reconstruct it; durable
			// evidence still creates one admission and one occurrence after restart.
			router := &sessionevents.Router{Triggers: triggers, Store: eventsql.New(db, backend == "postgres")}
			require.NoError(t, router.Tick(ctx, now))
			pending, err := events.Pending(ctx, now, 100)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.NoError(t, e.Materialize(ctx, pending[0]))
			// Simulate losing the outbox acknowledgment after occurrence commit.
			dispatch := &sessionevents.Dispatcher{Triggers: triggers, Consumer: e}
			n, err := dispatch.Drain(ctx, now, 100)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			n, err = dispatch.Drain(ctx, now, 100)
			require.NoError(t, err)
			require.Zero(t, n)
			page, err := store.Runs(ctx, g.Domain, g.ID, goals.ListRequest{Limit: 100})
			require.NoError(t, err)
			require.Len(t, page.Runs, 1)
			o := page.Runs[0]
			require.NotNil(t, o.Event)
			require.Equal(t, input.Observation.Data, o.Event.Observation.Data)
			require.NoError(t, s.DispatchOccurrence(ctx, g, o))
			require.Len(t, o.ReadDependencies(), 1)
			forged := o
			forged.ExpiresAt = forged.ExpiresAt.Add(time.Second)
			require.ErrorIs(t, s.DispatchOccurrence(ctx, g, forged), goals.ErrDenied)
			forged = o
			forged.Event = nil
			require.ErrorIs(t, s.DispatchOccurrence(ctx, g, forged), goals.ErrDenied)
			claimed, err := store.Claim(ctx, goals.ClaimRequest{ID: o.ID, Worker: "worker", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 4})
			require.NoError(t, err)
			require.Equal(t, o.Event, claimed.Event)
			state, err := events.Subscription(ctx, sub.ID)
			require.NoError(t, err)
			require.Equal(t, 1, state.Used)
			// A second event at the exact same due time retains a distinct identity.
			input.Sequence = 2
			input.Observation.EventID = "change-2"
			_, err = events.Ingest(ctx, input, 1)
			require.NoError(t, err)
			router.After = ""
			require.NoError(t, router.Tick(ctx, now))
			n, err = dispatch.Drain(ctx, now, 100)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			page, err = store.Runs(ctx, g.Domain, g.ID, goals.ListRequest{Limit: 100})
			require.NoError(t, err)
			require.Len(t, page.Runs, 2)
			require.Equal(t, page.Runs[0].DueAt, page.Runs[1].DueAt)
			auth.grantErr = goals.ErrDenied
			require.ErrorIs(t, s.DispatchOccurrence(ctx, g, o), goals.ErrDenied)
			auth.grantErr = nil
			access.denied = true
			require.ErrorIs(t, s.DispatchOccurrence(ctx, g, o), goals.ErrDenied)
			router.After = ""
			require.NoError(t, router.Tick(ctx, now))
			state, err = events.Subscription(ctx, sub.ID)
			require.NoError(t, err)
			require.True(t, state.Stopped)
			access.denied = false
			require.NoError(t, e.Activate(ctx, g))
			state, err = events.Subscription(ctx, sub.ID)
			require.NoError(t, err)
			require.True(t, state.Stopped, "replaying consent cannot resurrect a revoked watch")
		})
	}
}
func TestEventTermsCannotMixCalendarOrExceedWatchBounds(t *testing.T) {
	ctx := context.Background()
	s, _ := executionService(openSQL(t, filepath.Join(t.TempDir(), "bounds.db")))
	g := approveWatch(t, s, "bounds")
	tm := g.Execution.Terms
	tm.Schedule = &sessionschedule.Spec{Kind: "once", Timezone: "UTC", MaxRuns: 1, RunWindowSeconds: 60}
	_, err := s.RequestExecution(ctx, actor(), goals.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: "mixed", Terms: tm})
	require.ErrorIs(t, err, goals.ErrInvalid)
	tm.Schedule = nil
	tm.Event.MaxRuns = 101
	_, err = s.RequestExecution(ctx, actor(), goals.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: "unbounded", Terms: tm})
	require.ErrorIs(t, err, goals.ErrInvalid)
}
