package sqlstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	mempostgres "github.com/authzed/openagentprimitives/pkg/memory/postgres"
	"github.com/authzed/openagentprimitives/pkg/memory/sessionevents/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/authzed/openagentprimitives/test/testpostgres"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func eventDB(t *testing.T, backend string) *sql.DB {
	t.Helper()
	if backend == "sqlite" {
		c, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "events.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, c.Close()) })
		return c.DB()
	}
	uri := testpostgres.URI(t)
	admin, err := mempostgres.NewClient(context.Background(), uri)
	require.NoError(t, err)
	schema := "events_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Pool().Exec(context.Background(), "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Pool().Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, err)
		admin.Close()
	})
	parsed, err := url.Parse(uri)
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	c, err := mempostgres.NewClient(context.Background(), parsed.String())
	require.NoError(t, err)
	t.Cleanup(c.Close)
	db := stdlib.OpenDBFromPool(c.Pool())
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}
func watch(id string, now time.Time) sessionevents.Subscription {
	return sessionevents.Subscription{ID: id, Principal: "owner", Target: "consumer-work", Authority: "reviewed-digest", Source: input().Observation.Source,
		Predicate: sessionevents.Predicate{Kind: "trip.created", Subject: "trip-1", Equals: map[string]string{"destination": "Example city"}}, StartsAt: now, EndsAt: now.Add(time.Hour), MaxRuns: 2, RunWindowSeconds: 600, Timezone: "UTC", Burst: "skip_pending"}
}
func TestFiniteEventAdmission(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			db := eventDB(t, backend)
			store := sqlstore.New(db, backend == "postgres")
			require.NoError(t, store.Migrate(ctx))
			now := input().Observation.ObservedAt
			sub := watch("watch", now)
			_, err := store.CreateSubscription(ctx, sub)
			require.NoError(t, err)
			changed := sub
			changed.MaxRuns++
			_, err = store.CreateSubscription(ctx, changed)
			require.ErrorIs(t, err, sessionevents.ErrConflict)
			i := input()
			_, err = store.Ingest(ctx, i, 0)
			require.NoError(t, err)
			first, err := store.Admit(ctx, sub.ID, sub.Source, i.Observation.EventID, now)
			require.NoError(t, err)
			require.Equal(t, "accepted", first.Disposition)
			// Both allowance and launch survive a reconstructed operator, and retrying
			// long after expiry retains the original admission rather than catching up.
			store = sqlstore.New(db, backend == "postgres")
			duplicate, err := store.Admit(ctx, sub.ID, sub.Source, i.Observation.EventID, now.Add(2*time.Hour))
			require.NoError(t, err)
			require.Equal(t, first, duplicate)
			state, err := store.Subscription(ctx, sub.ID)
			require.NoError(t, err)
			require.Equal(t, 1, state.Used)
			launches, err := store.Pending(ctx, now, 100)
			require.NoError(t, err)
			require.Len(t, launches, 1)
			require.Equal(t, first, launches[0].Admission)
			require.Equal(t, i.Observation.Dependencies, launches[0].Observation.Dependencies)
			// Burst skips are retained even once the first launch is acknowledged.
			i.Sequence++
			i.Observation.EventID = "burst"
			_, err = store.Ingest(ctx, i, 1)
			require.NoError(t, err)
			burst, err := store.Admit(ctx, sub.ID, sub.Source, i.Observation.EventID, now)
			require.NoError(t, err)
			require.Equal(t, "burst_skipped", burst.Disposition)
			require.NoError(t, store.Acknowledge(ctx, first.LaunchID))
			require.NoError(t, store.Acknowledge(ctx, first.LaunchID))
			repeated, err := store.Admit(ctx, sub.ID, sub.Source, i.Observation.EventID, now.Add(time.Minute))
			require.NoError(t, err)
			require.Equal(t, burst, repeated)
			launches, err = store.Pending(ctx, now, 100)
			require.NoError(t, err)
			require.Empty(t, launches)
			// Independent workers compete for the final reviewed run. Exactly one
			// allowance and launch is committed, regardless of which worker wins.
			for n := 3; n <= 4; n++ {
				i.Sequence = int64(n)
				i.Observation.EventID = fmt.Sprint("race-", n)
				_, err = store.Ingest(ctx, i, int64(n-1))
				require.NoError(t, err)
			}
			results := make(chan sessionevents.Admission, 2)
			failures := make(chan error, 2)
			var wg sync.WaitGroup
			for _, eventID := range []string{"race-3", "race-4"} {
				wg.Add(1)
				go func(eventID string) {
					defer wg.Done()
					a, err := sqlstore.New(db, backend == "postgres").Admit(ctx, sub.ID, sub.Source, eventID, now)
					results <- a
					failures <- err
				}(eventID)
			}
			wg.Wait()
			close(results)
			close(failures)
			for err := range failures {
				require.NoError(t, err)
			}
			counts := map[string]int{}
			for result := range results {
				counts[result.Disposition]++
			}
			require.Equal(t, map[string]int{"accepted": 1, "exhausted": 1}, counts)
			state, err = store.Subscription(ctx, sub.ID)
			require.NoError(t, err)
			require.Equal(t, 2, state.Used)
			launches, err = store.Pending(ctx, now, 100)
			require.NoError(t, err)
			require.Len(t, launches, 1)
			cancelledID := launches[0].Admission.LaunchID
			require.NoError(t, store.StopSubscription(ctx, sub.ID, "goal cancelled"))
			require.NoError(t, store.StopSubscription(ctx, sub.ID, "retry"))
			// Replaying activation cannot re-enable a stopped subscription or refund a
			// spent run. The cancelled intent is unavailable even to a stale worker.
			state, err = store.CreateSubscription(ctx, sub)
			require.NoError(t, err)
			require.True(t, state.Stopped)
			require.Equal(t, 2, state.Used)
			require.Equal(t, "goal cancelled", state.StopReason)
			launches, err = store.Pending(ctx, now, 100)
			require.NoError(t, err)
			require.Empty(t, launches)
			_, err = store.Launch(ctx, cancelledID)
			require.ErrorIs(t, err, sessionevents.ErrDenied)
			require.ErrorIs(t, store.Acknowledge(ctx, cancelledID), sessionevents.ErrConflict)
			wrongSource := sub.Source
			wrongSource.UID = "replacement"
			_, err = store.Admit(ctx, sub.ID, wrongSource, "trip-1", now)
			require.ErrorIs(t, err, sessionevents.ErrDenied)
		})
	}
}
func TestEventDeadlinesAndPredicates(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			store := sqlstore.New(eventDB(t, backend), backend == "postgres")
			require.NoError(t, store.Migrate(ctx))
			now := input().Observation.ObservedAt
			cases := []struct {
				name          string
				offset, ready time.Duration
				quiet         []sessionschedule.QuietHours
				data          string
				want          string
			}{
				{name: "quiet fits", quiet: []sessionschedule.QuietHours{{Start: "00:00", End: "00:05"}}, want: "accepted"},
				{name: "quiet deadline", quiet: []sessionschedule.QuietHours{{Start: "00:00", End: "00:10"}}, want: "quiet_expired"},
				{name: "backlog", ready: 10 * time.Minute, want: "expired"},
				{name: "before watch", offset: -time.Second, want: "outside_window"},
				{name: "watch ended", ready: time.Hour, want: "outside_window"},
				{name: "literal mismatch", data: `{"destination":{"text":"Example city"}}`, want: "unmatched"},
				{name: "null string", data: `{"destination":null}`, want: "unmatched"},
			}
			for n, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					sub := watch("watch-"+tc.name, now)
					sub.QuietHours = tc.quiet
					_, err := store.CreateSubscription(ctx, sub)
					require.NoError(t, err)
					i := input()
					i.Sequence = int64(n + 1)
					i.Observation.EventID = tc.name
					i.Observation.ObservedAt = now.Add(tc.offset)
					if tc.data != "" {
						i.Observation.Data = []byte(tc.data)
					}
					_, err = store.Ingest(ctx, i, int64(n))
					require.NoError(t, err)
					a, err := store.Admit(ctx, sub.ID, sub.Source, i.Observation.EventID, now.Add(tc.ready))
					require.NoError(t, err)
					require.Equal(t, tc.want, a.Disposition)
					if tc.want == "accepted" {
						require.Equal(t, now.Add(5*time.Minute), a.Window.DueAt)
						require.Equal(t, now.Add(10*time.Minute), a.Window.ExpiresAt)
					}
					again, err := store.Admit(ctx, sub.ID, sub.Source, i.Observation.EventID, now.Add(20*time.Minute))
					require.NoError(t, err)
					require.Equal(t, a, again)
				})
			}
		})
	}
}

