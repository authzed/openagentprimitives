package memory_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// The per-kind read door has TWO filters — one for Query, one for Search — and
// moving the door onto an explicit caller mark updated only the first. So the
// loud half refused webd by name on both paths, while `_search` with no `kinds`
// named still returned platform-only entries to the browser-facing token.
//
// Two filters on one door is two places to update, and the gap is exactly the
// half that is silent by design: a broad sweep is meant to come back narrower,
// so nothing about the response says a filter did not run.

func TestSearch_KindReadDoor_RefusesANamedHiddenKindForWebd(t *testing.T) {
	m := newSearchDoorStore(t)
	scope := memory.Scope{Kind: "session", ID: readDoorScope}

	_, err := m.Search(webdCtx(), memory.SearchRequest{Scopes: []memory.Scope{scope}, Kinds: []string{hiddenKind}})

	assert.ErrorIs(t, err, memory.ErrKindNotSessionReadable,
		"a browser-facing token naming a platform-only Kind must be refused on the search path too")
}

func TestSearch_KindReadDoor_FiltersUnnamedKindsForWebd(t *testing.T) {
	m := newSearchDoorStore(t)
	scope := memory.Scope{Kind: "session", ID: readDoorScope}

	got, err := m.Search(webdCtx(), memory.SearchRequest{Scopes: []memory.Scope{scope}})
	require.NoError(t, err)

	assert.NotContains(t, searchKinds(got), hiddenKind,
		"an unnarrowed webd search must not return platform-only content")
	assert.Contains(t, searchKinds(got), ordinaryKind,
		"the door removes the hidden Kind and nothing else")
}

// A per-session bearer stays filtered on the old trigger, so the new one is
// additive here as it is on the query path.
func TestSearch_KindReadDoor_StillFiltersForAPerSessionToken(t *testing.T) {
	m := newSearchDoorStore(t)
	scope := memory.Scope{Kind: "session", ID: readDoorScope}

	got, err := m.Search(sessionCtx(), memory.SearchRequest{Scopes: []memory.Scope{scope}})
	require.NoError(t, err)

	assert.NotContains(t, searchKinds(got), hiddenKind)
}
