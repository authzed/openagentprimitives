package goals_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/authzed/openagentprimitives/test/testpostgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requestSeries(t *testing.T, s *goals.Service, key string) (goals.Goal, goals.ExecutionRequest) {
	t.Helper()
	ctx := context.Background()
	a := actor()
	g, err := s.Create(ctx, a, goals.CreateRequest{RequestID: key + "-create", Title: "Water", Outcome: "Send a private reminder"})
	require.NoError(t, err)
	g, err = s.Update(ctx, a, goals.Change{ID: g.ID, Revision: g.Revision, RequestID: key + "-activate", Action: "activate"})
	require.NoError(t, err)
	terms := terms(a)
	terms.Schedule = &sessionschedule.Spec{Kind: "interval", Timezone: "UTC", IntervalSeconds: 60, MaxRuns: 3, RunWindowSeconds: 30}
	req := goals.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: key + "-request", Terms: terms}
	g, err = s.RequestExecution(ctx, a, req)
	require.NoError(t, err)
	return g, req
}
func approveSeries(t *testing.T, s *goals.Service, key string) goals.Goal {
	t.Helper()
	g, _ := requestSeries(t, s, key)
	g, err := s.DecideExecution(context.Background(), g.Domain, g.ID, goals.ExecutionDecision{RequestID: key + "-approve", Digest: g.Execution.Digest, Owner: g.Domain.Owner, Approved: true, Witness: "test-human-decision"})
	require.NoError(t, err)
	return g
}
func TestSeriesConsentPinsWindowsAndRetries(t *testing.T) {
	ctx := context.Background()
	s, _ := executionService(openSQL(t, filepath.Join(t.TempDir(), "series.db")))
	g, req := requestSeries(t, s, "series")
	require.Len(t, g.Execution.Terms.ScheduleWindows, 3)
	// Caller-chosen windows cannot expand reviewed authority.
	req.Terms.ScheduleWindows = []sessionschedule.Window{{DueAt: executionNow, ExpiresAt: executionNow.Add(24 * time.Hour)}}
	s.Now = func() time.Time { return executionNow.Add(10 * time.Minute) }
	replay, err := s.RequestExecution(ctx, actor(), req)
	require.NoError(t, err)
	assert.Equal(t, g, replay, "retry survives passing dueAt")
	req.Terms.Schedule.QuietHours = []sessionschedule.QuietHours{{Start: "00:00", End: "08:00"}}
	_, err = s.RequestExecution(ctx, actor(), req)
	require.ErrorIs(t, err, goals.ErrConflict, "changing quiet hours requires new consent")
}
func TestSeriesStorageAndMissedWindows(t *testing.T) {
	for _, fixture := range durableFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.new(t)
			s, _ := executionService(store)
			g := approveSeries(t, s, "series")
			ledger := store.(goals.OccurrenceStore)
			sweeper := store.(goals.QueuedSweeper)
			// Independent workers replay consent publication without creating duplicates.
			var wg sync.WaitGroup
			errch := make(chan error, 2)
			for range 2 {
				wg.Add(1)
				go func() { defer wg.Done(); _, err := ledger.Schedule(ctx, g); errch <- err }()
			}
			wg.Wait()
			close(errch)
			for err := range errch {
				require.NoError(t, err)
			}
			history, err := store.(goals.RunStore).Runs(ctx, g.Domain, g.ID, goals.ListRequest{Limit: 100})
			require.NoError(t, err)
			require.Len(t, history.Runs, 3)
			assert.NotEqual(t, history.Runs[0].SessionName, history.Runs[1].SessionName)
			windows := g.Execution.Terms.ScheduleWindows
			now := windows[1].DueAt
			s.Now = func() time.Time { return now }
			count, err := sweeper.SweepQueued(ctx, now, 100)
			require.NoError(t, err)
			assert.Equal(t, 1, count)
			due, err := ledger.Due(ctx, now, 100)
			require.NoError(t, err)
			require.Len(t, due, 1)
			assert.Equal(t, windows[1].DueAt, due[0].DueAt)
			require.NoError(t, s.DispatchOccurrence(ctx, g, due[0]))
			wrong := due[0]
			wrong.ExpiresAt = wrong.ExpiresAt.Add(time.Second)
			require.ErrorIs(t, s.DispatchOccurrence(ctx, g, wrong), goals.ErrDenied, "the series grant cannot expand one occurrence")
			forgedGoal := g
			forgedConsent := *g.Execution
			forgedGoal.Execution = &forgedConsent
			forgedGoal.Execution.Terms.ScheduleWindows = []sessionschedule.Window{{DueAt: wrong.DueAt, ExpiresAt: wrong.ExpiresAt}}
			require.ErrorIs(t, s.DispatchOccurrence(ctx, forgedGoal, wrong), goals.ErrDenied, "window membership is re-read from durable consent")
			wrong = due[0]
			wrong.Domain.Owner = "another"
			require.ErrorIs(t, s.DispatchOccurrence(ctx, g, wrong), goals.ErrDenied)
			claimed, err := ledger.Claim(ctx, goals.ClaimRequest{ID: due[0].ID, Worker: "operator", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 4})
			require.NoError(t, err)
			// The missed-run sweeper cannot release a live capacity reservation.
			count, err = sweeper.SweepQueued(ctx, windows[2].ExpiresAt, 100)
			require.NoError(t, err)
			assert.Equal(t, 1, count)
			current, err := ledger.Occurrence(ctx, claimed.ID)
			require.NoError(t, err)
			assert.Equal(t, goals.OccurrenceClaimed, current.State)
			history, err = store.(goals.RunStore).Runs(ctx, g.Domain, g.ID, goals.ListRequest{Limit: 100})
			require.NoError(t, err)
			skipped := 0
			for _, run := range history.Runs {
				if run.State == goals.OccurrenceSkipped {
					skipped++
					require.NotNil(t, run.Outcome)
					assert.Equal(t, goals.RunMissed, run.Outcome.Reason)
					assert.Equal(t, "none", run.Outcome.Effects)
					assert.Empty(t, run.SessionUID)
					_, err = ledger.Claim(ctx, goals.ClaimRequest{ID: run.ID, Worker: "late", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 4})
					require.ErrorIs(t, err, goals.ErrConflict)
				}
			}
			assert.Equal(t, 2, skipped)
		})
	}
}
func TestSeriesSurvivesStoreReopenAndPause(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	s, _ := executionService(openSQL(t, path))
	g := approveSeries(t, s, "series")
	_, err := s.Store.(goals.OccurrenceStore).Schedule(ctx, g)
	require.NoError(t, err)
	reopened := openSQL(t, path)
	s2, _ := executionService(reopened)
	history, err := reopened.(goals.RunStore).Runs(ctx, g.Domain, g.ID, goals.ListRequest{Limit: 100})
	require.NoError(t, err)
	require.Len(t, history.Runs, 3)
	paused, err := s2.Update(ctx, actor(), goals.Change{ID: g.ID, Revision: g.Revision, RequestID: "pause", Action: "pause"})
	require.NoError(t, err)
	count, err := reopened.(goals.QueuedSweeper).SweepQueued(ctx, executionNow, 100)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	history, err = reopened.(goals.RunStore).Runs(ctx, g.Domain, g.ID, goals.ListRequest{Limit: 100})
	require.NoError(t, err)
	for _, run := range history.Runs {
		assert.Equal(t, goals.OccurrenceCancelled, run.State)
		require.NotNil(t, run.Outcome)
		assert.Equal(t, goals.RunPaused, run.Outcome.Reason)
	}
	_, err = s2.Update(ctx, actor(), goals.Change{ID: g.ID, Revision: paused.Revision, RequestID: "resume", Action: "resume"})
	require.NoError(t, err)
	_, err = reopened.(goals.OccurrenceStore).Schedule(ctx, g)
	require.ErrorIs(t, err, goals.ErrConflict, "old consent never revives on resume")
}

