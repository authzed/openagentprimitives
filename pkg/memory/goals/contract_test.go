package goals_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

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

func TestMain(m *testing.M) { code := m.Run(); testpostgres.Stop(); os.Exit(code) }

type allow struct{}

func (allow) Authorize(context.Context, goals.Actor, bool) error           { return nil }
func (allow) Sources(context.Context, goals.Actor) ([]goals.Source, error) { return nil, nil }
func (allow) ReadGoal(context.Context, goals.Actor, goals.Goal) error      { return nil }
func actor() goals.Actor {
	return goals.Actor{Domain: goals.Domain{Namespace: "team", Owner: "alice", Class: "assistant", ClassUID: "class-uid"}, Session: "team/session-1", Proof: "inbound-1"}
}
func openSQL(t *testing.T, path string) goals.Store {
	t.Helper()
	c, err := memsqlite.NewClient(path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, c.Close()) })
	s := goalsqlite.New(c.DB())
	require.NoError(t, s.Migrate(context.Background()))
	return s
}
func service(s goals.Store) *goals.Service { return &goals.Service{Store: s, Auth: allow{}} }
func create(t *testing.T, s *goals.Service, key string) goals.Goal {
	t.Helper()
	g, err := s.Create(context.Background(), actor(), goals.CreateRequest{RequestID: key, Title: "Prepare meeting", Outcome: "Gather the agenda"})
	require.NoError(t, err)
	return g
}

func TestStorageContract(t *testing.T) {
	for _, fixture := range []struct {
		name string
		new  func(*testing.T) goals.Store
	}{
		{"inmem", func(*testing.T) goals.Store { return inmem.New() }},
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
		{"sqlite", func(t *testing.T) goals.Store { return openSQL(t, filepath.Join(t.TempDir(), "goals.db")) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.new(t)
			s := service(store)
			a := actor()
			g := create(t, s, "create")
			// A transport retry returns the originally accepted result, even after
			// subsequent revisions, and conflicting reuse never changes state.
			next, err := s.Update(ctx, a, goals.Change{ID: g.ID, Revision: 1, RequestID: "activate", Action: "activate"})
			require.NoError(t, err)
			assert.Equal(t, goals.Active, next.State)
			replayed := create(t, s, "create")
			assert.Equal(t, g, replayed)
			_, err = s.Create(ctx, a, goals.CreateRequest{RequestID: "create", Title: "Different", Outcome: "Different"})
			require.ErrorIs(t, err, goals.ErrConflict)
			_, err = s.Update(ctx, a, goals.Change{ID: g.ID, Revision: 1, RequestID: "stale", Action: "cancel"})
			require.ErrorIs(t, err, goals.ErrConflict)
			for _, other := range []goals.Domain{
				{Namespace: "team", Owner: "bob", Class: "assistant", ClassUID: "class-uid"},
				{Namespace: "team", Owner: "alice", Class: "assistant", ClassUID: "replacement-uid"},
				{Namespace: "other", Owner: "alice", Class: "assistant", ClassUID: "class-uid"},
			} {
				b := a
				b.Domain = other
				_, err := s.Get(ctx, b, g.ID)
				require.ErrorIs(t, err, goals.ErrNotFound)
				page, err := s.List(ctx, b, goals.ListRequest{})
				require.NoError(t, err)
				assert.Empty(t, page.Goals)
			}
			_, err = s.Update(ctx, a, goals.Change{ID: g.ID, Revision: 2, RequestID: "complete", Action: "complete"})
			require.ErrorIs(t, err, goals.ErrInvalid)
			terminal, err := s.Update(ctx, a, goals.Change{ID: g.ID, Revision: 2, RequestID: "cancel", Action: "cancel"})
			require.NoError(t, err)
			assert.Equal(t, goals.Cancelled, terminal.State)
			_, err = s.Update(ctx, a, goals.Change{ID: g.ID, Revision: 3, RequestID: "resume", Action: "resume"})
			require.ErrorIs(t, err, goals.ErrInvalid)
			events, err := store.Pending(ctx, 100)
			require.NoError(t, err)
			require.Len(t, events, 3)
			for _, e := range events {
				assert.Equal(t, a.Proof, e.Proof)
				require.ErrorIs(t, store.Published(ctx, e.ID), goals.ErrConflict)
				raw := json.RawMessage(`{"signed":"original"}`)
				require.NoError(t, store.SaveEnvelope(ctx, e.ID, raw))
				require.NoError(t, store.SaveEnvelope(ctx, e.ID, raw))
				require.ErrorIs(t, store.SaveEnvelope(ctx, e.ID, json.RawMessage(`{"signed":"different"}`)), goals.ErrConflict)
				require.NoError(t, store.Published(ctx, e.ID))
			}
			events, err = store.Pending(ctx, 100)
			require.NoError(t, err)
			assert.Empty(t, events)
		})
	}
}

func TestSQLiteIndependentConnectionsAndRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "goals.db")
	first, second := openSQL(t, path), openSQL(t, path)
	s1, s2 := service(first), service(second)
	g := create(t, s1, "create")
	start := make(chan struct{})
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s := s1
			if i%2 == 1 {
				s = s2
			}
			_, err := s.Update(ctx, actor(), goals.Change{ID: g.ID, Revision: 1, RequestID: fmt.Sprintf("cancel-%d", i), Action: "cancel"})
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
			assert.True(t, errors.Is(err, goals.ErrConflict), "%v", err)
		}
	}
	assert.Equal(t, 1, wins)
	reopened := openSQL(t, path)
	got, err := reopened.Get(ctx, actor().Domain, g.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Revision)
	assert.Equal(t, goals.Cancelled, got.State)
	events, err := reopened.Pending(ctx, 100)
	require.NoError(t, err)
	assert.Len(t, events, 2)
}

func TestNoAuthorizerFailsClosed(t *testing.T) {
	s := &goals.Service{Store: inmem.New()}
	_, err := s.Create(context.Background(), actor(), goals.CreateRequest{RequestID: "x", Title: "x", Outcome: "x"})
	require.ErrorIs(t, err, goals.ErrDenied)
}

func TestSQLiteAuditEnvelopeSurvivesDatabaseClose(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "outbox.db")
	first, err := memsqlite.NewClient(path)
	require.NoError(t, err)
	s := goalsqlite.New(first.DB())
	require.NoError(t, s.Migrate(ctx))
	create(t, service(s), "persistent-outbox")
	events, err := s.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, events, 1)
	raw := json.RawMessage(`{"signedEnvelope":"must not be resigned"}`)
	require.NoError(t, s.SaveEnvelope(ctx, events[0].ID, raw))
	require.NoError(t, first.Close())
	second, err := memsqlite.NewClient(path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, second.Close()) })
	reopened := goalsqlite.New(second.DB())
	require.NoError(t, reopened.Migrate(ctx))
	saved, err := reopened.Envelope(ctx, events[0].ID)
	require.NoError(t, err)
	assert.Equal(t, raw, saved)
	pending, err := reopened.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, reopened.Published(ctx, events[0].ID))
	pending, err = reopened.Pending(ctx, 100)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// competingReceiptStore models another session winning an identical request
// after this caller's receipt lookup, with a dependency this caller cannot read.
type competingReceiptStore struct{ goals.Store }

func (s competingReceiptStore) Commit(ctx context.Context, m goals.Mutation) (goals.Goal, error) {
	winner := m
	winner.Goal.Sources = []goals.Source{{ResourceType: "document", ResourceID: "restricted", Permission: "view"}}
	winner.Event.Goal = winner.Goal
	if _, err := s.Store.Commit(ctx, winner); err != nil {
		return goals.Goal{}, err
	}
	return s.Store.Commit(ctx, m)
}

type denyRestricted struct{ allow }

func (denyRestricted) ReadGoal(_ context.Context, _ goals.Actor, g goals.Goal) error {
	if len(g.Sources) > 0 {
		return goals.ErrDenied
	}
	return nil
}

func TestConcurrentReceiptRechecksAcceptedDependencies(t *testing.T) {
	store := inmem.New()
	s := &goals.Service{Store: competingReceiptStore{store}, Auth: denyRestricted{}}
	got, err := s.Create(context.Background(), actor(), goals.CreateRequest{RequestID: "shared-retry", Title: "Agenda", Outcome: "Prepare agenda"})
	require.ErrorIs(t, err, goals.ErrDenied)
	assert.Empty(t, got.ID, "the inaccessible winning snapshot must not be disclosed")
	page, err := store.List(context.Background(), actor().Domain, goals.ListRequest{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Goals, 1, "the winning mutation remains committed")
}
