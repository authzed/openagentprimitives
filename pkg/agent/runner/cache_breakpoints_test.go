package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// msg builds a message with n text blocks, so tests can express block counts.
func msg(t *testing.T, role string, n int) llm.Message {
	t.Helper()
	blocks := make([]llm.ContentBlock, n)
	for i := range blocks {
		blocks[i] = llm.ContentBlock{Type: "text", Text: "x"}
	}
	return llm.Message{Role: role, Content: blocks}
}

// marked returns the (messageIndex, blockIndex) of every Cacheable block.
func marked(msgs []llm.Message) [][2]int {
	var out [][2]int
	for i, m := range msgs {
		for j, cb := range m.Content {
			if cb.Cacheable {
				out = append(out, [2]int{i, j})
			}
		}
	}
	return out
}

func TestMarkCacheBreakpoints_FirstRequest_MovingOnly(t *testing.T) {
	l := &Loop{}
	msgs := []llm.Message{msg(t, "user", 1)}

	l.markCacheBreakpoints(msgs)

	assert.Equal(t, [][2]int{{0, 0}}, marked(msgs),
		"first request has no previous position, so only the moving breakpoint is set")
}

func TestMarkCacheBreakpoints_SteadyState_AnchorAtPreviousPosition(t *testing.T) {
	l := &Loop{}
	first := []llm.Message{msg(t, "user", 1)}
	l.markCacheBreakpoints(first)
	require.Equal(t, 0, l.prevBreakpointMsg, "first request records index 0")

	// Next request: the loop appended an assistant turn and its tool results.
	second := []llm.Message{msg(t, "user", 1), msg(t, "assistant", 2), msg(t, "user", 3)}
	l.markCacheBreakpoints(second)

	assert.Equal(t, [][2]int{{0, 0}, {2, 2}}, marked(second),
		"anchor sits at the previous request's position (msg 0), moving at the new end")
	assert.Equal(t, 2, l.prevBreakpointMsg, "the moving position becomes the next anchor")
}

func TestMarkCacheBreakpoints_NeverExceedsTwo(t *testing.T) {
	l := &Loop{}
	msgs := []llm.Message{msg(t, "user", 1), msg(t, "assistant", 1), msg(t, "user", 1)}
	l.markCacheBreakpoints(msgs)
	l.markCacheBreakpoints(msgs)
	l.markCacheBreakpoints(msgs)

	assert.LessOrEqual(t, len(marked(msgs)), 2,
		"repeated marking must not accumulate breakpoints past the 2 free slots")
}

func TestMarkCacheBreakpoints_HistoryShrank_AnchorDropped(t *testing.T) {
	l := &Loop{prevBreakpointMsg: 9, hasPrevBreakpoint: true}
	msgs := []llm.Message{msg(t, "user", 1), msg(t, "assistant", 1)}

	l.markCacheBreakpoints(msgs)

	assert.Equal(t, [][2]int{{1, 0}}, marked(msgs),
		"a stale index past the end (compaction, fork, replay) drops the anchor rather than panicking")
}

func TestMarkCacheBreakpoints_EmptyAndBlanks_NoPanic(t *testing.T) {
	l := &Loop{}
	l.markCacheBreakpoints(nil)

	msgs := []llm.Message{{Role: "user"}} // message with no content blocks
	l.markCacheBreakpoints(msgs)

	assert.Empty(t, marked(msgs), "a message with no content blocks has nothing to mark")
}

// TestMarkCacheBreakpoints_GrowingHistory_StaysAtTwo exercises the shape
// production actually uses: ONE slice that grows by two messages per turn
// (the loop calls markCacheBreakpoints on l's live messages slice, then
// appends the next assistant/user pair before the following turn) — unlike
// TestMarkCacheBreakpoints_NeverExceedsTwo, which re-marks a fixed-length
// slice and so never exercises the clear-then-remark path at all. Without
// the clear loop at the top of markCacheBreakpoints, marks accumulate
// linearly across turns and blow through Anthropic's 4-breakpoint budget.
func TestMarkCacheBreakpoints_GrowingHistory_StaysAtTwo(t *testing.T) {
	l := &Loop{}
	msgs := []llm.Message{msg(t, "user", 1)}
	for turn := 0; turn < 5; turn++ {
		l.markCacheBreakpoints(msgs)
		require.LessOrEqual(t, len(marked(msgs)), 2, "turn %d accumulated marks", turn)
		msgs = append(msgs, msg(t, "assistant", 2), msg(t, "user", 3))
	}
	l.markCacheBreakpoints(msgs)
	assert.Equal(t, [][2]int{{8, 2}, {10, 2}},
		marked(msgs), "steady state is exactly the anchor plus the moving breakpoint")
}
