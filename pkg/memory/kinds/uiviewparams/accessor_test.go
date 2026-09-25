package uiviewparams_test

// Coverage for the ui_view_params accessor. The Kind's whole reason to exist is
// durability — a viewer who says "last seven days" in a channel and opens the
// browser an hour later must find the dashboard on that window — so the
// properties worth pinning are the ones that decide what a later page load
// sees: replacement (not merge), absence-is-not-an-error, and a corrupt record
// surfacing rather than being skipped past.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewparams"
)

const (
	demoUI      = "demo-dashboard"
	otherUI     = "other-dashboard"
	demoScopeID = "demo-ns/demo-session"
)

// newMem builds a session-scoped in-memory store plus the system-approval
// context every memory call needs.
func newMem(t *testing.T) (context.Context, memory.Memory, memory.Scope) {
	t.Helper()
	return memory.WithSystemApproval(context.Background(), "uiviewparams-test"),
		memory.NewLocal(inmem.NewBackend()),
		memory.Scope{Kind: "session", ID: demoScopeID}
}

// record is the common write, with WrittenAt supplied so callers do not repeat
// a timestamp they do not care about.
func record(t *testing.T, ctx context.Context, m memory.Memory, scope memory.Scope, ui string, params map[string]string) {
	t.Helper()
	require.NoError(t, uiviewparams.Record(ctx, m, scope, uiviewparams.Content{
		UI:        ui,
		Params:    params,
		WrittenAt: time.Now(),
	}), "Record %s", ui)
}

func TestEntryID(t *testing.T) {
	t.Run("deterministic for the same UI name", func(t *testing.T) {
		assert.Equal(t, uiviewparams.EntryID(demoUI), uiviewparams.EntryID(demoUI),
			"the same UI must map to the same entry, or Record appends instead of replacing")
	})
	t.Run("distinct UI names do not collide", func(t *testing.T) {
		assert.NotEqual(t, uiviewparams.EntryID(demoUI), uiviewparams.EntryID(otherUI))
	})
	t.Run("carries the Kind's ID prefix", func(t *testing.T) {
		assert.True(t, strings.HasPrefix(uiviewparams.EntryID(demoUI), uiviewparams.IDPrefix),
			"the registry enforces prefix uniqueness; an ID without it belongs to no Kind")
	})
	t.Run("hashed, so the ID charset never inherits a CR name's", func(t *testing.T) {
		got := uiviewparams.EntryID("a name with spaces/and.dots")
		assert.Len(t, got, len(uiviewparams.IDPrefix)+32)
		assert.NotContains(t, got, " ")
		assert.NotContains(t, got, ".")
	})
	t.Run("the empty UI name still yields a well-formed ID", func(t *testing.T) {
		assert.Len(t, uiviewparams.EntryID(""), len(uiviewparams.IDPrefix)+32)
	})
}

func TestGet_NoRecordIsNotAnError(t *testing.T) {
	ctx, m, scope := newMem(t)

	got, err := uiviewparams.Get(ctx, m, scope, demoUI)
	require.NoError(t, err,
		"a UI whose viewer drives its own controls has no record; that is the ordinary state, not a failure")
	assert.NotNil(t, got, "callers range over the result without a nil check")
	assert.Empty(t, got)
}

func TestRecordAndGet_RoundTrip(t *testing.T) {
	ctx, m, scope := newMem(t)
	record(t, ctx, m, scope, demoUI, map[string]string{"window.from": "-7d", "window.to": "now"})

	got, err := uiviewparams.Get(ctx, m, scope, demoUI)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"window.from": "-7d", "window.to": "now"}, got)
}

// TestRecord_ReplacesRatherThanMerges is the documented invariant: the agent
// states the SET of controls it is driving, so a key it stops naming is a
// control it has stopped driving. A merge would make an agent-set filter
// impossible to clear.
func TestRecord_ReplacesRatherThanMerges(t *testing.T) {
	ctx, m, scope := newMem(t)

	record(t, ctx, m, scope, demoUI, map[string]string{"window.from": "-7d", "status": "open"})
	record(t, ctx, m, scope, demoUI, map[string]string{"window.from": "-1d"})

	got, err := uiviewparams.Get(ctx, m, scope, demoUI)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"window.from": "-1d"}, got,
		"the second write states the whole set; the dropped key must be gone, not merged forward")
	assert.NotContains(t, got, "status", "a key the agent stopped naming must be clearable")
}

func TestRecord_EmptyParamsClearsEverything(t *testing.T) {
	ctx, m, scope := newMem(t)

	record(t, ctx, m, scope, demoUI, map[string]string{"window.from": "-7d"})
	record(t, ctx, m, scope, demoUI, map[string]string{})

	got, err := uiviewparams.Get(ctx, m, scope, demoUI)
	require.NoError(t, err)
	assert.Empty(t, got, "an empty set means the agent drives nothing; the page falls back to its own defaults")
}

