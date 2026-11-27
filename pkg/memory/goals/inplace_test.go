package goals_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
)

// CommitInPlace rewrites a goal at its current revision, so it — and any
// commit carrying Prior — is guarded by the whole stored goal; its event is
// published like any other.
func TestCommitInPlaceContract(t *testing.T) {
	for _, fixture := range []struct {
		name string
		new  func(*testing.T) goals.Store
	}{
		{"inmem", func(*testing.T) goals.Store { return inmem.New() }},
		{"sqlite", func(t *testing.T) goals.Store { return openSQL(t, filepath.Join(t.TempDir(), "goals.db")) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.new(t)
			inplace, ok := store.(goals.InPlaceStore)
			require.True(t, ok, "the store commits in place")
			g := create(t, service(store), "create")
			prior, err := goals.EncodePrior(g)
			require.NoError(t, err)

			next := g
			next.Title = "written-in-place"
			m := goals.Mutation{Goal: next, Expected: g.Revision, RequestID: "inplace-1", Hash: "h1", Prior: prior, Event: goals.Event{ID: "goalev-inplace-1", Goal: next, Action: "request_renewal", Actor: goals.SystemActor}}
			got, err := inplace.CommitInPlace(ctx, m)
			require.NoError(t, err)
			assert.Equal(t, g.Revision, got.Revision, "the revision is unchanged")
			assert.Equal(t, "written-in-place", got.Title)

			replay, err := inplace.CommitInPlace(ctx, m)
			require.NoError(t, err, "an identical retry replays its receipt")
			assert.Equal(t, got, replay)

			stale := goals.Mutation{Goal: next, Expected: g.Revision, RequestID: "inplace-2", Hash: "h2", Prior: prior, Event: goals.Event{ID: "goalev-inplace-2", Goal: next}}
			_, err = inplace.CommitInPlace(ctx, stale)
			require.ErrorIs(t, err, goals.ErrConflict, "a goal changed since it was read is not overwritten")

			_, err = inplace.CommitInPlace(ctx, goals.Mutation{Goal: next, Expected: g.Revision, RequestID: "inplace-3", Hash: "h3", Event: goals.Event{ID: "goalev-inplace-3", Goal: next}})
			require.ErrorIs(t, err, goals.ErrConflict, "an in-place commit needs the goal it read")

			bumped := next
			bumped.Revision++
			_, err = store.Commit(ctx, goals.Mutation{Goal: bumped, Expected: g.Revision, RequestID: "edit-stale", Hash: "h4", Prior: prior, Event: goals.Event{ID: "goalev-edit-stale", Goal: bumped}})
			require.ErrorIs(t, err, goals.ErrConflict, "a revision-bumping write that read before an in-place one does not lose it")

			pending, err := store.Pending(ctx, 100)
			require.NoError(t, err)
			var found bool
			for _, e := range pending {
				if e.ID == "goalev-inplace-1" {
					found = true
					assert.Equal(t, goals.SystemActor, e.Actor)
				}
			}
			require.True(t, found, "the in-place event is pending publication")
			require.NoError(t, store.SaveEnvelope(ctx, "goalev-inplace-1", []byte(`{"signed":true}`)))
			require.NoError(t, store.Published(ctx, "goalev-inplace-1"))
		})
	}
}

func TestCommitUpdateNeedsPrior(t *testing.T) {
	for _, fixture := range []struct {
		name string
		new  func(*testing.T) goals.Store
	}{
		{"inmem", func(*testing.T) goals.Store { return inmem.New() }},
		{"sqlite", func(t *testing.T) goals.Store { return openSQL(t, filepath.Join(t.TempDir(), "goals.db")) }},
	} {
		t.Run(fixture.name+": an update without the goal it read is refused", func(t *testing.T) {
			store := fixture.new(t)
			g := create(t, service(store), "create")
			next := g
			next.Revision++
			_, err := store.Commit(context.Background(), goals.Mutation{Goal: next, Expected: g.Revision, RequestID: "no-prior", Hash: "h", Event: goals.Event{ID: "goalev-no-prior", Goal: next}})
			require.ErrorIs(t, err, goals.ErrConflict)
		})
	}
}
