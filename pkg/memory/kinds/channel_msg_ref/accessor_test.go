package channel_msg_ref_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/channel_msg_ref"
)

func newMem(t *testing.T) memory.Memory {
	t.Helper()
	return memory.NewLocal(inmem.NewBackend())
}

func TestRecord_RoundTrip(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	scope := memory.Scope{Kind: "AgentSession", ID: "ns/sess"}

	err := channel_msg_ref.Record(ctx, mem, scope, "slack", "C1:1.0:1.5", 7)
	require.NoError(t, err)

	idx, ok, err := channel_msg_ref.Lookup(ctx, mem, scope, "slack", "C1:1.0:1.5")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 7, idx)
}

func TestLookup_NotFound(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	scope := memory.Scope{Kind: "AgentSession", ID: "ns/sess"}

	_, ok, err := channel_msg_ref.Lookup(ctx, mem, scope, "slack", "missing")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestRecord_Idempotent(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	scope := memory.Scope{Kind: "AgentSession", ID: "ns/sess"}

	for i := 0; i < 3; i++ {
		require.NoError(t, channel_msg_ref.Record(ctx, mem, scope, "slack", "C1:1.0:1.5", 7))
	}
	idx, ok, err := channel_msg_ref.Lookup(ctx, mem, scope, "slack", "C1:1.0:1.5")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 7, idx)
}

func TestRecord_DifferentRefs_BothPersist(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	scope := memory.Scope{Kind: "AgentSession", ID: "ns/sess"}

	require.NoError(t, channel_msg_ref.Record(ctx, mem, scope, "slack", "ref-a", 1))
	require.NoError(t, channel_msg_ref.Record(ctx, mem, scope, "slack", "ref-b", 2))

	a, ok, err := channel_msg_ref.Lookup(ctx, mem, scope, "slack", "ref-a")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, a)

	b, ok, err := channel_msg_ref.Lookup(ctx, mem, scope, "slack", "ref-b")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, b)
}
