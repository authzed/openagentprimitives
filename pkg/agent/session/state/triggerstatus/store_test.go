package triggerstatus_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/triggerstatus"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// notesRecorder captures the wrapped system_notes a Store emits.
type notesRecorder struct{ notes []map[string]any }

func (n *notesRecorder) append(_ context.Context, content map[string]any) error {
	n.notes = append(n.notes, content)
	return nil
}

func TestStore_UnansweredUntilSomethingRecordsAnAnswer(t *testing.T) {
	s := triggerstatus.NewStore(state.Deps{})

	_, done := s.Concluded()
	assert.False(t, done, "a fresh session's trigger carries no answer")

	require.NoError(t, s.RecordConcluded(context.Background(), channelkinds.TriggerOutcomeProblemsFound))

	outcome, done := s.Concluded()
	assert.True(t, done)
	assert.Equal(t, channelkinds.TriggerOutcomeProblemsFound, outcome,
		"the recorded verdict is the one that was published, so a reader can name it")
}

// TestStore_RefusesAnOutcomeOutsideTheSeamsClosedSet: the store is what the
// completion gate reads, so "concluded with nothing" must not be a state it can
// be put into — that would satisfy the gate without an answer existing.
func TestStore_RefusesAnOutcomeOutsideTheSeamsClosedSet(t *testing.T) {
	cases := []struct {
		name    string
		outcome channelkinds.TriggerOutcome
	}{
		{name: "empty: not a verdict anyone reached", outcome: ""},
		{name: "invented: no kind can publish it", outcome: "looks_fine_to_me"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := triggerstatus.NewStore(state.Deps{})
			require.Error(t, s.RecordConcluded(context.Background(), tc.outcome))
			_, done := s.Concluded()
			assert.False(t, done, "a refused record must leave the trigger unanswered")
		})
	}
}

func TestStore_RepeatOfTheSameOutcomeWritesNoSecondNote(t *testing.T) {
	rec := &notesRecorder{}
	s := triggerstatus.NewStore(state.Deps{AppendSystemNote: rec.append})

	require.NoError(t, s.RecordConcluded(context.Background(), channelkinds.TriggerOutcomeClean))
	require.NoError(t, s.RecordConcluded(context.Background(), channelkinds.TriggerOutcomeClean))

	require.Len(t, rec.notes, 1, "re-recording the answer already held has nothing to persist")
	assert.Equal(t, "triggerstatus", rec.notes[0]["kind"], "the note must route back to this Kind on replay")
}

// TestStore_ReplayRebuildsAcrossResume is the durability claim the Kind exists
// for: a resumed session must not demand an answer it already delivered.
func TestStore_ReplayRebuildsAcrossResume(t *testing.T) {
	rec := &notesRecorder{}
	live := triggerstatus.NewStore(state.Deps{AppendSystemNote: rec.append})
	require.NoError(t, live.RecordConcluded(context.Background(), channelkinds.TriggerOutcomeClean))
	require.Len(t, rec.notes, 1)

	// A fresh registry, as a restarted runner builds — then the same note fed
	// back through the framework's dispatch, as replay does.
	resumed := state.NewRegistry(state.Deps{})
	raw, err := json.Marshal(rec.notes[0])
	require.NoError(t, err)
	matched, err := state.DispatchSystemNote(resumed, raw)
	require.NoError(t, err)
	require.True(t, matched, "the note must route to this Kind, not fall through")

	store, ok := resumed.Get("triggerstatus")
	require.True(t, ok)
	rebuilt, ok := store.(*triggerstatus.Store)
	require.True(t, ok)
	outcome, done := rebuilt.Concluded()
	assert.True(t, done, "the resumed session must know its trigger was already answered")
	assert.Equal(t, channelkinds.TriggerOutcomeClean, outcome)
}
