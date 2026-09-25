package relwritesaudit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
)

func TestRelwritesAudit_Registered(t *testing.T) {
	k, ok := memory.LookupKind("relwrites_audit")
	require.True(t, ok)
	assert.Equal(t, "relw-", k.IDPrefix())
}

func TestRelwritesAudit_RecordAndQuery(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, relwritesaudit.Record(ctx, m, scope, "tu-1", relwritesaudit.Audit{
		Tuples: []relwritesaudit.Tuple{
			{Resource: "github_repo:authzed/spicedb", Relation: "owner", Subject: "user:alice"},
		},
		Source: "mcp.github.create_repo",
	}))

	got, err := relwritesaudit.ByToolCall(ctx, m, scope, "tu-1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "mcp.github.create_repo", got[0].Source)
}
