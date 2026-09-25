package inmem_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

func TestInMem_Capabilities(t *testing.T) {
	b := inmem.NewBackend()
	caps := b.Capabilities()
	assert.Equal(t, 1, caps.LinkTraversal)
	assert.True(t, caps.ReverseLinks)
	assert.True(t, caps.TagFilters)
	assert.True(t, caps.TimeRange)
	assert.True(t, caps.ContentSchemas, "inmem honors FieldEquals, so the unit suite can observe a wrong path")
	assert.True(t, caps.FieldRange, "and the ordering ops too")
}

func TestInMem_PutGetRoundtrip(t *testing.T) {
	b := inmem.NewBackend()
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	e := memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		CreatedAt: time.Now().UTC(),
		Tags:      []string{"x"},
	}
	require.NoError(t, b.Put(ctx, e))
	got, ok, err := b.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, e, got)
}

func TestInMem_PutGetRoundTripsProvenance(t *testing.T) {
	b := inmem.NewBackend()
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	e := memory.Entry{
		Scope:     scope,
		Kind:      "alpha",
		ID:        "a-prov-1",
		CreatedAt: time.Now().UTC(),
		Provenance: &memory.Provenance{
			Publisher: "system:test",
			KeyID:     "abcd1234",
			Seq:       7,
			PrevHash:  "deadbeef",
			Sig:       []byte{1, 2, 3, 4},
		},
	}
	require.NoError(t, b.Put(ctx, e))
	got, ok, err := b.Get(ctx, scope, "alpha", "a-prov-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, e.Provenance, got.Provenance)
}

func TestInMem_GetMissReturnsFalse(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	_, ok, err := b.Get(context.Background(), scope, "alpha", "absent")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestInMem_DeleteIdempotent(t *testing.T) {
	b := inmem.NewBackend()
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1"}))
	require.NoError(t, b.Delete(ctx, scope, "alpha", "a-1"))
	require.NoError(t, b.Delete(ctx, scope, "alpha", "a-1"), "second delete is a no-op")
	_, ok, _ := b.Get(ctx, scope, "alpha", "a-1")
	assert.False(t, ok)
}

func TestInMem_DeleteScope_RemovesAllEntries(t *testing.T) {
	b := inmem.NewBackend()
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	other := memory.Scope{Kind: "session", ID: "ns/b"}

	require.NoError(t, b.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1"}))
	require.NoError(t, b.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-2"}))
	require.NoError(t, b.Put(ctx, memory.Entry{Scope: other, Kind: "alpha", ID: "a-3"}))

	require.NoError(t, b.DeleteScope(ctx, scope))

	res, err := b.Query(ctx, memory.Query{Scope: scope})
	require.NoError(t, err)
	assert.Empty(t, res.Entries, "deleted scope must return no entries")

	// A sibling scope is untouched.
	resOther, err := b.Query(ctx, memory.Query{Scope: other})
	require.NoError(t, err)
	require.Len(t, resOther.Entries, 1)

	// DeleteScope is idempotent.
	require.NoError(t, b.DeleteScope(ctx, scope), "second DeleteScope is a no-op")
}

func TestInMem_Query_FilterByKind(t *testing.T) {
	b := inmem.NewBackend()
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	now := time.Now().UTC()
	require.NoError(t, b.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: now}))
	require.NoError(t, b.Put(ctx, memory.Entry{Scope: scope, Kind: "beta", ID: "b-1", CreatedAt: now}))

	res, err := b.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"alpha"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-1", res.Entries[0].ID)
}

func TestInMem_Query_FilterByIDs(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	for _, id := range []string{"a-1", "a-2", "a-3"} {
		require.NoError(t, b.Put(context.Background(), memory.Entry{Scope: scope, Kind: "alpha", ID: id}))
	}
	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, Kinds: []string{"alpha"}, IDs: []string{"a-1", "a-3"},
	})
	require.NoError(t, err)
	got := map[string]struct{}{}
	for _, e := range res.Entries {
		got[e.ID] = struct{}{}
	}
	assert.Equal(t, map[string]struct{}{"a-1": {}, "a-3": {}}, got)
}

func TestInMem_Query_FilterByTags(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(context.Background(), memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", Tags: []string{"x", "y"}}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{Scope: scope, Kind: "alpha", ID: "a-2", Tags: []string{"x"}}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{Scope: scope, Kind: "alpha", ID: "a-3", Tags: []string{"y"}}))

	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, Kinds: []string{"alpha"}, Tags: []string{"x", "y"},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1, "only the entry with BOTH x and y matches")
	assert.Equal(t, "a-1", res.Entries[0].ID)
}

func TestInMem_Query_FilterByTimeRange(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, ts := range []time.Time{t0, t0.Add(time.Hour), t0.Add(2 * time.Hour)} {
		require.NoError(t, b.Put(context.Background(), memory.Entry{
			Scope: scope, Kind: "alpha", ID: fmt.Sprintf("a-%d", i+1), CreatedAt: ts,
		}))
	}
	since := t0.Add(30 * time.Minute)
	until := t0.Add(90 * time.Minute)
	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, Since: &since, Until: &until,
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-2", res.Entries[0].ID)
}

