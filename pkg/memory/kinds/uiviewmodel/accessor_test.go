package uiviewmodel_test

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
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewmodel"
)

// newMemFixture builds a fresh in-memory backend for a fixed session scope,
// mirroring pkg/memory/kinds/uiaction/accessor_test's fixture shape. The
// context carries a system approval because Memory.Put/Query gate on
// EnsureApproval unconditionally, independent of any pluggable Authorizer.
func newMemFixture(t *testing.T) (context.Context, memory.Memory, memory.Scope) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	return ctx, memory.NewLocal(inmem.NewBackend()), memory.Scope{Kind: "session", ID: "demo-ns/demo-session"}
}

func TestRecordIsLastWriteWinsPerSlot(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)

	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "panel", Node: json.RawMessage(`{"component":"ap:text","props":{"text":"first"}}`),
		WrittenAt: time.Now().UTC(),
	}))
	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "panel", Node: json.RawMessage(`{"component":"ap:text","props":{"text":"second"}}`),
		WrittenAt: time.Now().UTC(),
	}))

	got, err := uiviewmodel.List(ctx, mem, scope, "demo-ui")
	require.NoError(t, err, "a second write must REPLACE, never conflict — this Kind is deliberately mutable")
	require.Len(t, got, 1, "one record per (ui, slot)")
	assert.JSONEq(t, `{"component":"ap:text","props":{"text":"second"}}`, string(got[0].Node))
}

// TestRecordRoundTripsAClearedRecord pins that a Content with Cleared: true
// and an empty Node — the shape Runtime.Clear writes — survives Record/List
// unchanged, rather than the omitempty on Cleared or the absent Node quietly
// losing the intentional-empty on the way back out.
func TestRecordRoundTripsAClearedRecord(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)

	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "panel", Cleared: true, WrittenAt: time.Now().UTC(),
	}))

	got, err := uiviewmodel.List(ctx, mem, scope, "demo-ui")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].Cleared)
	assert.Empty(t, got[0].Node)
}

func TestListFiltersByUIAndSortsBySlot(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	write := func(ui, slot string) {
		require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
			UI: ui, Slot: slot, Node: json.RawMessage(`{"component":"ap:empty"}`), WrittenAt: time.Now().UTC(),
		}))
	}
	write("demo-ui", "zebra")
	write("demo-ui", "alpha")
	write("other-ui", "alpha")

	got, err := uiviewmodel.List(ctx, mem, scope, "demo-ui")
	require.NoError(t, err)
	require.Len(t, got, 2, "another UI's records must not leak into this one's view")
	assert.Equal(t, []string{"alpha", "zebra"}, []string{got[0].Slot, got[1].Slot},
		"deterministic order: the rejection list a caller builds from this must be diffable")
}

func TestEntryIDIsStableAndSeparates(t *testing.T) {
	assert.Equal(t, uiviewmodel.EntryID("ui", "slot"), uiviewmodel.EntryID("ui", "slot"),
		"the same (ui, slot) must always land at the same key or last-write-wins is not last-write-wins")
	assert.NotEqual(t, uiviewmodel.EntryID("a", "b/c"), uiviewmodel.EntryID("a/b", "c"),
		"a separator that can occur in either half would let two slots collide")
	assert.True(t, strings.HasPrefix(uiviewmodel.EntryID("ui", "slot"), uiviewmodel.IDPrefix))
}

func TestListReturnsEmptyNotErrorWhenNothingWritten(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	got, err := uiviewmodel.List(ctx, mem, scope, "demo-ui")
	require.NoError(t, err, "a UI the agent has never composed is Tier-0, not an error")
	assert.Empty(t, got)
}

// TestListReturnsErrorOnMalformedStoredContent pins that a record which
// cannot be decoded is surfaced to the caller rather than silently skipped —
// a record that will not decode is a UI that silently loses a slot, which is
// the failure AGENTS.md's no-silent-errors rule exists to prevent.
func TestListReturnsErrorOnMalformedStoredContent(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	_, err := mem.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      uiviewmodel.KindName,
		ID:        uiviewmodel.EntryID("demo-ui", "panel"),
		CreatedAt: time.Now().UTC(),
		Content:   json.RawMessage(`{not valid json`),
	})
	require.NoError(t, err)

	_, err = uiviewmodel.List(ctx, mem, scope, "demo-ui")
	assert.Error(t, err, "a record that cannot decode must be reported, not dropped")
}
