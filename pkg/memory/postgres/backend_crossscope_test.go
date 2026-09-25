package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestQueryAllScopes_Postgres(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	mk := func(scopeID, kind, id string, at time.Time) memory.Entry {
		return memory.Entry{Scope: memory.Scope{Kind: "session", ID: scopeID},
			Kind: kind, ID: id, CreatedAt: at, Content: []byte(`{"x":1}`)}
	}
	require.NoError(t, b.Put(ctx, mk("default/s1", "approval", "approval-1", t0)))
	require.NoError(t, b.Put(ctx, mk("default/s2", "approval", "approval-2", t0.Add(time.Minute))))
	require.NoError(t, b.Put(ctx, mk("default/s2", "turn", "turn-0-user", t0.Add(2*time.Minute))))
	require.NoError(t, b.Put(ctx, memory.Entry{Scope: memory.Scope{Kind: "global", ID: "g"},
		Kind: "approval", ID: "approval-g", CreatedAt: t0, Content: []byte(`{}`)}))

	res, err := b.QueryAllScopes(ctx, memory.CrossScopeQuery{
		ScopeKind: "session", Kinds: []string{"approval"}, OrderDesc: true,
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)
	assert.Equal(t, "approval-2", res.Entries[0].ID)
	assert.Equal(t, "default/s2", res.Entries[0].Scope.ID)
	assert.Equal(t, "approval-1", res.Entries[1].ID)

	since := t0.Add(30 * time.Second)
	res, err = b.QueryAllScopes(ctx, memory.CrossScopeQuery{
		ScopeKind: "session", Kinds: []string{"approval", "turn"},
		Since: &since, OrderDesc: true, Limit: 1, Offset: 1,
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "approval-2", res.Entries[0].ID)
}