func TestGet_NilParamsReadsAsEmptyMap(t *testing.T) {
	ctx, m, scope := newMem(t)
	record(t, ctx, m, scope, demoUI, nil)

	got, err := uiviewparams.Get(ctx, m, scope, demoUI)
	require.NoError(t, err)
	assert.NotNil(t, got, "a nil Params must not reach the caller as a nil map")
	assert.Empty(t, got)
}

// TestGet_IsScopedToTheNamedUI proves one session's UIs do not read each
// other's controls — a dashboard repainted with a sibling's window would be a
// silent wrong answer, not a visible failure.
func TestGet_IsScopedToTheNamedUI(t *testing.T) {
	ctx, m, scope := newMem(t)
	record(t, ctx, m, scope, demoUI, map[string]string{"window.from": "-7d"})
	record(t, ctx, m, scope, otherUI, map[string]string{"window.from": "-30d"})

	t.Run("each UI reads back its own params", func(t *testing.T) {
		got, err := uiviewparams.Get(ctx, m, scope, demoUI)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"window.from": "-7d"}, got)

		got, err = uiviewparams.Get(ctx, m, scope, otherUI)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"window.from": "-30d"}, got)
	})
	t.Run("an unrecorded UI reads empty even when siblings have params", func(t *testing.T) {
		got, err := uiviewparams.Get(ctx, m, scope, "never-recorded")
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

// TestGet_IsScopedToTheSession guards the scope boundary: params recorded in
// one session must never surface in another.
func TestGet_IsScopedToTheSession(t *testing.T) {
	ctx, m, scope := newMem(t)
	record(t, ctx, m, scope, demoUI, map[string]string{"window.from": "-7d"})

	other := memory.Scope{Kind: "session", ID: "demo-ns/other-session"}
	got, err := uiviewparams.Get(ctx, m, other, demoUI)
	require.NoError(t, err)
	assert.Empty(t, got, "another session's params must not leak across the scope boundary")
}

// TestGet_UndecodableRecordIsReturnedNotSkipped pins AGENTS.md's
// no-silent-errors rule at this accessor. Skipping a corrupt record would
// repaint the page at the author's defaults while the viewer believes their
// chosen window is still applied — a wrong answer with no signal anywhere.
func TestGet_UndecodableRecordIsReturnedNotSkipped(t *testing.T) {
	ctx, m, scope := newMem(t)

	_, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      uiviewparams.KindName,
		ID:        uiviewparams.EntryID(demoUI),
		CreatedAt: time.Now(),
		Content:   json.RawMessage(`{"ui": 12345}`), // ui must be a string
	})
	require.NoError(t, err, "seeding the corrupt record must itself succeed")

	got, err := uiviewparams.Get(ctx, m, scope, demoUI)
	require.Error(t, err, "a record that will not decode must surface, not be skipped past")
	assert.Contains(t, err.Error(), "uiviewparams.Get: unmarshal",
		"the error must name the accessor and the failing step")
	assert.Nil(t, got, "a failed read must not hand back params a caller would apply")
}

func TestKind_Metadata(t *testing.T) {
	k := uiviewparams.Kind{}

	t.Run("registered under its own name and prefix", func(t *testing.T) {
		assert.Equal(t, uiviewparams.KindName, k.Name())
		assert.Equal(t, uiviewparams.IDPrefix, k.IDPrefix())
	})

	// The retention answers below are load-bearing, not cosmetic: a chatty
	// session must not evict the window its own dashboard is showing, and a
	// completed session's page must not come back with content intact and
	// filters reset.
	t.Run("essential while live, archived on completion and failure", func(t *testing.T) {
		r := k.Retention()
		assert.True(t, r.EssentialWhileLive,
			"eviction would silently repaint the dashboard at the author's defaults")
		assert.False(t, r.AppendOnly,
			"append-only would let the agent set the page's controls exactly once")
		assert.NotEmpty(t, r.ArchiveOn, "a completed session's params must not outlive it indefinitely")
		assert.Positive(t, r.TTLAfterArchive)
	})

	t.Run("session-written: the agent is the author", func(t *testing.T) {
		assert.Equal(t, memory.SessionWritten, k.WriteAuthority())
	})

	t.Run("indexes the ui field the accessor filters on", func(t *testing.T) {
		assert.Equal(t, []string{"ui"}, k.IndexedFields())
	})

	t.Run("content schema is the Content struct", func(t *testing.T) {
		assert.Equal(t, "Content", k.ContentSchema().Name())
	})
}
