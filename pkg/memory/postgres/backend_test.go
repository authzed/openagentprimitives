package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	mempostgres "github.com/authzed/openagentprimitives/pkg/memory/postgres"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/test/testpostgres"
)

// TestMain purges the container testpostgres may have started, so a `go test`
// run of this package leaves nothing behind. It is a no-op when the tests ran
// against an operator-supplied POSTGRES_URI, or skipped entirely.
func TestMain(m *testing.M) {
	code := m.Run()
	testpostgres.Stop()
	os.Exit(code)
}

func newTestBackend(t *testing.T) *mempostgres.Backend {
	t.Helper()
	// testpostgres.URI prefers an operator-supplied POSTGRES_URI, otherwise
	// starts one container for this test binary when AP_TEST_POSTGRES=docker,
	// otherwise skips with a message naming the mage target that sets it. Every
	// test in this package used to skip unconditionally without POSTGRES_URI,
	// so the durable memory backend every remote install runs was exercised by
	// none of the three ship-gate suites.
	uri := testpostgres.URI(t)
	ctx := context.Background()
	client, err := mempostgres.NewClient(ctx, uri)
	require.NoError(t, err)
	t.Cleanup(client.Close)
	require.NoError(t, client.Migrate(ctx))

	// Clean tables between tests for isolation. The container (or the supplied
	// database) is shared by the whole binary, so a truncation that silently
	// failed would leak rows into the NEXT test and surface as a baffling
	// unrelated failure — report it instead of dropping the error.
	pool := client.Pool()
	t.Cleanup(func() {
		for _, stmt := range []string{"DELETE FROM memory_link", "DELETE FROM memory_entry"} {
			if _, err := pool.Exec(context.Background(), stmt); err != nil {
				t.Errorf("postgres fixture cleanup %q: %v (later tests in this package may see leaked rows)", stmt, err)
			}
		}
	})
	return mempostgres.NewBackend(client)
}

func TestPostgres_Capabilities(t *testing.T) {
	b := newTestBackend(t)
	caps := b.Capabilities()
	assert.True(t, caps.ContentSchemas)
	assert.True(t, caps.FieldRange)
	assert.Equal(t, -1, caps.LinkTraversal)
	assert.True(t, caps.ReverseLinks)
	assert.True(t, caps.TagFilters)
	assert.True(t, caps.TimeRange)
}

func TestPostgres_PutGetRoundtrip(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	e := memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
		Tags:      []string{"x"},
	}
	require.NoError(t, b.Put(ctx, e))
	got, ok, err := b.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, e.ID, got.ID)
	assert.Equal(t, e.Kind, got.Kind)
	assert.Equal(t, e.Tags, got.Tags)
}

func TestPostgres_PutGetWithContent(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	content := json.RawMessage(`{"outcome":"approved","score":42}`)
	e := memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
		Content:   content,
	}
	require.NoError(t, b.Put(ctx, e))
	got, ok, err := b.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, string(content), string(got.Content))
}

func TestPostgres_PutGetWithLinks(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	e := memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-1",
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
		Links: []memory.Link{
			{Relation: "for_resource", Scope: scope, Kind: "repo", ID: "r-1"},
		},
	}
	require.NoError(t, b.Put(ctx, e))
	got, ok, err := b.Get(ctx, scope, "decision", "d-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, got.Links, 1)
	assert.Equal(t, "for_resource", got.Links[0].Relation)
	assert.Equal(t, "repo", got.Links[0].Kind)
	assert.Equal(t, "r-1", got.Links[0].ID)
}

func TestPostgres_GetMissReturnsFalse(t *testing.T) {
	b := newTestBackend(t)
	_, ok, err := b.Get(context.Background(),
		memory.Scope{Kind: "session", ID: "ns/a"}, "alpha", "absent")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPostgres_PutOverwrite(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		CreatedAt: time.Now().UTC(), Tags: []string{"old"},
	}))
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		CreatedAt: time.Now().UTC(), Tags: []string{"new"},
	}))

	got, ok, err := b.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []string{"new"}, got.Tags)
}

func TestPostgres_DeleteIdempotent(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, b.Delete(ctx, scope, "alpha", "a-1"))
	require.NoError(t, b.Delete(ctx, scope, "alpha", "a-1"))
	_, ok, _ := b.Get(ctx, scope, "alpha", "a-1")
	assert.False(t, ok)
}

func TestPostgres_DeleteScope(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "beta", ID: "b-1", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, b.DeleteScope(ctx, scope))
	res, err := b.Query(ctx, memory.Query{Scope: scope})
	require.NoError(t, err)
	assert.Empty(t, res.Entries)
}

func TestPostgres_Query_KindFilter(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "beta", ID: "b-1", CreatedAt: time.Now().UTC(),
	}))
	res, err := b.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"alpha"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "alpha", res.Entries[0].Kind)
}