func TestInMem_Query_OrderByCreatedAtDesc(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, ts := range []time.Time{t0, t0.Add(time.Hour), t0.Add(2 * time.Hour)} {
		require.NoError(t, b.Put(context.Background(), memory.Entry{
			Scope: scope, Kind: "alpha", ID: fmt.Sprintf("a-%d", i+1), CreatedAt: ts,
		}))
	}
	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, OrderBy: memory.OrderBy{Field: "createdAt", Desc: true},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 3)
	assert.Equal(t, "a-3", res.Entries[0].ID)
	assert.Equal(t, "a-1", res.Entries[2].ID)
}

func TestInMem_Query_Limit(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	for i := 0; i < 5; i++ {
		require.NoError(t, b.Put(context.Background(), memory.Entry{
			Scope: scope, Kind: "alpha", ID: fmt.Sprintf("a-%d", i),
			CreatedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
		}))
	}
	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, OrderBy: memory.OrderBy{Field: "createdAt"}, Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)
	assert.Equal(t, []string{"a-0", "a-1"}, ids(res.Entries),
		"the oldest two, not an arbitrary two")
}

// `oap memory list` and `oap memory query` set Limit and leave OrderBy zero, so
// this is the shape the CLI actually sends. Truncating an unordered Go map
// walk returns whichever entries the randomized iteration happened to visit
// first, so the same query answers differently every time and there is no way
// to page through the rest.
func TestInMem_Query_LimitWithoutOrderByIsDeterministic(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	for i := 0; i < 10; i++ {
		require.NoError(t, b.Put(context.Background(), memory.Entry{
			Scope: scope, Kind: "alpha", ID: fmt.Sprintf("a-%d", i),
			CreatedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
		}))
	}

	// Repeat: map iteration order is randomized per range, so one lucky
	// agreement proves nothing.
	for i := 0; i < 10; i++ {
		res, err := b.Query(context.Background(), memory.Query{Scope: scope, Limit: 3})
		require.NoError(t, err)
		require.Len(t, res.Entries, 3)
		assert.Equal(t, []string{"a-9", "a-8", "a-7"}, ids(res.Entries),
			"an unordered Limit must still truncate deterministically (newest first)")
	}
}

// Entries sharing a CreatedAt must not swap places between calls either.
func TestInMem_Query_OrderByTiesBreakOnID(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"a-3", "a-1", "a-2"} {
		require.NoError(t, b.Put(context.Background(), memory.Entry{
			Scope: scope, Kind: "alpha", ID: id, CreatedAt: ts,
		}))
	}
	for i := 0; i < 10; i++ {
		res, err := b.Query(context.Background(), memory.Query{
			Scope: scope, OrderBy: memory.OrderBy{Field: "createdAt"},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"a-1", "a-2", "a-3"}, ids(res.Entries))
	}
}

func ids(entries []memory.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func TestInMem_Query_LinkedTo(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-1",
		Links: []memory.Link{{Relation: "for_resource", Kind: "repo", ID: "r-1"}},
	}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-2",
		Links: []memory.Link{{Relation: "for_resource", Kind: "repo", ID: "r-2"}},
	}))

	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, Kinds: []string{"decision"},
		LinkedTo: []memory.LinkFilter{{Relation: "for_resource", Kind: "repo", ID: "r-1"}},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "d-1", res.Entries[0].ID)
}

// TestInMem_Query_FieldEquals covers the predicate this backend used to drop.
// Dropping it made inmem an unfaithful double: because the unit suite runs on
// inmem, a predicate naming a key that does not exist matched everything here
// and nothing on postgres/sqlite, so no unit test could observe a wrong
// FieldFilter.Path. The cases below pin the SQL backends' semantics — top-level
// key, text comparison, NULL for an absent key — so a divergence shows up as a
// failure here instead of as an empty result in production.
func TestInMem_Query_FieldEquals(t *testing.T) {
	cases := []struct {
		name   string
		filter memory.FieldFilter
		wantID []string
	}{
		{
			name:   "matching content key: only that entry",
			filter: memory.FieldFilter{Path: "outcome", Value: "denied"},
			wantID: []string{"a-1"},
		},
		{
			name:   "Go field name instead of the json tag: matches nothing",
			filter: memory.FieldFilter{Path: "Outcome", Value: "denied"},
			wantID: nil,
		},
		{
			name:   "numeric value compares as the stored text",
			filter: memory.FieldFilter{Path: "turnIndex", Value: 2},
			wantID: []string{"a-2"},
		},
		{
			name:   "gte spans both entries",
			filter: memory.FieldFilter{Path: "turnIndex", Op: memory.FieldOpGte, Value: 1},
			wantID: []string{"a-1", "a-2"},
		},
		{
			name:   "not_nil excludes the entry whose key is absent",
			filter: memory.FieldFilter{Path: "reason", Op: memory.FieldOpNotNil},
			wantID: []string{"a-1"},
		},
		{
			name:   "is_nil selects the entry whose key is absent",
			filter: memory.FieldFilter{Path: "reason", Op: memory.FieldOpIsNil},
			wantID: []string{"a-2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := inmem.NewBackend()
			scope := memory.Scope{Kind: "session", ID: "ns/a"}
			require.NoError(t, b.Put(context.Background(), memory.Entry{
				Scope: scope, Kind: "alpha", ID: "a-1",
				Content: []byte(`{"outcome":"denied","turnIndex":1,"reason":"policy"}`),
			}))
			require.NoError(t, b.Put(context.Background(), memory.Entry{
				Scope: scope, Kind: "alpha", ID: "a-2",
				Content: []byte(`{"outcome":"allowed","turnIndex":2}`),
			}))

			res, err := b.Query(context.Background(), memory.Query{
				Scope:       scope,
				Kinds:       []string{"alpha"},
				FieldEquals: []memory.FieldFilter{tc.filter},
			})
			require.NoError(t, err)
			assert.False(t, res.Partial, "the predicate is honored, so nothing is dropped")
			assert.Empty(t, res.DroppedPredicates)

			got := make([]string, 0, len(res.Entries))
			for _, e := range res.Entries {
				got = append(got, e.ID)
			}
			sort.Strings(got)
			assert.Equal(t, tc.wantID, nilIfEmpty(got))
		})
	}
}

