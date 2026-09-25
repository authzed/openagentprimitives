package infoleakagetaint_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
)

// captureSlog swaps the default slog logger for one writing into the returned
// buffer, restoring the previous logger at test end. Mutates package-global
// state, so no test using it may be parallel.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestList_UndecodableTaintRefusesTheReadAndNamesTheWriter pins the direction
// this list must NOT degrade in.
//
// Sibling Kinds whose rows are a RECORD (turn, toolsession, the audit Kinds)
// skip an entry that will not decode, because a hard error on an append-only
// row that can never be deleted is a permanent wedge. A taint row is not a
// record: it is the input to the respond-time audience gate, which denies when
// this read errors. Skipping one would make the gate forget a taint — and a
// forgotten taint PERMITS the leak the row was written to catch. So the read
// stays fail-closed, and this test exists so a future sweep toward "tolerate
// everything" cannot quietly turn a denial of service into a disclosure.
//
// The log line is the other half: with the row undeletable, naming the entry
// and its publisher is the operator's only route to a remedy, and the returned
// error reaches the caller as a Deny reason that names neither.
func TestList_UndecodableTaintRefusesTheReadAndNamesTheWriter(t *testing.T) {
	logs := captureSlog(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/s1"}

	require.NoError(t, infoleakagetaint.Append(ctx, m, scope, infoleakagetaint.TaintRecord{
		ResourceType: "issue", ResourceID: "ENG-1", Permission: "view",
	}), "seed one well-formed taint record")

	_, err := m.Put(ctx, memory.Entry{
		Scope: scope, Kind: "infoleakage_taint", ID: "ilt-poison",
		Content:    []byte("{not json"),
		Provenance: &memory.Provenance{Publisher: "session:default/s1", KeyID: "k1"},
	})
	require.NoError(t, err, "the poisoned entry is accepted — Put never validates content")

	_, err = infoleakagetaint.List(ctx, m, scope)
	require.Error(t, err, "an undecodable taint row must fail the read, never vanish from the taint set")
	assert.Contains(t, err.Error(), "ilt-poison", "the error names the entry that failed")
	assert.Contains(t, logs.String(), "ilt-poison", "the refused entry's ID reaches the log")
	assert.Contains(t, logs.String(), "session:default/s1", "the publisher that signed it reaches the log")
}
