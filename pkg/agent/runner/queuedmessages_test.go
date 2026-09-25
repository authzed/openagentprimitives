package runner

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeldInbox_ReturnsUndrainedInboxTurnsSorted(t *testing.T) {
	l := newDrainLoop(t) // from drain_inbox_test.go: *Loop with inmem Memory
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Two undrained inbox turns, out of Index order.
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(5, "inbox", "second")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(3, "inbox", "first")))
	// One already-drained inbox turn (paired inbox_done marker at same Index).
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "inbox", "done-already")))
	require.NoError(t, l.Memory.Append(ctx, memory.Turn{Index: 1, Role: "inbox_done"}))

	held, err := l.heldInbox(ctx)
	require.NoError(t, err)
	require.Len(t, held, 2, "only the two undrained inbox turns")
	assert.Equal(t, 3, held[0].Index, "sorted ascending by Index")
	assert.Equal(t, 5, held[1].Index)
	assert.Equal(t, "first", firstText(t, held[0]))
	assert.Equal(t, "second", firstText(t, held[1]))
}

func TestResultsIncludeAwaitResume(t *testing.T) {
	assert.False(t, resultsIncludeAwaitResume(nil), "no results → not a resume")
	assert.False(t, resultsIncludeAwaitResume([]tool.Result{{Content: "work"}}),
		"ordinary tool results → not a resume")
	assert.True(t, resultsIncludeAwaitResume([]tool.Result{{Content: "x"}, {AwaitResumed: true}}),
		"any AwaitResumed result → resume")
}
