package infoleakagetaint_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
)

func TestTaintKindRegistration(t *testing.T) {
	k, ok := memory.LookupKind("infoleakage_taint")
	require.True(t, ok)
	assert.Equal(t, "infoleakage_taint", k.Name())
	assert.Equal(t, "ilt-", k.IDPrefix())
}

func TestTaintAppendListRoundTrip(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess1"}

	rec := infoleakagetaint.TaintRecord{
		ToolUseID:    "tu-1",
		AccessedAt:   time.Now().UTC().Truncate(time.Second),
		ResourceType: "linear_issue",
		ResourceID:   "ABC-123",
		Permission:   "view",
	}
	require.NoError(t, infoleakagetaint.Append(ctx, m, scope, rec))

	got, err := infoleakagetaint.List(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, rec.ResourceType, got[0].ResourceType)
	assert.Equal(t, rec.ResourceID, got[0].ResourceID)
	assert.Equal(t, rec.ToolUseID, got[0].ToolUseID)
	assert.Equal(t, rec.Permission, got[0].Permission)
	assert.Equal(t, rec.AccessedAt.Unix(), got[0].AccessedAt.Unix())
}

func TestTaintScopeIsolation(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	s1 := memory.Scope{Kind: "session", ID: "ns/a"}
	s2 := memory.Scope{Kind: "session", ID: "ns/b"}

	require.NoError(t, infoleakagetaint.Append(ctx, m, s1,
		infoleakagetaint.TaintRecord{ResourceType: "t", ResourceID: "x", Permission: "view"}))
	require.NoError(t, infoleakagetaint.Append(ctx, m, s2,
		infoleakagetaint.TaintRecord{ResourceType: "t", ResourceID: "y", Permission: "view"}))

	gotA, err := infoleakagetaint.List(ctx, m, s1)
	require.NoError(t, err)
	gotB, err := infoleakagetaint.List(ctx, m, s2)
	require.NoError(t, err)

	require.Len(t, gotA, 1)
	require.Len(t, gotB, 1)
	assert.Equal(t, "x", gotA[0].ResourceID)
	assert.Equal(t, "y", gotB[0].ResourceID)
}
