package memory_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// backendSearcher is the shape every registered SearchProvider has in
// production: it answers from the BACKEND, not through the facade, so the
// facade's per-kind read door is the only thing standing between a session's
// ranked-retrieval tool and a platform-only Kind.
//
// The in-memory provider does exactly this (it calls Backend.Query with the
// request's Kinds), and the SQLite/Postgres providers index every Kind that is
// Put — pt_tag_content included — so a text-only search reaches the same bytes.
type backendSearcher struct{ backend memory.Backend }

func (s backendSearcher) Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	res, err := s.backend.Query(ctx, memory.Query{Scope: req.Scopes[0], Kinds: req.Kinds})
	if err != nil {
		return memory.MergedSearchResult{}, err
	}
	out := memory.MergedSearchResult{}
	for _, e := range res.Entries {
		out.Entries = append(out.Entries, memory.ScoredEntry{Entry: e, Score: 1, Source: "fake"})
	}
	return out, nil
}

// newSearchDoorStore seeds the same two Kinds newReadDoorStore uses, then
// wires a searcher over the SAME backend — so the only difference between the
// Query tests and these is which facade method the caller reaches for.
func newSearchDoorStore(t *testing.T) *memory.Local {
	t.Helper()
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(hiddenFakeKind{name: hiddenKind, prefix: "rdh-"})
	memory.RegisterKind(fakeKind{name: ordinaryKind, prefix: "rdo-"})

	backend := inmem.NewBackend()
	m := memory.NewLocal(backend, memory.WithSearcher(backendSearcher{backend: backend}))
	scope := memory.Scope{Kind: "session", ID: readDoorScope}
	for _, e := range []memory.Entry{
		{Scope: scope, Kind: hiddenKind, ID: "rdh-1", CreatedAt: time.Unix(0, 0).UTC(), Content: json.RawMessage(`{}`)},
		{Scope: scope, Kind: ordinaryKind, ID: "rdo-1", CreatedAt: time.Unix(0, 0).UTC(), Content: json.RawMessage(`{}`)},
	} {
		_, err := m.Put(platformCtx(), e)
		require.NoError(t, err, "seeding %s", e.Kind)
	}
	return m
}

func searchKinds(res memory.MergedSearchResult) []string {
	var kinds []string
	for _, e := range res.Entries {
		kinds = append(kinds, e.Entry.Kind)
	}
	return kinds
}

// TestSearchDoorRefusesASessionNamingAHiddenKind is the LOUD half, on the
// second read path.
//
// The Query door already refuses this. Search is the path the agent's
// search_memory tool takes, so a door on one and not the other hides a Kind
// from the tool nobody uses and exposes it to the tool the model holds.
func TestSearchDoorRefusesASessionNamingAHiddenKind(t *testing.T) {
	m := newSearchDoorStore(t)

	_, err := m.Search(sessionCtx(), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: readDoorScope}},
		Kinds:  []string{hiddenKind},
	})
	require.ErrorIs(t, err, memory.ErrKindNotSessionReadable,
		"a session naming a platform-only Kind must be refused on Search exactly as it is on Query")
}

// TestSearchDoorFiltersAHiddenKindFromABroadSweep is the SILENT half.
//
// This is the variant that needs no knowledge of the Kind's name at all: a
// plain text search naming no Kinds returned the withheld bytes.
func TestSearchDoorFiltersAHiddenKindFromABroadSweep(t *testing.T) {
	m := newSearchDoorStore(t)

	res, err := m.Search(sessionCtx(), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: readDoorScope}},
	})
	require.NoError(t, err, "a broad search stays legal; it is simply narrower than the store")

	kinds := searchKinds(res)
	assert.NotContains(t, kinds, hiddenKind,
		"withholding a Kind from the model is a formality if ranked retrieval returns it")
	assert.Contains(t, kinds, ordinaryKind,
		"the door must remove the hidden Kind and nothing else")
}

// TestSearchDoorDoesNotApplyToPlatformCallers pins that Search's door, like
// Query's, restricts the session and not the platform that stores the content
// on its behalf.
func TestSearchDoorDoesNotApplyToPlatformCallers(t *testing.T) {
	m := newSearchDoorStore(t)

	res, err := m.Search(platformCtx(), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: readDoorScope}},
		Kinds:  []string{hiddenKind},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1, "the platform must still resolve what it stored")
	assert.Equal(t, "rdh-1", res.Entries[0].Entry.ID)
}

// TestSearchDoorLeavesOrdinaryKindsAlone stops the door from becoming a
// blanket restriction on ranked retrieval. If this fails, every agent's
// search_memory went dark at once.
func TestSearchDoorLeavesOrdinaryKindsAlone(t *testing.T) {
	m := newSearchDoorStore(t)

	res, err := m.Search(sessionCtx(), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: readDoorScope}},
		Kinds:  []string{ordinaryKind},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "rdo-1", res.Entries[0].Entry.ID)
}
