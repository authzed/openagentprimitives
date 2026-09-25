package toolcatalog_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
)

func newMem(t *testing.T) (context.Context, memory.Memory, memory.Scope) {
	t.Helper()
	return memory.WithSystemApproval(context.Background(), "test"),
		memory.NewLocal(inmem.NewBackend()),
		memory.Scope{Kind: "session", ID: "default/demo-session"}
}

// TestForTurn_IsAStepFunction is the whole contract. Records are written only
// when the offered set CHANGES, so the catalog for turn N is the most recent
// record at or before N — not an exact-index lookup, which would find nothing
// for the many turns that wrote no record.
func TestForTurn_IsAStepFunction(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, toolcatalog.Record(ctx, m, scope, toolcatalog.Content{
		FromTurnIndex: 0, Tools: []string{"respond_to_user", "update_plan"},
	}))
	require.NoError(t, toolcatalog.Record(ctx, m, scope, toolcatalog.Content{
		FromTurnIndex: 4, Tools: []string{"respond_to_user", "update_plan", "acme_list_things"},
	}))

	cases := []struct {
		name string
		turn int
		want []string
	}{
		{"turn 0 gets the first record", 0, []string{"respond_to_user", "update_plan"}},
		{"turn 3 still gets the first record: nothing changed at 1..3", 3, []string{"respond_to_user", "update_plan"}},
		{"turn 4 gets the second record on its own index", 4, []string{"acme_list_things", "respond_to_user", "update_plan"}},
		{"turn 9 gets the second record: it is still in force", 9, []string{"acme_list_things", "respond_to_user", "update_plan"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toolcatalog.ForTurn(ctx, m, scope, tc.turn)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestForTurn_BeforeAnyRecordReturnsNil pins that an unrecorded prefix reads as
// "unknown", not as "no tools were offered". A capture must be able to tell
// those apart: the first is a session captured before this Kind shipped and the
// toolOffered assertions are simply omitted; the second would be a bundle
// asserting the agent had nothing to call.
func TestForTurn_BeforeAnyRecordReturnsNil(t *testing.T) {
	ctx, m, scope := newMem(t)
	require.NoError(t, toolcatalog.Record(ctx, m, scope, toolcatalog.Content{
		FromTurnIndex: 3, Tools: []string{"respond_to_user"},
	}))

	got, err := toolcatalog.ForTurn(ctx, m, scope, 1)
	require.NoError(t, err)
	assert.Nil(t, got, "no record in force at turn 1 must read as unknown, not as an empty catalog")
}

// TestRecord_ReRecordingTheSameTurnIsIdempotent pins that a restarted runner
// replaying to the same index does not conflict with itself. The Kind is
// append-only, so a non-idempotent re-record would fail the resume.
func TestRecord_ReRecordingTheSameTurnIsIdempotent(t *testing.T) {
	ctx, m, scope := newMem(t)
	c := toolcatalog.Content{FromTurnIndex: 2, Tools: []string{"respond_to_user"}}

	require.NoError(t, toolcatalog.Record(ctx, m, scope, c))
	require.NoError(t, toolcatalog.Record(ctx, m, scope, c), "byte-identical re-record must be a no-op")

	all, err := toolcatalog.List(ctx, m, scope)
	require.NoError(t, err)
	assert.Len(t, all, 1)
}

// TestRecord_SortsAndDigests pins that the stored tool list is canonical, so a
// catalog whose only difference is provider iteration order does not read as a
// change and write a spurious record.
func TestRecord_SortsAndDigests(t *testing.T) {
	ctx, m, scope := newMem(t)
	require.NoError(t, toolcatalog.Record(ctx, m, scope, toolcatalog.Content{
		FromTurnIndex: 0, Tools: []string{"zeta", "alpha", "mid"},
	}))

	all, err := toolcatalog.List(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, all[0].Tools)
	assert.Equal(t, toolcatalog.Digest([]string{"alpha", "mid", "zeta"}), all[0].Digest)
	assert.Equal(t, toolcatalog.Digest([]string{"zeta", "alpha", "mid"}), all[0].Digest,
		"Digest must canonicalize order itself, or a caller comparing digests re-derives the sort and gets it wrong")
}

// TestForTurn_AcrossResume pins the restart case: a runner resumed mid-session
// continues the transcript at whatever index replay() rebuilds, which is
// monotonically increasing across the whole session — never a value that
// resets to 0 the way a Send-ordinal would on the new process. Keying
// FromTurnIndex on anything that resets would collide the append-only entry
// id (toolcat-<FromTurnIndex>) with turn 0's, and Record's ErrAppendOnlyConflict
// handling would swallow the resumed catalog as "already recorded" — silently
// losing it. This simulates that shape directly: a second record at a HIGHER
// transcript index than the first, as a resumed run's own nextIndex would be,
// and asserts both survive and ForTurn steps across them correctly.
func TestForTurn_AcrossResume(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, toolcatalog.Record(ctx, m, scope, toolcatalog.Content{
		FromTurnIndex: 1, Tools: []string{"respond_to_user"},
	}))
	// A later run resumes the same session and, mid-transcript, the catalog
	// changes (a capability opens). Its transcript index is far higher than
	// the first run's — nothing here ever resets to 0.
	require.NoError(t, toolcatalog.Record(ctx, m, scope, toolcatalog.Content{
		FromTurnIndex: 47, Tools: []string{"respond_to_user", "acme_list_things"},
	}))

	all, err := toolcatalog.List(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, all, 2, "both the pre-restart and post-restart records must survive")

	got, err := toolcatalog.ForTurn(ctx, m, scope, 20)
	require.NoError(t, err)
	assert.Equal(t, []string{"respond_to_user"}, got,
		"a turn between the two records must resolve to the pre-restart catalog")

	got, err = toolcatalog.ForTurn(ctx, m, scope, 47)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme_list_things", "respond_to_user"}, got,
		"the resumed run's catalog must be in force from its own transcript index onward")
}

// TestRecord_DifferingReRecordAtSameIndexErrors pins the other half of the
// idempotency contract Record must uphold: the entry id is keyed on
// FromTurnIndex, not on content, so a conflict on that id does NOT by itself
// prove the content matches (unlike systemprompt, whose id IS its digest).
// A same-index write carrying a genuinely DIFFERENT tool set — e.g. a
// mid-loop retry that never advanced nextIndex, racing a sidecar tool
// becoming ready — must surface as an error naming the turn and both
// digests, not vanish into the "already recorded" no-op path.
func TestRecord_DifferingReRecordAtSameIndexErrors(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, toolcatalog.Record(ctx, m, scope, toolcatalog.Content{
		FromTurnIndex: 2, Tools: []string{"respond_to_user"},
	}))

	err := toolcatalog.Record(ctx, m, scope, toolcatalog.Content{
		FromTurnIndex: 2, Tools: []string{"respond_to_user", "acme_list_things"},
	})
	require.Error(t, err, "a different tool set at an already-recorded turn must not be swallowed as idempotent")
	assert.Contains(t, err.Error(), "2", "the error must name the conflicting turn index")

	all, listErr := toolcatalog.List(ctx, m, scope)
	require.NoError(t, listErr)
	require.Len(t, all, 1, "the original record must be untouched — the rejected write must not have landed")
	assert.Equal(t, []string{"respond_to_user"}, all[0].Tools)
}

// TestRecord_RefusesEmptyCatalog pins the empty-catalog guard: an empty Tools
// slice would read as real evidence the agent was offered nothing, which is
// never true (respond_to_user is always present), so Record must refuse it
// rather than write a misleading row.
func TestRecord_RefusesEmptyCatalog(t *testing.T) {
	ctx, m, scope := newMem(t)

	err := toolcatalog.Record(ctx, m, scope, toolcatalog.Content{FromTurnIndex: 0, Tools: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty", "the error should say why the write was refused")

	all, listErr := toolcatalog.List(ctx, m, scope)
	require.NoError(t, listErr)
	assert.Empty(t, all, "a refused write must not land as an entry")
}
