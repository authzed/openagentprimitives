package sessionscope_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

func casScope(t *testing.T) (context.Context, memory.Memory, memory.Scope) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	return ctx, memory.NewLocal(inmem.NewBackend()), memory.Scope{Kind: "session", ID: "ns/sess"}
}

// session_scope is read-modify-written by TWO independent writers: authzd,
// applying a human's scope decision, and the runner, appending bound slots on
// every dispatch round once any observed fact exists. Put replaced the whole
// document with no version check, and memory.Entry carries no version, so the
// later write silently discarded whatever the other had just applied.
//
// The consequence that matters is directional. Disallow is the sole hard-deny
// enforcement surface, so a lost update can drop a HardDeny the owner just
// clicked — and drop it silently: no error, and the scope_audit record still
// says it was applied. The runner's write is frequent and its timing is not
// something the losing party controls.
func TestPutIfVersion_RefusesAStaleWrite(t *testing.T) {
	ctx, m, sc := casScope(t)

	require.NoError(t, sessionscope.Put(ctx, m, sc, scope.Scope{ScopeVersion: 1}))

	// Two readers both see version 1.
	first, _, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)
	second, _, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)

	// The human's hard deny lands first.
	denied := first
	denied.ScopeVersion = first.ScopeVersion + 1
	denied.Disallow = []scope.ScopeResource{{ResourceType: "github_repo", IDs: []string{"acme/secret"}}}
	require.NoError(t, sessionscope.PutIfVersion(ctx, m, sc, denied, first.ScopeVersion))

	// The runner's slot append, computed from the pre-deny read, must NOT
	// overwrite it.
	stale := second
	stale.ScopeVersion = second.ScopeVersion + 1
	err = sessionscope.PutIfVersion(ctx, m, sc, stale, second.ScopeVersion)
	require.Error(t, err, "a write computed from a superseded read must be refused, not applied")
	assert.ErrorIs(t, err, sessionscope.ErrVersionConflict)

	got, _, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)
	require.Len(t, got.Disallow, 1, "the hard deny must survive the concurrent write")
	assert.Equal(t, "acme/secret", got.Disallow[0].IDs[0])
}

// The ordinary case: a writer holding the current version wins.
func TestPutIfVersion_AcceptsAWriteFromTheCurrentVersion(t *testing.T) {
	ctx, m, sc := casScope(t)
	require.NoError(t, sessionscope.Put(ctx, m, sc, scope.Scope{ScopeVersion: 3}))

	cur, _, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)

	next := cur
	next.ScopeVersion = cur.ScopeVersion + 1
	next.Resources = []scope.ScopeResource{{ResourceType: "github_repo", IDs: []string{"acme/app"}}}
	require.NoError(t, sessionscope.PutIfVersion(ctx, m, sc, next, cur.ScopeVersion))

	got, _, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)
	assert.Equal(t, int64(4), got.ScopeVersion)
	require.Len(t, got.Resources, 1)
}

// The first write for a session has nothing to conflict with.
func TestPutIfVersion_AllowsTheFirstWrite(t *testing.T) {
	ctx, m, sc := casScope(t)

	require.NoError(t, sessionscope.PutIfVersion(ctx, m, sc, scope.Scope{ScopeVersion: 1}, 0))

	got, _, err := sessionscope.Get(ctx, m, sc)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.ScopeVersion)
}
