package plangateaudit_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

func entry(t *testing.T, c plangateaudit.Content) memory.Entry {
	t.Helper()
	b, err := json.Marshal(c)
	require.NoError(t, err)
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/demo-agent-1"},
		Kind:      plangateaudit.Kind{}.Name(),
		ID:        "pgaud-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   b,
	}
}

func sampleContent() plangateaudit.Content {
	idx := int32(0)
	return plangateaudit.Content{
		Event:      plangateaudit.EventGateWouldDeny,
		PhaseIndex: &idx,
		Handle:     "perm:write:tracker_issue",
		Tool:       "update_issue",
		Outcome:    plangateaudit.OutcomeWouldDeny,
		Mode:       "logging",
		Provenance: "plan_gate",
		At:         time.Unix(1700000000, 0).UTC(),
	}
}

// The gate's durable state is this log, so a record must never be rewritable:
// a rewritten "denied" would erase a human's decision.
func TestPlanGateAudit_isAppendOnlyThroughTheFacade(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := context.Background()

	e := entry(t, sampleContent())
	_, err := mem.Put(ctx, e)
	require.NoError(t, err, "first put creates the record")

	changed := sampleContent()
	changed.Outcome = plangateaudit.OutcomeAllow
	_, err = mem.Put(ctx, entry(t, changed))
	assert.ErrorIs(t, err, memory.ErrAppendOnlyConflict,
		"flipping would_deny to allow must be refused, not silently accepted")
}

// A retried write (network blip, restart mid-put) must be a no-op rather than a
// conflict. The facade compares MARSHALED BYTES, so this holds only while the
// caller reuses the record it already built.
func TestPlanGateAudit_identicalRetryIsIdempotent(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := context.Background()

	e := entry(t, sampleContent())
	_, err := mem.Put(ctx, e)
	require.NoError(t, err)

	_, err = mem.Put(ctx, e)
	assert.NoError(t, err, "byte-identical retry must be an idempotent no-op")
}

// The hazard the field comment on Content.At warns about, pinned as a test.
// memory.entriesEquivalent compares Content bytes, so a caller that re-stamps
// At on retry produces DIFFERENT bytes and turns a safe retry into a hard
// ErrAppendOnlyConflict. At must be stamped once, when the record is built,
// and the same record reused on retry — the same "no volatile value in a
// compared payload" rule the SSA guidance states for applied fields.
func TestPlanGateAudit_restampingAtBreaksRetryIdempotency(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := context.Background()

	first := sampleContent()
	_, err := mem.Put(ctx, entry(t, first))
	require.NoError(t, err)

	restamped := sampleContent()
	restamped.At = first.At.Add(time.Second) // what time.Now() on retry would do

	_, err = mem.Put(ctx, entry(t, restamped))
	assert.ErrorIs(t, err, memory.ErrAppendOnlyConflict,
		"a re-stamped At is a DIFFERENT record to the facade — callers must not re-stamp on retry")
}
