package inmem_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

func put(t *testing.T, b *inmem.Backend, scopeID, kind, id string, at time.Time) {
	t.Helper()
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: scopeID}, Kind: kind, ID: id,
		CreatedAt: at, Content: []byte(`{"x":1}`),
	}))
}

func TestQueryAllScopes_Inmem(t *testing.T) {
	b := inmem.NewBackend()
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	put(t, b, "default/s1", "approval", "approval-1", t0)
	put(t, b, "default/s2", "approval", "approval-2", t0.Add(time.Minute))
	put(t, b, "default/s2", "turn", "turn-0-user", t0.Add(2*time.Minute))
	put(t, b, "other/s3", "authz_decision", "authzd-1", t0.Add(3*time.Minute))
	// Different scope KIND — must never match a "session" cross-scope query.
	require.NoError(t, b.Put(context.Background(), memory.Entry{
		Scope: memory.Scope{Kind: "global", ID: "g"}, Kind: "approval", ID: "approval-g",
		CreatedAt: t0, Content: []byte(`{}`),
	}))

	ctx := context.Background()

	t.Run("kinds filter spans all scopes, newest first", func(t *testing.T) {
		res, err := b.QueryAllScopes(ctx, memory.CrossScopeQuery{
			ScopeKind: "session", Kinds: []string{"approval", "authz_decision"}, OrderDesc: true,
		})
		require.NoError(t, err)
		require.Len(t, res.Entries, 3)
		assert.Equal(t, "authzd-1", res.Entries[0].ID)
		assert.Equal(t, "approval-2", res.Entries[1].ID)
		assert.Equal(t, "approval-1", res.Entries[2].ID)
		assert.Equal(t, "other/s3", res.Entries[0].Scope.ID, "scope must ride along on each entry")
	})

	t.Run("time range + limit + offset paginate deterministically", func(t *testing.T) {
		since := t0.Add(30 * time.Second)
		res, err := b.QueryAllScopes(ctx, memory.CrossScopeQuery{
			ScopeKind: "session", Kinds: []string{"approval", "turn", "authz_decision"},
			Since: &since, OrderDesc: true, Limit: 2,
		})
		require.NoError(t, err)
		require.Len(t, res.Entries, 2)
		assert.Equal(t, "authzd-1", res.Entries[0].ID)
		assert.Equal(t, "turn-0-user", res.Entries[1].ID)

		res, err = b.QueryAllScopes(ctx, memory.CrossScopeQuery{
			ScopeKind: "session", Kinds: []string{"approval", "turn", "authz_decision"},
			Since: &since, OrderDesc: true, Limit: 2, Offset: 2,
		})
		require.NoError(t, err)
		require.Len(t, res.Entries, 1)
		assert.Equal(t, "approval-2", res.Entries[0].ID)
	})
}

func TestQueryAllScopes_FacadeValidation(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	_, err := m.QueryAllScopes(context.Background(), memory.CrossScopeQuery{Kinds: []string{"approval"}})
	assert.Error(t, err, "empty ScopeKind must be rejected")
	_, err = m.QueryAllScopes(context.Background(), memory.CrossScopeQuery{ScopeKind: "session"})
	assert.Error(t, err, "empty Kinds must be rejected")
}
