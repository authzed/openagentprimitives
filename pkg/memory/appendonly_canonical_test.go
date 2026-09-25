package memory_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/memtest"
)

// TestPutAppendOnlyIdempotentThroughDurableBackend pins the property the
// append-only contract actually promises: a retried write of the SAME entry is
// answered with the stored entry, not ErrAppendOnlyConflict — including when
// the pre-check Get reads it back from a backend that reshapes it.
//
// The shipped configuration is exactly that case. Content is a JSONB column
// and created_at a TIMESTAMPTZ column, and MEMORY_READ_SOURCE defaults to the
// postgres secondary, so Local.Put's pre-check Get compares a caller's entry
// against a reshaped one. TestPutAppendOnlyConflict covers the same contract
// over plain inmem, which stores entries verbatim and therefore cannot see
// this: that asymmetry is why the raw-byte comparison stayed green.
func TestPutAppendOnlyIdempotentThroughDurableBackend(t *testing.T) {
	// The registered turn Kind, not a test-local one: registering would mean
	// resetting the registry on cleanup, and a reset leaves it EMPTY for every
	// test that runs after this file (they are ordered by filename, and this
	// one sorts early).
	mem := memory.NewLocal(memtest.NewRoundTrip(inmem.NewBackend()))
	ctx := context.Background()

	// Multi-key content (so key order can change) and a CreatedAt with
	// sub-microsecond precision (so truncation bites) — the two hazards a
	// durable round trip introduces.
	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:      "turn",
		ID:        "turn-000000-user",
		CreatedAt: time.Date(2026, 8, 11, 12, 0, 0, 123456789, time.UTC),
		Content:   json.RawMessage(`{"zeta":1,"alpha":{"b":2,"a":"x"}}`),
		Tags:      []string{"t1"},
	}
	stored, err := mem.Put(ctx, e)
	require.NoError(t, err, "first put creates")

	again, err := mem.Put(ctx, e)
	require.NoError(t, err, "re-put of an identical entry must be an idempotent no-op")
	assert.Equal(t, stored.ID, again.ID, "the idempotent re-put returns the stored entry")

	// The conflict door must still shut on genuinely different content — the
	// canonical comparison may not be so loose that it stops detecting tampering.
	e2 := e
	e2.Content = json.RawMessage(`{"zeta":2,"alpha":{"b":2,"a":"x"}}`)
	_, err = mem.Put(ctx, e2)
	assert.ErrorIs(t, err, memory.ErrAppendOnlyConflict,
		"different content at the same append-only ID is still a conflict")
}