type triggerAuthority struct{ deny, denyObservation bool }

func (a *triggerAuthority) CheckSubscription(context.Context, sessionevents.Subscription) error {
	if a.deny {
		return sessionevents.ErrDenied
	}
	return nil
}
func (a *triggerAuthority) CheckObservation(context.Context, sessionevents.Subscription, sessionevents.Observation) error {
	if a.denyObservation {
		return sessionevents.ErrDenied
	}
	return nil
}
func TestTriggerAuthorityCheckedOnReplayAndLaunch(t *testing.T) {
	ctx := context.Background()
	store := sqlstore.New(eventDB(t, "sqlite"), false)
	require.NoError(t, store.Migrate(ctx))
	now := input().Observation.ObservedAt
	sub := watch("watch", now)
	auth := &triggerAuthority{}
	triggers := sessionevents.Triggers{Store: store, Observations: store, Authority: auth}
	_, err := triggers.Activate(ctx, sub)
	require.NoError(t, err)
	_, err = store.Ingest(ctx, input(), 0)
	require.NoError(t, err)
	a, err := triggers.Accept(ctx, sub.ID, sub.Source, "trip-1", now)
	require.NoError(t, err)
	launch, err := store.Launch(ctx, a.LaunchID)
	require.NoError(t, err)
	require.NoError(t, triggers.CheckLaunch(ctx, launch, now))
	tampered := launch
	tampered.Observation.Data = []byte(`{"instruction":"widen scope"}`)
	require.ErrorIs(t, triggers.CheckLaunch(ctx, tampered, now), sessionevents.ErrDenied)
	require.ErrorIs(t, triggers.CheckLaunch(ctx, launch, a.Window.ExpiresAt), sessionevents.ErrDenied)
	auth.denyObservation = true
	_, err = triggers.Accept(ctx, sub.ID, sub.Source, "trip-1", now)
	require.ErrorIs(t, err, sessionevents.ErrDenied)
	require.ErrorIs(t, triggers.CheckLaunch(ctx, launch, now), sessionevents.ErrDenied)
	auth.denyObservation = false
	auth.deny = true
	_, err = triggers.Accept(ctx, sub.ID, sub.Source, "trip-1", now)
	require.ErrorIs(t, err, sessionevents.ErrDenied)
	require.ErrorIs(t, triggers.CheckLaunch(ctx, launch, now), sessionevents.ErrDenied)
	state, err := store.Subscription(ctx, sub.ID)
	require.NoError(t, err)
	require.Equal(t, 1, state.Used)
	var empty *sessionevents.Triggers
	_, err = empty.Activate(ctx, sub)
	require.True(t, errors.Is(err, sessionevents.ErrDenied))
}

func TestSubscriptionSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	db, err := memsqlite.NewClient(path)
	require.NoError(t, err)
	store := sqlstore.New(db.DB(), false)
	require.NoError(t, store.Migrate(ctx))
	now := input().Observation.ObservedAt
	sub := watch("watch", now)
	_, err = store.CreateSubscription(ctx, sub)
	require.NoError(t, err)
	_, err = store.Ingest(ctx, input(), 0)
	require.NoError(t, err)
	accepted, err := store.Admit(ctx, sub.ID, sub.Source, "trip-1", now)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = memsqlite.NewClient(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store = sqlstore.New(db.DB(), false)
	require.NoError(t, store.Migrate(ctx))
	replay, err := store.Admit(ctx, sub.ID, sub.Source, "trip-1", now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, accepted, replay)
	launches, err := store.Pending(ctx, now.Add(time.Second), 100)
	require.NoError(t, err)
	require.Len(t, launches, 1)
	state, err := store.Subscription(ctx, sub.ID)
	require.NoError(t, err)
	require.Equal(t, 1, state.Used)
	// Old launch expiration clears burst blocking, but never refunds the use.
	i := input()
	i.Sequence = 2
	i.Observation.EventID = "fresh"
	i.Observation.ObservedAt = now.Add(11 * time.Minute)
	_, err = store.Ingest(ctx, i, 1)
	require.NoError(t, err)
	fresh, err := store.Admit(ctx, sub.ID, sub.Source, "fresh", i.Observation.ObservedAt)
	require.NoError(t, err)
	require.Equal(t, "accepted", fresh.Disposition)
	_, err = store.Launch(ctx, accepted.LaunchID)
	require.ErrorIs(t, err, sessionevents.ErrDenied)
	state, err = store.Subscription(ctx, sub.ID)
	require.NoError(t, err)
	require.Equal(t, 2, state.Used)
}