// TestInMem_Query_FieldEquals_AllPredicatesMustHold pins AND semantics across
// multiple filters, matching the SQL builders' separate WHERE clauses.
func TestInMem_Query_FieldEquals_AllPredicatesMustHold(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		Content: []byte(`{"outcome":"denied","turnIndex":1}`),
	}))

	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, Kinds: []string{"alpha"},
		FieldEquals: []memory.FieldFilter{
			{Path: "outcome", Value: "denied"},
			{Path: "turnIndex", Value: 9},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, res.Entries, "one unsatisfied predicate excludes the entry")
}

// TestInMem_Query_FieldEquals_OpaqueContentNeverMatches covers an entry whose
// content is not a JSON object: every key extraction is NULL, exactly as
// content->>'k' yields for a non-object.
func TestInMem_Query_FieldEquals_OpaqueContentNeverMatches(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1", Content: []byte(`"just a string"`),
	}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-2",
	}))

	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, Kinds: []string{"alpha"},
		FieldEquals: []memory.FieldFilter{{Path: "outcome", Value: "denied"}},
	})
	require.NoError(t, err)
	assert.Empty(t, res.Entries)
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestInMem_Status_UnknownWhenEmpty(t *testing.T) {
	b := inmem.NewBackend()
	st, err := b.Status(context.Background(), memory.Scope{Kind: "session", ID: "ns/a"})
	require.NoError(t, err)
	assert.Equal(t, memory.StatusUnknown, st.State)
}

func TestInMem_Status_LiveWhenNonEmpty(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(context.Background(), memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1"}))

	st, err := b.Status(context.Background(), scope)
	require.NoError(t, err)
	assert.Equal(t, memory.StatusLive, st.State)
}

func TestInMem_Query_LinkedFrom(t *testing.T) {
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-1",
		Links: []memory.Link{{Relation: "of_operation", Kind: "operation", ID: "op-1"}},
	}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-2",
		Links: []memory.Link{{Relation: "of_operation", Kind: "operation", ID: "op-1"}},
	}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-3",
		Links: []memory.Link{{Relation: "of_operation", Kind: "operation", ID: "op-2"}},
	}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{Scope: scope, Kind: "operation", ID: "op-1"}))
	require.NoError(t, b.Put(context.Background(), memory.Entry{Scope: scope, Kind: "operation", ID: "op-2"}))

	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, Kinds: []string{"operation"},
		LinkedFrom: []memory.LinkFilter{{Relation: "of_operation", Kind: "decision", ID: "d-1"}},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "op-1", res.Entries[0].ID)
}

type capKind struct{ cap int }

func (k capKind) Name() string                { return "capped" }
func (k capKind) IDPrefix() string            { return "cap-" }
func (k capKind) Retention() memory.Retention { return memory.Retention{SoftCapPerScope: k.cap} }
func (capKind) ContentSchema() reflect.Type   { return nil }
func (capKind) IndexedFields() []string       { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (capKind) WriteAuthority() memory.WriteAuthority          { return memory.SessionWritten }
func (capKind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return capHooks{} }

type capHooks struct{}

func (capHooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func TestInMem_Eviction_SoftCapPerScope_FIFO(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(capKind{cap: 3})
	b := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	for i := 0; i < 5; i++ {
		require.NoError(t, b.Put(context.Background(), memory.Entry{
			Scope: scope, Kind: "capped", ID: fmt.Sprintf("cap-%d", i),
			CreatedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
		}))
	}

	res, err := b.Query(context.Background(), memory.Query{
		Scope: scope, Kinds: []string{"capped"},
		OrderBy: memory.OrderBy{Field: "createdAt"},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 3)
	assert.Equal(t, "cap-2", res.Entries[0].ID)
	assert.Equal(t, "cap-4", res.Entries[2].ID)
}
