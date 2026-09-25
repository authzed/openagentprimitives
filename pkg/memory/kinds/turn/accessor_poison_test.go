package turn_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// captureSlog swaps the default slog logger for one writing into the returned
// buffer, restoring the previous logger at test end. The skip path reports
// through slog.Default(), so this is how a test observes that a dropped record
// was announced rather than swallowed.
//
// It mutates package-global state, so no test using it may be parallel.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestReadAll_PoisonedTurnEntryIsSkippedNotFatal pins the degradation the
// transcript read owes a session.
//
// turn is BOTH append-only and session-written, so a session's own bearer may
// Put an entry whose ID merely starts with "turn-" — Local.Put validates the
// Kind, the write authority and the ID PREFIX, and nothing else: ContentSchema()
// is never enforced at write, and turn declares none anyway. EntryToTurn Sscanfs
// the ID, so "turn-x" with EMPTY content is enough; no malformed content is
// required. Nothing can then remove it: per-entry Delete is refused
// unconditionally for an append-only Kind and DeleteScope skips it.
//
// A hard error in the ReadAll loop therefore made every read of that scope fail
// forever, and — unlike lifecycle's fold, which at least filters by EventTag —
// no tag narrows which entries reach the decoder, so there is no blast-radius
// limit. Four independent readers wedge permanently on it: the operator's
// restart/fork reconcile (pkg/controllers/agentsession/restart.go:381,394 via
// lastParentTurnIndex), every restarted runner pod's replay
// (pkg/agent/runner/loop.go:1807), every inbox drain
// (pkg/agent/runner/queuedmessages.go:28), and the resumed browser transcript
// (pkg/web/webui/livemirror/history.go:65).
//
// One unreadable record must degrade the transcript, not end the session.
func TestReadAll_PoisonedTurnEntryIsSkippedNotFatal(t *testing.T) {
	logs := captureSlog(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/s1"}
	a := turn.NewAppender(m, scope)

	require.NoError(t, a.Append(ctx, textTurn(0, "user", "hello")), "seed one well-formed turn")

	// The poison. Written through the same Put a session bearer reaches over
	// HTTP: prefix "turn-", no index, no role, no content at all.
	_, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      "turn",
		ID:        "turn-x",
		CreatedAt: time.Unix(1, 0).UTC(),
	})
	require.NoError(t, err, "the poisoned entry is accepted — Put validates the ID prefix, never the shape")

	out, err := a.ReadAll(ctx)
	require.NoError(t, err, "one unreadable entry must not fail the whole transcript read")
	require.Len(t, out, 1, "the well-formed turn survives the poisoned one")
	assert.Equal(t, "hello", out[0].Content[0].Text)

	// The drop must be findable by an operator: the entry that vanished, and
	// who signed it (AGENTS.md — never silently drop an error).
	assert.Contains(t, logs.String(), "turn-x", "the skipped entry's ID must reach the log")
	assert.Contains(t, logs.String(), "publisher", "the skipped entry's publisher must reach the log")
}

// TestReadAll_SkippedEntryNamesItsPublisher proves the log line identifies the
// WRITER, not just the row. A poisoned append-only entry cannot be deleted, so
// the only remedy available to an operator is to go fix whoever is producing
// them — which requires naming them.
func TestReadAll_SkippedEntryNamesItsPublisher(t *testing.T) {
	logs := captureSlog(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/s1"}

	_, err := m.Put(ctx, memory.Entry{
		Scope: scope, Kind: "turn", ID: "turn-x",
		CreatedAt:  time.Unix(1, 0).UTC(),
		Provenance: &memory.Provenance{Publisher: "session:default/s1", KeyID: "k1"},
	})
	require.NoError(t, err, "Put accepts the entry")

	out, err := turn.ReadAll(ctx, m, scope)
	require.NoError(t, err, "the read degrades rather than halting")
	assert.Empty(t, out)
	assert.Contains(t, logs.String(), "session:default/s1", "the publisher that signed the unreadable entry")
}