func checkScheduleMigration(t *testing.T, db *sql.DB, postgres bool) {
	t.Helper()
	ctx := context.Background()
	store := sqlstore.New(db, postgres)
	require.NoError(t, store.Migrate(ctx))
	s, _ := executionService(store)
	g := approveExecution(t, s, actor(), "legacy")
	run, err := store.Schedule(ctx, g)
	require.NoError(t, err)
	now := g.Execution.Terms.DueAt
	run, err = store.Claim(ctx, goals.ClaimRequest{ID: run.ID, Worker: "existing-worker", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 4})
	require.NoError(t, err)
	run, err = store.Attach(ctx, run, "existing-root-uid", now)
	require.NoError(t, err)
	events, err := store.Pending(ctx, 100)
	require.NoError(t, err)
	var queries []string
	if postgres {
		queries = []string{`ALTER TABLE oap_goal_occurrences ADD CONSTRAINT oap_goal_occurrences_domain_goal_id_goal_revision_key UNIQUE(domain,goal_id,goal_revision)`}
	} else {
		var ddl string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='oap_goal_occurrences'`).Scan(&ddl))
		legacyDDL := strings.Replace(ddl, "UNIQUE(domain,goal_id,goal_revision,due_at)", "UNIQUE(domain,goal_id,goal_revision)", 1)
		require.NotEqual(t, ddl, legacyDDL)
		queries = []string{`ALTER TABLE oap_goal_occurrences RENAME TO oap_goal_occurrences_old`, legacyDDL, `INSERT INTO oap_goal_occurrences SELECT * FROM oap_goal_occurrences_old`, `DROP TABLE oap_goal_occurrences_old`}
	}
	queries = append(queries, `DELETE FROM oap_goal_schema`, `INSERT INTO oap_goal_schema(version) VALUES(5)`)
	for _, query := range queries {
		_, err = db.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	require.NoError(t, store.Migrate(ctx))
	require.NoError(t, store.Migrate(ctx))
	recovered, err := store.Occurrence(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, run, recovered, "identity and live fence survive migration")
	after, err := store.Pending(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, events, after, "audit publication state survives migration")
	recurring := approveSeries(t, s, "after-upgrade")
	_, err = store.Schedule(ctx, recurring)
	require.NoError(t, err)
	history, err := store.Runs(ctx, recurring.Domain, recurring.ID, goals.ListRequest{Limit: 100})
	require.NoError(t, err)
	assert.Len(t, history.Runs, 3)
}
func TestScheduleMigrationPreservesLegacyLedger(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "v5.db"))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, db.Close()) })
		checkScheduleMigration(t, db.DB(), false)
	})
	t.Run("postgres", func(t *testing.T) {
		ctx := context.Background()
		pool, err := pgxpool.New(ctx, testpostgres.URI(t))
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		// Isolate the legacy schema from the shared test database's other fixtures.
		schema := fmt.Sprintf("goal_schedule_migration_%d", time.Now().UnixNano())
		_, err = pool.Exec(ctx, "CREATE SCHEMA "+schema)
		require.NoError(t, err)
		t.Cleanup(func() { _, err := pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); assert.NoError(t, err) })
		config := pool.Config().Copy()
		config.ConnConfig.RuntimeParams["search_path"] = schema
		scoped, err := pgxpool.NewWithConfig(ctx, config)
		require.NoError(t, err)
		t.Cleanup(scoped.Close)
		db := stdlib.OpenDBFromPool(scoped)
		t.Cleanup(func() { assert.NoError(t, db.Close()) })
		checkScheduleMigration(t, db, true)
	})
}
