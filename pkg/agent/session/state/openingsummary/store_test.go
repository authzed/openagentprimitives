package openingsummary_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/openingsummary"
)

type notesRecorder struct{ notes []map[string]any }

func (n *notesRecorder) append(_ context.Context, content map[string]any) error {
	n.notes = append(n.notes, content)
	return nil
}

func TestStore_EmptyUntilSet(t *testing.T) {
	s := openingsummary.NewStore(state.Deps{})
	assert.Equal(t, "", s.Body(), "a fresh session has no enrichment body")
}

func TestStore_SetReplacesLastWriteWins(t *testing.T) {
	s := openingsummary.NewStore(state.Deps{})
	require.NoError(t, s.SetBody(context.Background(), "one finding"))
	require.NoError(t, s.SetBody(context.Background(), "two findings"))
	assert.Equal(t, "two findings", s.Body(), "last write wins")
}

func TestStore_RepeatOfSameBodyWritesNoSecondNote(t *testing.T) {
	rec := &notesRecorder{}
	s := openingsummary.NewStore(state.Deps{AppendSystemNote: rec.append})
	require.NoError(t, s.SetBody(context.Background(), "same"))
	require.NoError(t, s.SetBody(context.Background(), "same"))
	require.Len(t, rec.notes, 1, "re-recording the body already held persists nothing")
	assert.Equal(t, "openingsummary", rec.notes[0]["kind"])
}

func TestStore_ReplayRebuildsAcrossResume(t *testing.T) {
	rec := &notesRecorder{}
	live := openingsummary.NewStore(state.Deps{AppendSystemNote: rec.append})
	require.NoError(t, live.SetBody(context.Background(), "reviewed 7 files"))
	require.Len(t, rec.notes, 1)

	resumed := state.NewRegistry(state.Deps{})
	raw, err := json.Marshal(rec.notes[0])
	require.NoError(t, err)
	matched, err := state.DispatchSystemNote(resumed, raw)
	require.NoError(t, err)
	require.True(t, matched)

	got, ok := resumed.Get("openingsummary")
	require.True(t, ok)
	rebuilt := got.(*openingsummary.Store)
	assert.Equal(t, "reviewed 7 files", rebuilt.Body())
}
