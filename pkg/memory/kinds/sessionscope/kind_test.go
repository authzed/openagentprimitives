package sessionscope_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

func TestSessionScope_Registered(t *testing.T) {
	k, ok := memory.LookupKind("session_scope")
	require.True(t, ok)
	assert.Equal(t, "scp-", k.IDPrefix())
}

func TestSessionScope_ContentSchemaIsScope(t *testing.T) {
	k, _ := memory.LookupKind("session_scope")
	assert.Equal(t, reflect.TypeOf(scope.Scope{}), k.ContentSchema())
}

func TestSessionScope_RetentionSignals(t *testing.T) {
	k, _ := memory.LookupKind("session_scope")
	r := k.Retention()
	assert.Contains(t, r.ArchiveOn, lifecycle.SigSessionCompleted)
	assert.Contains(t, r.ArchiveOn, lifecycle.SigSessionFailed)
	assert.True(t, r.TTLAfterArchive > 0)
}

func TestSessionScope_PutGet(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/a"}

	s := scope.Scope{
		Resources:    []scope.ScopeResource{{ResourceType: "github_repo", IDs: []string{"foo/bar"}}},
		ScopeVersion: 1,
	}
	require.NoError(t, sessionscope.Put(ctx, m, sc, s))

	got, found, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, s, got)
}

func TestSessionScope_GetMissing(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "fresh"}

	got, found, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, scope.Scope{}, got)
}

func TestSessionScope_PutOverwrites(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/b"}

	require.NoError(t, sessionscope.Put(ctx, m, sc, scope.Scope{ScopeVersion: 1}))
	require.NoError(t, sessionscope.Put(ctx, m, sc, scope.Scope{ScopeVersion: 2}))

	got, _, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.ScopeVersion, "second Put overwrites first via stable ID")

	// Confirm only one entry exists.
	res, err := m.Query(ctx, memory.Query{Scope: sc, Kinds: []string{"session_scope"}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
}

func TestSessionScope_OnSignalIsNoop(t *testing.T) {
	k, _ := memory.LookupKind("session_scope")
	hooks := k.NewScopeHooks(memory.Scope{Kind: "session", ID: "ns/a"})
	// Both retention signals should not produce an error.
	require.NoError(t, hooks.OnSignal(context.Background(), memory.Signal{Kind: lifecycle.SigSessionCompleted}))
	require.NoError(t, hooks.OnSignal(context.Background(), memory.Signal{Kind: lifecycle.SigSessionFailed}))
}
