package sqlstore_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	mempostgres "github.com/authzed/openagentprimitives/pkg/memory/postgres"
	"github.com/authzed/openagentprimitives/pkg/memory/sessionevents/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/authzed/openagentprimitives/test/testpostgres"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { code := m.Run(); testpostgres.Stop(); os.Exit(code) }
func input() sessionevents.Input {
	return sessionevents.Input{Publisher: "system:operator", Sequence: 1, Observation: sessionevents.Observation{
		Source: sessionevents.Source{Kind: "native", Namespace: "team", ID: "team/source", UID: "source-uid"}, EventID: "trip-1", Kind: "trip.created", Subject: "trip-1", ObservedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), Data: []byte(`{"destination":"Example city"}`), Dependencies: []sessionevents.Dependency{{ResourceType: "agentsession", ResourceID: "team/source", Permission: "view_memory"}},
	}}
}
func TestDurableObservationContract(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var db *sql.DB
			sqlitePath := filepath.Join(t.TempDir(), "events.db")
			if backend == "sqlite" {
				c, err := memsqlite.NewClient(sqlitePath)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, c.Close()) })
				db = c.DB()
			} else {
				c, err := mempostgres.NewClient(ctx, testpostgres.URI(t))
				require.NoError(t, err)
				t.Cleanup(c.Close)
				db = stdlib.OpenDBFromPool(c.Pool())
				t.Cleanup(func() { require.NoError(t, db.Close()) })
			}
			store := sqlstore.New(db, backend == "postgres")
			require.NoError(t, store.Migrate(ctx))
			require.NoError(t, store.Migrate(ctx))
			i := input()
			accepted, err := store.Ingest(ctx, i, 0)
			require.NoError(t, err)
			// Reconstruct the store and retry the lost acknowledgement with its OLD
			// expected checkpoint. It must return the original record, not conflict.
			if backend == "sqlite" {
				require.NoError(t, db.Close())
				reopened, err := memsqlite.NewClient(sqlitePath)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reopened.Close()) })
				db = reopened.DB()
			}
			restarted := sqlstore.New(db, backend == "postgres")
			store = restarted
			repeated, err := restarted.Ingest(ctx, i, 0)
			require.NoError(t, err)
			require.Equal(t, accepted, repeated)
			cp, err := restarted.Checkpoint(ctx, i.Observation.Source, i.Publisher)
			require.NoError(t, err)
			require.EqualValues(t, 1, cp.Sequence)
			changed := i
			changed.Sequence = 2
			changed.Observation.Data = []byte(`{"destination":"Changed city"}`)
			_, err = store.Ingest(ctx, changed, 1)
			require.ErrorIs(t, err, sessionevents.ErrConflict)
			cp, err = store.Checkpoint(ctx, i.Observation.Source, i.Publisher)
			require.NoError(t, err)
			require.EqualValues(t, 1, cp.Sequence)
			// Same provider event re-attested at a new cursor advances ingestion only.
			redelivered := i
			redelivered.Sequence = 2
			redelivered.Observation.Witness = &sessionevents.Witness{Kind: "signed-memory-entry", Reference: "another", Digest: "another"}
			repeated, err = store.Ingest(ctx, redelivered, 1)
			require.NoError(t, err)
			require.Equal(t, accepted, repeated)
			cp, err = store.Checkpoint(ctx, i.Observation.Source, i.Publisher)
			require.NoError(t, err)
			require.EqualValues(t, 2, cp.Sequence)
			// A failed compare-and-swap must insert NEITHER evidence nor checkpoint.
			next := i
			next.Sequence = 3
			next.Observation.EventID = "trip-2"
			_, err = store.Ingest(ctx, next, 1)
			require.ErrorIs(t, err, sessionevents.ErrConflict)
			_, err = store.Get(ctx, next.Observation.Source, next.Observation.EventID)
			require.ErrorIs(t, err, sessionevents.ErrNotFound)
			next.Observation.ObservedAt = next.Observation.ObservedAt.Add(-time.Hour)
			_, err = store.Ingest(ctx, next, 2)
			require.NoError(t, err) // provider event time can be out of order
			// Independent workers sharing a checkpoint serialize. Only one event can
			// occupy the new position; the losing worker's event is not persisted.
			var wg sync.WaitGroup
			results := make(chan error, 2)
			for _, id := range []string{"trip-3", "trip-4"} {
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					candidate := i
					candidate.Sequence = 4
					candidate.Observation.EventID = id
					_, err := sqlstore.New(db, backend == "postgres").Ingest(ctx, candidate, 3)
					results <- err
				}(id)
			}
			wg.Wait()
			close(results)
			success, conflict := 0, 0
			for err := range results {
				if err == nil {
					success++
				} else if errors.Is(err, sessionevents.ErrConflict) {
					conflict++
				} else {
					require.NoError(t, err)
				}
			}
			require.Equal(t, 1, success)
			require.Equal(t, 1, conflict)
			persisted := 0
			for _, id := range []string{"trip-3", "trip-4"} {
				_, err := store.Get(ctx, i.Observation.Source, id)
				if err == nil {
					persisted++
				} else {
					require.ErrorIs(t, err, sessionevents.ErrNotFound)
				}
			}
			require.Equal(t, 1, persisted)
			cp, err = store.Checkpoint(ctx, i.Observation.Source, i.Publisher)
			require.NoError(t, err)
			require.EqualValues(t, 4, cp.Sequence)

			// New resource incarnation cannot inherit an old cursor or evidence.
			recreated := i
			recreated.Observation.Source.UID = "recreated-uid"
			cp, err = store.Checkpoint(ctx, recreated.Observation.Source, i.Publisher)
			require.NoError(t, err)
			require.Zero(t, cp.Sequence)
			_, err = store.Get(ctx, recreated.Observation.Source, i.Observation.EventID)
			require.ErrorIs(t, err, sessionevents.ErrNotFound)
			_, err = store.Ingest(ctx, recreated, 0)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO oap_session_observation_schema(version) VALUES(4)`)
			require.NoError(t, err)
			require.ErrorContains(t, store.Migrate(ctx), "newer than this operator")
			_, err = store.Get(ctx, i.Observation.Source, i.Observation.EventID)
			require.NoError(t, err)

		})
	}
}