func TestPostgres_Query_TagFilter(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		CreatedAt: time.Now().UTC(), Tags: []string{"important", "urgent"},
	}))
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-2",
		CreatedAt: time.Now().UTC(), Tags: []string{"important"},
	}))
	res, err := b.Query(ctx, memory.Query{
		Scope: scope, Tags: []string{"important", "urgent"},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-1", res.Entries[0].ID)
}

func TestPostgres_Query_FieldEquals(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1",
		CreatedAt: time.Now().UTC(),
		Content:   json.RawMessage(`{"outcome":"approved"}`),
	}))
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-2",
		CreatedAt: time.Now().UTC(),
		Content:   json.RawMessage(`{"outcome":"denied"}`),
	}))
	res, err := b.Query(ctx, memory.Query{
		Scope: scope,
		FieldEquals: []memory.FieldFilter{
			{Path: "outcome", Op: memory.FieldOpEq, Value: "approved"},
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-1", res.Entries[0].ID)
}

func TestPostgres_Query_TimeRange(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, ts := range []time.Time{t0, t0.Add(time.Hour), t0.Add(2 * time.Hour)} {
		require.NoError(t, b.Put(ctx, memory.Entry{
			Scope: scope, Kind: "alpha", ID: fmt.Sprintf("a-%d", i+1), CreatedAt: ts,
		}))
	}
	since := t0.Add(30 * time.Minute)
	until := t0.Add(90 * time.Minute)
	res, err := b.Query(ctx, memory.Query{
		Scope: scope, Since: &since, Until: &until,
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-2", res.Entries[0].ID)
}

func TestPostgres_Query_LinkedTo(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-1",
		CreatedAt: time.Now().UTC(),
		Links:     []memory.Link{{Relation: "for_resource", Scope: scope, Kind: "repo", ID: "r-1"}},
	}))
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-2",
		CreatedAt: time.Now().UTC(),
		Links:     []memory.Link{{Relation: "for_resource", Scope: scope, Kind: "repo", ID: "r-2"}},
	}))
	res, err := b.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{"decision"},
		LinkedTo: []memory.LinkFilter{{Relation: "for_resource", Kind: "repo", ID: "r-1"}},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "d-1", res.Entries[0].ID)
}

func TestPostgres_Query_LinkedFrom(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-1",
		CreatedAt: time.Now().UTC(),
		Links:     []memory.Link{{Relation: "of_operation", Scope: scope, Kind: "operation", ID: "op-1"}},
	}))
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "decision", ID: "d-2",
		CreatedAt: time.Now().UTC(),
		Links:     []memory.Link{{Relation: "of_operation", Scope: scope, Kind: "operation", ID: "op-1"}},
	}))
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "operation", ID: "op-1", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "operation", ID: "op-2", CreatedAt: time.Now().UTC(),
	}))
	res, err := b.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{"operation"},
		LinkedFrom: []memory.LinkFilter{{Relation: "of_operation", Kind: "decision", ID: "d-1"}},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "op-1", res.Entries[0].ID)
}

func TestPostgres_Query_OrderAndLimit(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		require.NoError(t, b.Put(ctx, memory.Entry{
			Scope: scope, Kind: "alpha", ID: fmt.Sprintf("a-%d", i),
			CreatedAt: t0.Add(time.Duration(i) * time.Hour),
		}))
	}
	res, err := b.Query(ctx, memory.Query{
		Scope:   scope,
		OrderBy: memory.OrderBy{Field: "createdAt", Desc: true},
		Limit:   2,
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)
	assert.Equal(t, "a-4", res.Entries[0].ID)
	assert.Equal(t, "a-3", res.Entries[1].ID)
}

func TestPutGetRoundTripsProvenance(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	e := memory.Entry{
		Scope:     scope,
		Kind:      "alpha",
		ID:        "a-prov-1",
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
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
	// Query round-trips provenance too (scanEntryFromRows path).
	res, err := b.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"alpha"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, e.Provenance, res.Entries[0].Provenance)
}

// TestProvenanceDigestSurvivesRoundTrip is the CI guard for two postgres
// seams. (1) JSONB: postgres stores Content as JSONB, which reshapes the
// bytes (key reordering, whitespace, number normalization) on round-trip.
// Content keys here are deliberately non-sorted so a naive verbatim-bytes
// digest would differ after the round-trip. (2) TIMESTAMPTZ: postgres
// stores CreatedAt at microsecond precision, so the nanosecond CreatedAt
// below is DELIBERATE — it's the regression guard for the digest
// microsecond-truncation fix. The digest must canonicalize both content
// and timestamp so a signature survives the round-trip; without
// truncation, the stored value loses nanoseconds and the digest would
// differ.
func TestProvenanceDigestSurvivesRoundTrip(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	written := memory.Entry{
		Scope: scope,
		Kind:  "alpha",
		ID:    "a-digest-1",
		// Nanosecond precision, NOT pre-truncated: postgres will drop the
		// sub-microsecond part. EntryDigest truncates on both sides so the
		// digests still match — that's the regression this guards.
		CreatedAt: time.Now().UTC().Add(123 * time.Nanosecond),
		Content:   json.RawMessage(`{"zeta":1,"alpha":"x","beta":[3,2,1]}`),
		Provenance: &memory.Provenance{
			Publisher: "system:test",
			KeyID:     "abcd1234",
			Seq:       1,
			Sig:       []byte{9, 8, 7},
		},
	}
	require.NoError(t, b.Put(ctx, written))
	readBack, ok, err := b.Get(ctx, scope, "alpha", "a-digest-1")
	require.NoError(t, err)
	require.True(t, ok)

	assert.Equal(t, provenance.EntryDigest(written), provenance.EntryDigest(readBack),
		"digest must survive the JSONB content + TIMESTAMPTZ microsecond round-trip")
}

func TestPostgres_Status(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	st, err := b.Status(ctx, scope)
	require.NoError(t, err)
	assert.Equal(t, memory.StatusUnknown, st.State)

	require.NoError(t, b.Put(ctx, memory.Entry{
		Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC(),
	}))
	st, err = b.Status(ctx, scope)
	require.NoError(t, err)
	assert.Equal(t, memory.StatusLive, st.State)
}
