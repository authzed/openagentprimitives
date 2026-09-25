package lineage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lineage"
)

func newMem(t *testing.T) memory.Memory {
	t.Helper()
	return memory.NewLocal(inmem.NewBackend())
}

func TestRecordFork_WritesBothDirections(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)

	parent := memory.Scope{Kind: "AgentSession", ID: "ns/parent-uid"}
	child := memory.Scope{Kind: "AgentSession", ID: "ns/child-uid"}

	err := lineage.RecordFork(ctx, mem, parent, "parent-name", child, "child-name", 5, lineage.ReasonRestart)
	require.NoError(t, err)

	out, err := lineage.OutEdges(ctx, mem, parent)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "out", out[0].Direction)
	assert.Equal(t, "child-name", out[0].Peer)
	assert.Equal(t, 5, out[0].AtTurn)

	in, err := lineage.InEdges(ctx, mem, child)
	require.NoError(t, err)
	require.Len(t, in, 1)
	assert.Equal(t, "in", in[0].Direction)
	assert.Equal(t, "parent-name", in[0].Peer)
	assert.Equal(t, 5, in[0].AtTurn)
}

func TestRecordFork_Idempotent(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)

	parent := memory.Scope{Kind: "AgentSession", ID: "ns/p"}
	child := memory.Scope{Kind: "AgentSession", ID: "ns/c"}

	for i := 0; i < 3; i++ {
		require.NoError(t, lineage.RecordFork(ctx, mem, parent, "p", child, "c", 7, lineage.ReasonRestart))
	}
	out, err := lineage.OutEdges(ctx, mem, parent)
	require.NoError(t, err)
	assert.Len(t, out, 1, "duplicate fork records should not accumulate")
}

func TestOutEdges_SortedByPeer(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	parent := memory.Scope{Kind: "AgentSession", ID: "ns/p"}

	for _, name := range []string{"c3", "c1", "c2"} {
		child := memory.Scope{Kind: "AgentSession", ID: "ns/" + name}
		require.NoError(t, lineage.RecordFork(ctx, mem, parent, "p", child, name, 1, lineage.ReasonRestart))
	}
	out, err := lineage.OutEdges(ctx, mem, parent)
	require.NoError(t, err)
	require.Len(t, out, 3)
	assert.Equal(t, "c1", out[0].Peer)
	assert.Equal(t, "c2", out[1].Peer)
	assert.Equal(t, "c3", out[2].Peer)
}