type lostAckStore struct {
	sessionevents.TriggerStore
	fail bool
}

func (s *lostAckStore) Acknowledge(ctx context.Context, id string) error {
	if s.fail {
		s.fail = false
		return errors.New("lost acknowledgment")
	}
	return s.TriggerStore.Acknowledge(ctx, id)
}

type retainedConsumer struct{ db *sql.DB }

func (c *retainedConsumer) Materialize(ctx context.Context, launch sessionevents.Launch) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO retained_test_launches(id) VALUES(?) ON CONFLICT(id) DO NOTHING`, launch.Admission.LaunchID)
	return err
}
func TestDispatchLostAcknowledgmentDoesNotCreateSecondRequest(t *testing.T) {
	ctx := context.Background()
	db := eventDB(t, "sqlite")
	store := sqlstore.New(db, false)
	require.NoError(t, store.Migrate(ctx))
	_, err := db.ExecContext(ctx, `CREATE TABLE retained_test_launches(id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	now := input().Observation.ObservedAt
	sub := watch("watch", now)
	auth := &triggerAuthority{}
	triggers := &sessionevents.Triggers{Store: &lostAckStore{TriggerStore: store, fail: true}, Observations: store, Authority: auth}
	_, err = triggers.Activate(ctx, sub)
	require.NoError(t, err)
	_, err = store.Ingest(ctx, input(), 0)
	require.NoError(t, err)
	_, err = triggers.Accept(ctx, sub.ID, sub.Source, "trip-1", now)
	require.NoError(t, err)
	dispatcher := sessionevents.Dispatcher{Triggers: triggers, Consumer: &retainedConsumer{db: db}}
	count, err := dispatcher.Drain(ctx, now, 100)
	require.ErrorContains(t, err, "lost acknowledgment")
	require.Zero(t, count)
	// Reconstruct every worker collaborator after the post-commit failure.
	restarted := sqlstore.New(db, false)
	dispatcher = sessionevents.Dispatcher{Triggers: &sessionevents.Triggers{Store: restarted, Observations: restarted, Authority: auth}, Consumer: &retainedConsumer{db: db}}
	count, err = dispatcher.Drain(ctx, now, 100)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	count, err = dispatcher.Drain(ctx, now, 100)
	require.NoError(t, err)
	require.Zero(t, count)
	var requests int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM retained_test_launches`).Scan(&requests))
	require.Equal(t, 1, requests)
	state, err := store.Subscription(ctx, sub.ID)
	require.NoError(t, err)
	require.Equal(t, 1, state.Used)
}
