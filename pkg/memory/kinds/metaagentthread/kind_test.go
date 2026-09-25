package metaagentthread_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentthread"
)

func TestMetaagentThread_Registered(t *testing.T) {
	k, ok := memory.LookupKind("metaagent_thread")
	require.True(t, ok)
	assert.Equal(t, "mth-", k.IDPrefix())
	assert.Equal(t, reflect.TypeOf(metaagentthread.Content{}), k.ContentSchema())
}

func TestMetaagentThread_AppendAndList(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/a"}

	now := time.Now()
	msgs := []metaagentthread.Content{
		{Ts: now, Role: metaagentthread.RoleUserMention, Body: "@metaagent add github access", RequesterID: "user:alice"},
		{Ts: now.Add(time.Second), Role: metaagentthread.RoleApprovalBlock, Body: "pending approval"},
		{Ts: now.Add(2 * time.Second), Role: metaagentthread.RoleMetaagentNotice, Body: "Access approved"},
	}
	for _, msg := range msgs {
		require.NoError(t, metaagentthread.Append(ctx, m, sc, msg))
	}

	got, err := metaagentthread.List(ctx, m, sc)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, metaagentthread.RoleUserMention, got[0].Role)
	assert.Equal(t, "user:alice", got[0].RequesterID)
	assert.Equal(t, metaagentthread.RoleApprovalBlock, got[1].Role)
	assert.Equal(t, metaagentthread.RoleMetaagentNotice, got[2].Role)
}

func TestMetaagentThread_EmptyListReturnsEmpty(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	sc := memory.Scope{Kind: "session", ID: "fresh"}
	msgs, err := metaagentthread.List(memory.WithSystemApproval(context.Background(), "test"), m, sc)
	require.NoError(t, err)
	assert.Empty(t, msgs)
	assert.NotNil(t, msgs)
}

func TestMetaagentThread_AutoTs(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/auto"}
	// Ts zero → auto-set to now.
	require.NoError(t, metaagentthread.Append(ctx, m, sc, metaagentthread.Content{
		Role: metaagentthread.RoleUserMention,
		Body: "hello",
	}))
	msgs, err := metaagentthread.List(ctx, m, sc)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.False(t, msgs[0].Ts.IsZero())
}
