package postgres_test

// appendonly_facade_test.go drives the memory.Local append-only contract
// against a REAL PostgreSQL server. Every other test of that contract runs over
// inmem, which hands entries back verbatim; postgres does not, and the
// difference is the whole point:
//
//   - content is a JSONB column, so it comes back with object keys reordered
//     and whitespace normalized;
//   - created_at is a TIMESTAMPTZ column, so the pgx encoder truncates it to
//     microseconds and the nanoseconds a time.Now() CreatedAt carries are gone;
//   - tags/links come back through normalizeEntry, which maps empty to nil.
//
// Local.Put's append-only pre-check Get reads through the shadow backend's read
// source, which the shipped configuration leaves at the postgres secondary. So
// a comparison of raw bytes there is broken in exactly the mode every remote
// install runs, and green in every mode a developer tests.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// TestPostgresAppendOnly_IdenticalRePutIsIdempotent pins the append-only
// contract's idempotency arm against postgres: a retried write of the same
// entry is answered with the stored entry, not ErrAppendOnlyConflict.
//
// The turn Kind is used rather than a test-local one because it is registered
// (init) and append-only, so the test needs no global registry mutation — this
// binary's other tests share that registry.
func TestPostgresAppendOnly_IdenticalRePutIsIdempotent(t *testing.T) {
	mem := memory.NewLocal(newTestBackend(t))
	ctx := context.Background()

	e := memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "default/pg-append"},
		Kind:  turn.KindName,
		ID:    turn.EntryID(0, "user"),
		// Sub-microsecond precision, so TIMESTAMPTZ truncation bites, and
		// multi-key nested content, so JSONB key reordering bites.
		CreatedAt: time.Date(2026, 8, 11, 12, 0, 0, 123456789, time.UTC),
		Content:   json.RawMessage(`{"zeta":1,"alpha":{"b":2,"a":"x"},"content":[{"type":"text","text":"hello"}]}`),
		Tags:      []string{"t1"},
	}

	stored, err := mem.Put(ctx, e)
	require.NoError(t, err, "first put creates the entry")

	again, err := mem.Put(ctx, e)
	require.NoError(t, err, "re-put of an identical entry must be an idempotent no-op")
	assert.Equal(t, stored.ID, again.ID, "the idempotent re-put returns the stored entry")

	// Genuinely different content at the same append-only ID must still be
	// refused — the canonical comparison may not be so loose that it stops
	// detecting a rewrite of the tamper-evident log.
	e2 := e
	e2.Content = json.RawMessage(`{"zeta":2,"alpha":{"b":2,"a":"x"},"content":[{"type":"text","text":"hello"}]}`)
	_, err = mem.Put(ctx, e2)
	assert.ErrorIs(t, err, memory.ErrAppendOnlyConflict,
		"different content at the same append-only ID is still a conflict")

	// And the stored entry is the ORIGINAL: a refused re-put must not have
	// upserted over it (the postgres Put is an upsert; only the pre-check
	// stands between a conflicting write and the durable row).
	readBack, found, err := mem.Get(ctx, e.Scope, e.Kind, e.ID)
	require.NoError(t, err)
	require.True(t, found, "the entry is still stored")
	assert.JSONEq(t, string(e.Content), string(readBack.Content),
		"the original content survived the refused re-put")
}
