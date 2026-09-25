package extracted_entity_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/extracted_entity"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
)

// sqliteMemory builds a Local over the sqlite backend — one of the two
// SHIPPED backends (desktop bundle; postgres is the other), and unlike inmem
// it advertises ContentSchemas and actually renders FieldEquals into SQL. An
// accessor whose FieldFilter.Path does not name a real content key answers
// empty here while staying green on inmem.
func sqliteMemory(t *testing.T) *memory.Local {
	t.Helper()
	c, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "memory.db"))
	require.NoError(t, err, "sqlite NewClient")
	require.NoError(t, c.Migrate(context.Background()), "sqlite Migrate")
	t.Cleanup(func() { _ = c.Close() })
	return memory.NewLocal(memsqlite.NewBackend(c))
}

func TestExtractedEntity_Registered(t *testing.T) {
	k, ok := memory.LookupKind("extracted_entity")
	require.True(t, ok)
	assert.Equal(t, "eex-", k.IDPrefix())
}

func TestExtractedEntity_RecordAndForTurn(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, extracted_entity.Record(ctx, m, scope, extracted_entity.Content{
		ResourceType: "github_repo", ResourceID: "authzed/spicedb", TurnIndex: 1,
	}))
	require.NoError(t, extracted_entity.Record(ctx, m, scope, extracted_entity.Content{
		ResourceType: "linear_team", ResourceID: "platform", TurnIndex: 1,
	}))
	require.NoError(t, extracted_entity.Record(ctx, m, scope, extracted_entity.Content{
		ResourceType: "github_repo", ResourceID: "demo-org/demo-repo", TurnIndex: 2,
	}))

	got, err := extracted_entity.ForTurn(ctx, m, scope, 1)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

// TestExtractedEntity_ForTurn_SQLiteBackend_ReturnsTheTurnsCandidates pins the
// content-key contract for FieldFilter.Path. The accessor once filtered on the
// Go field name "TurnIndex" while the stored key is "turnIndex", so the SQL
// backends extracted NULL and ForTurn answered empty on every shipped backend —
// silently, because inmem drops FieldEquals and the unit suite runs on inmem.
func TestExtractedEntity_ForTurn_SQLiteBackend_ReturnsTheTurnsCandidates(t *testing.T) {
	m := sqliteMemory(t)
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, extracted_entity.Record(ctx, m, scope, extracted_entity.Content{
		ResourceType: "code_repo", ResourceID: "demo-org/demo-repo", TurnIndex: 1,
	}))
	require.NoError(t, extracted_entity.Record(ctx, m, scope, extracted_entity.Content{
		ResourceType: "tracker_team", ResourceID: "demo-team", TurnIndex: 1,
	}))
	require.NoError(t, extracted_entity.Record(ctx, m, scope, extracted_entity.Content{
		ResourceType: "code_repo", ResourceID: "demo-org/other-repo", TurnIndex: 2,
	}))

	got, err := extracted_entity.ForTurn(ctx, m, scope, 1)
	require.NoError(t, err)
	assert.Len(t, got, 2, "turn 1's two candidates, and not turn 2's")
}
