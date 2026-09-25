package extraction_state_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
)

// sqliteMemory builds a Local over the sqlite backend — one of the two
// SHIPPED backends (desktop bundle; postgres is the other), and unlike inmem
// it advertises ContentSchemas and actually renders FieldEquals into SQL. An
// accessor whose FieldFilter.Path does not name a real content key answers
// empty here while staying green on inmem, so every ForTurn accessor is
// exercised against a backend that honors the predicate.
func sqliteMemory(t *testing.T) *memory.Local {
	t.Helper()
	c, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "memory.db"))
	require.NoError(t, err, "sqlite NewClient")
	require.NoError(t, c.Migrate(context.Background()), "sqlite Migrate")
	t.Cleanup(func() { _ = c.Close() })
	return memory.NewLocal(memsqlite.NewBackend(c))
}

func TestExtractionState_Registered(t *testing.T) {
	k, ok := memory.LookupKind("extraction_state")
	require.True(t, ok)
	assert.Equal(t, "exs-", k.IDPrefix())
}

func TestExtractionState_RecordAndForTurn(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")
	start := time.Now().UTC()

	require.NoError(t, extraction_state.Record(ctx, m, scope, extraction_state.Content{
		TurnIndex: 3, Status: extraction_state.StatusPending, StartedAt: start,
	}))
	got, ok, err := extraction_state.ForTurn(ctx, m, scope, 3)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, extraction_state.StatusPending, got.Status)

	require.NoError(t, extraction_state.Record(ctx, m, scope, extraction_state.Content{
		TurnIndex: 3, Status: extraction_state.StatusComplete,
		StartedAt: start, CompletedAt: time.Now().UTC(), CandidateCount: 2,
	}))
	got, ok, err = extraction_state.ForTurn(ctx, m, scope, 3)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, extraction_state.StatusComplete, got.Status, "second Record overwrites first via stable ID")
	assert.Equal(t, 2, got.CandidateCount)
}

// TestExtractionState_ForTurn_SQLiteBackend_ReturnsTheRecordedTurn pins the
// content-key contract for FieldFilter.Path. The accessor once filtered on the
// Go field name "TurnIndex" while the stored key is "turnIndex", so the SQL
// backends extracted NULL and ForTurn answered "no such turn" on every shipped
// backend — silently, because inmem drops FieldEquals and the unit suite runs
// on inmem.
func TestExtractionState_ForTurn_SQLiteBackend_ReturnsTheRecordedTurn(t *testing.T) {
	m := sqliteMemory(t)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, extraction_state.Record(ctx, m, scope, extraction_state.Content{
		TurnIndex: 3, Status: extraction_state.StatusComplete,
		StartedAt: time.Now().UTC(), CandidateCount: 2,
	}))

	got, ok, err := extraction_state.ForTurn(ctx, m, scope, 3)
	require.NoError(t, err)
	require.True(t, ok, "the recorded turn must be found on a backend that honors FieldEquals")
	assert.Equal(t, extraction_state.StatusComplete, got.Status)
	assert.Equal(t, 2, got.CandidateCount)
}

// TestExtractionState_ForTurn_SQLiteBackend_OtherTurnsExcluded proves the
// predicate still discriminates: fixing the path must not degrade to "return
// everything of this kind".
func TestExtractionState_ForTurn_SQLiteBackend_OtherTurnsExcluded(t *testing.T) {
	m := sqliteMemory(t)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, extraction_state.Record(ctx, m, scope, extraction_state.Content{
		TurnIndex: 1, Status: extraction_state.StatusFailed, StartedAt: time.Now().UTC(),
	}))
	require.NoError(t, extraction_state.Record(ctx, m, scope, extraction_state.Content{
		TurnIndex: 2, Status: extraction_state.StatusComplete, StartedAt: time.Now().UTC(),
	}))

	got, ok, err := extraction_state.ForTurn(ctx, m, scope, 2)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, extraction_state.StatusComplete, got.Status, "must return turn 2, not turn 1")
}

func TestExtractionState_AbsentTurn(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	_, ok, err := extraction_state.ForTurn(memory.WithSystemApproval(context.Background(), "test"), m, scope, 99)
	require.NoError(t, err)
	assert.False(t, ok)
}
