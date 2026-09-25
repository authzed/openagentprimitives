package tool_dispatch_snapshot_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

func newMem(t *testing.T) memory.Memory {
	t.Helper()
	return memory.NewLocal(inmem.NewBackend())
}

func TestRecord_RoundTrip(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	scope := memory.Scope{Kind: "AgentSession", ID: "ns/sess"}

	rec := tool_dispatch_snapshot.Content{
		ToolUseID:       "tool_use_abc",
		SpiceboxSession: "sess-bundle-a",
		TurnIndex:       3,
		Sequence:        0,
		SessionUID:      "uid-1",
	}
	require.NoError(t, tool_dispatch_snapshot.Record(ctx, mem, scope, rec))

	all, err := tool_dispatch_snapshot.ReadAll(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, rec, all[0])
}

func TestForTurnRange_FiltersAndSorts(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMem(t)
	scope := memory.Scope{Kind: "AgentSession", ID: "ns/sess"}

	cases := []tool_dispatch_snapshot.Content{
		{ToolUseID: "u1", SpiceboxSession: "b", TurnIndex: 1, Sequence: 0, SessionUID: "uid"},
		{ToolUseID: "u2", SpiceboxSession: "b", TurnIndex: 3, Sequence: 1, SessionUID: "uid"},
		{ToolUseID: "u3", SpiceboxSession: "b", TurnIndex: 3, Sequence: 0, SessionUID: "uid"},
		{ToolUseID: "u4", SpiceboxSession: "b", TurnIndex: 5, Sequence: 0, SessionUID: "uid"},
	}
	for _, c := range cases {
		require.NoError(t, tool_dispatch_snapshot.Record(ctx, mem, scope, c))
	}

	afterTurn2, err := tool_dispatch_snapshot.ForTurnRange(ctx, mem, scope, 2, 100)
	require.NoError(t, err)
	require.Len(t, afterTurn2, 3, "expect turns 3,3,5 (turn 1 excluded)")
	assert.Equal(t, 3, afterTurn2[0].TurnIndex)
	assert.Equal(t, 0, afterTurn2[0].Sequence)
	assert.Equal(t, 3, afterTurn2[1].TurnIndex)
	assert.Equal(t, 1, afterTurn2[1].Sequence)
	assert.Equal(t, 5, afterTurn2[2].TurnIndex)
}

// Pin the turn import; accessor.go uses turn.EntryID for the for_turn link target.
var _ = turn.KindName
