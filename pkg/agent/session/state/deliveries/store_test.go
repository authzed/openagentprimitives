package deliveries_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
)

// notesRecorder captures the wrapped system_notes a Store emits.
type notesRecorder struct{ notes []map[string]any }

func (n *notesRecorder) append(_ context.Context, content map[string]any) error {
	n.notes = append(n.notes, content)
	return nil
}

// item is the terse fixture constructor these tests use; the render name and
// the artifact id are the two halves the store records together.
func item(render, artifact string) deliveries.Item {
	return deliveries.Item{RenderName: render, ArtifactID: artifact}
}

func TestStore_RecordsAndReports(t *testing.T) {
	s := deliveries.NewStore(state.Deps{})
	require.NoError(t, s.Record(context.Background(), item("ar-a", "artifact-a"), item("ar-b", "artifact-b")))

	assert.True(t, s.Delivered("ar-a"))
	assert.True(t, s.Delivered("ar-b"))
	assert.False(t, s.Delivered("ar-never-sent"), "an artifact nobody attached is not delivered")
	assert.Equal(t, []string{"ar-a", "ar-b"}, s.All())
}

// TestStore_LastArtifactID_IsTheMostRecentlyDelivered: a session may hand over
// several artifacts, and something naming "the result" — a check run's details
// link — has to pick one. The most recent is the only defensible answer: it is
// what the agent last put in front of a person, and it is the one its closing
// judgement is about.
func TestStore_LastArtifactID_IsTheMostRecentlyDelivered(t *testing.T) {
	s := deliveries.NewStore(state.Deps{})
	assert.Empty(t, s.LastArtifactID(), "nothing delivered yet: no artifact to name")

	require.NoError(t, s.Record(context.Background(), item("ar-draft", "artifact-draft")))
	assert.Equal(t, "artifact-draft", s.LastArtifactID())

	require.NoError(t, s.Record(context.Background(), item("ar-final", "artifact-final")))
	assert.Equal(t, "artifact-final", s.LastArtifactID(), "the later delivery wins")

	// A re-delivery of an already-recorded render adds nothing, so it must not
	// move the pointer back to an earlier artifact.
	require.NoError(t, s.Record(context.Background(), item("ar-draft", "artifact-draft")))
	assert.Equal(t, "artifact-final", s.LastArtifactID(), "a repeat delivery is not a newer one")
}

// TestStore_LastArtifactID_SkipsRendersWithNoArtifactID: a render whose CR
// carries no artifact-id label is still a delivery for the completion gate, but
// it cannot be linked to. It must not shadow the last one that can.
func TestStore_LastArtifactID_SkipsRendersWithNoArtifactID(t *testing.T) {
	s := deliveries.NewStore(state.Deps{})
	require.NoError(t, s.Record(context.Background(), item("ar-a", "artifact-a")))
	require.NoError(t, s.Record(context.Background(), item("ar-unlabelled", "")))

	assert.True(t, s.Delivered("ar-unlabelled"), "it was still delivered")
	assert.Equal(t, "artifact-a", s.LastArtifactID(), "an unlinkable delivery does not erase a linkable one")
}

func TestStore_RepeatDeliveryWritesNoSecondNote(t *testing.T) {
	rec := &notesRecorder{}
	s := deliveries.NewStore(state.Deps{AppendSystemNote: rec.append})

	require.NoError(t, s.Record(context.Background(), item("ar-a", "artifact-a")))
	require.NoError(t, s.Record(context.Background(), item("ar-a", "artifact-a")))
	require.NoError(t, s.Record(context.Background(), item("", "artifact-a")))

	require.Len(t, rec.notes, 1,
		"the set only grows, so re-attaching the same artifact — or nothing at all — has nothing to persist")
	assert.Equal(t, "deliveries", rec.notes[0]["kind"], "the note must route back to this Kind on replay")
}

// TestStore_ReplayRebuildsAcrossResume is the durability claim the Kind exists
// for: a resumed session must not re-report artifacts it already delivered, and
// must still be able to name the artifact its result lives in.
func TestStore_ReplayRebuildsAcrossResume(t *testing.T) {
	rec := &notesRecorder{}
	live := deliveries.NewStore(state.Deps{AppendSystemNote: rec.append})
	require.NoError(t, live.Record(context.Background(), item("ar-a", "artifact-a")))
	require.NoError(t, live.Record(context.Background(), item("ar-b", "artifact-b")))
	require.Len(t, rec.notes, 2, "two distinct deliveries, two notes")

	// A fresh store, as a restarted runner builds — then the same notes fed
	// back through the framework's dispatch, as replay does.
	resumed := state.NewRegistry(state.Deps{})
	for _, note := range rec.notes {
		raw, err := json.Marshal(note)
		require.NoError(t, err)
		matched, derr := state.DispatchSystemNote(resumed, raw)
		require.NoError(t, derr)
		require.True(t, matched, "a deliveries note must route to the deliveries store")
	}

	store, ok := resumed.Get("deliveries")
	require.True(t, ok)
	assert.Equal(t, []string{"ar-a", "ar-b"}, store.(*deliveries.Store).All(),
		"a resumed session remembers what it already delivered")
	assert.Equal(t, "artifact-b", store.(*deliveries.Store).LastArtifactID(),
		"replay preserves delivery ORDER, so the last artifact is still the last one")
}

// TestStore_ReplayOfAPreArtifactIDNote is the compatibility case: a session
// resumed across an upgrade replays notes written before deliveries recorded
// artifact ids. Those deliveries must still count for the completion gate; they
// simply cannot be linked to, which is the same graceful state as an
// unlabelled render.
func TestStore_ReplayOfAPreArtifactIDNote(t *testing.T) {
	s := deliveries.NewStore(state.Deps{})
	require.NoError(t, s.ReplayNote(json.RawMessage(`{"renders":["ar-old"]}`)))

	assert.True(t, s.Delivered("ar-old"), "an older note still records the delivery")
	assert.Empty(t, s.LastArtifactID(), "it names no artifact, so there is nothing to link to")
}

func TestStore_ReplayRejectsCorruptPayload(t *testing.T) {
	s := deliveries.NewStore(state.Deps{})
	require.Error(t, s.ReplayNote(json.RawMessage(`{"renders":"not-a-list"}`)),
		"a note that cannot be read must surface, not resume with the set silently short")
}
