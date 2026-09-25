package toolguardaudit_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolguardaudit"
)

func TestRecordAndList(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/s"}
	c := toolguardaudit.Content{
		Event: "breaker_opened", Tool: "github_search",
		Origin: "mcpserver/github", Key: "tool/github_search",
		UseID: "tu_1", Trips: 1, CoolOff: "30s",
		RetryAt: time.Unix(2000, 0).UTC(), Action: "deny", Provenance: "builtin",
	}
	require.NoError(t, toolguardaudit.Record(memory.WithSystemApproval(context.Background(), "test"), m, scope, c))

	got, err := toolguardaudit.List(memory.WithSystemApproval(context.Background(), "test"), m, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "breaker_opened", got[0].Event)
	assert.Equal(t, "github_search", got[0].Tool)
	assert.Equal(t, int32(1), got[0].Trips)
	assert.False(t, got[0].At.IsZero(), "At auto-stamped")
}

func TestRegistered(t *testing.T) {
	k, ok := memory.LookupKind("toolguard_audit")
	require.True(t, ok)
	assert.Equal(t, "tgaud-", k.IDPrefix())
	assert.Equal(t, reflect.TypeOf(toolguardaudit.Content{}), k.ContentSchema())
}

func TestAutoAt(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/b"}
	require.NoError(t, toolguardaudit.Record(ctx, m, sc, toolguardaudit.Content{
		Event: "guard_deny", Tool: "some_tool", Key: "k", Action: "deny", Provenance: "builtin",
	}))
	got, err := toolguardaudit.List(ctx, m, sc)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].At.IsZero())
}

func TestEmptyListReturnsEmpty(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	sc := memory.Scope{Kind: "session", ID: "fresh"}
	got, err := toolguardaudit.List(memory.WithSystemApproval(context.Background(), "test"), m, sc)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, got)
}
