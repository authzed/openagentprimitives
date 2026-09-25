package memory_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestUserScope(t *testing.T) {
	s, err := memory.UserScope("YWxpY2U")
	require.NoError(t, err)
	assert.Equal(t, memory.Scope{Kind: memory.ScopeKindUser, ID: "YWxpY2U"}, s)

	for _, bad := range []string{"", "a/b", "a:b"} {
		_, err := memory.UserScope(bad)
		assert.Error(t, err, "id %q must be refused", bad)
	}
}

func TestSystemContext_ClearsTokenSessionAndCaller(t *testing.T) {
	ctx := memory.WithTokenSession(context.Background(), memory.NamespacedName{Namespace: "ns", Name: "s"})
	ctx = memory.WithCaller(ctx, "someone")
	ctx = memory.SystemContext(ctx, "test-component")

	_, ok := memory.TokenSessionFrom(ctx)
	assert.False(t, ok, "token session must be cleared")

	callerID, callerOK := memory.CallerFrom(ctx)
	assert.False(t, callerOK, "caller must be cleared")
	assert.Empty(t, callerID, "caller must be cleared")
}
