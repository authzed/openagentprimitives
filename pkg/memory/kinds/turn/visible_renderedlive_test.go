package turn_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// textTurn, respondTurn and texts are defined alongside the other visible-timeline
// tests in the same test package.

// toolResultTurn builds the tool-result RELAY turn the runner writes after a
// tool batch: role "user" (where the providers require tool results to sit),
// authored by no human, every block a tool_result.
func toolResultTurn(index int, blocks ...memory.ContentBlock) memory.Turn {
	return memory.Turn{
		Index:     index,
		Role:      "user",
		Content:   blocks,
		CreatedAt: time.Unix(int64(index), 0).UTC(),
	}
}

// enveloped wraps content the way the runner does before persisting it, so the
// fixtures are the real stored shape rather than a convenient bare string.
func enveloped(s string) string { return toolenvelope.Wrap(s, "cafebabe0badf00d") }

// resultBlock is one stored tool_result block. renderedLive says whether the
// live surfaces already showed this content to the user.
func resultBlock(id, content string, renderedLive, isErr bool) memory.ContentBlock {
	return memory.ContentBlock{Type: "tool_result", ToolResult: &memory.ToolResultBlock{
		ToolUseID: id, Content: enveloped(content), IsError: isErr, RenderedLive: renderedLive,
	}}
}

// TestVisibleTimelineRenderedLiveToolResult pins BOTH directions of the rule,
// because either one alone is a bug a user has hit:
//
//   - A streaming sub-agent toolkit's report is composed FOR A HUMAN and was on
//     screen the whole time it streamed. Dropping it on reload is the reported
//     confusion — the work is visibly gone with no trace it happened.
//   - An ordinary tool result (a memory query's rows, a kubectl dump) is
//     plumbing the live surfaces never rendered. Showing it on reload would
//     bury the conversation in machine output the user never saw.
//
// The fixtures deliberately MIX shown and ordinary blocks inside one relay turn:
// the rule is per-block, and a per-turn implementation would pass a test that
// only ever put one kind of block in a turn.
func TestVisibleTimelineRenderedLiveToolResult(t *testing.T) {
	const report = "status: success (7.3s, $0.09)\nI reviewed the README and found no typos."
	cases := []struct {
		name  string
		turns []memory.Turn
		want  []string
	}{
		{
			name: "result the live surfaces showed: rendered, as an AGENT message",
			turns: []memory.Turn{
				textTurn(0, "user", "review the README"),
				toolResultTurn(1, resultBlock("tu_1", report, true, false)),
			},
			want: []string{"user:review the README", "agent:" + report},
		},
		{
			name: "ordinary tool result: still hidden",
			turns: []memory.Turn{
				textTurn(0, "user", "what do you remember?"),
				toolResultTurn(1, resultBlock("tu_1", "id,tags\n1,foo\n2,bar", false, false)),
			},
			want: []string{"user:what do you remember?"},
		},
		{
			name: "failed streamed run: rendered too — live kept it EXPANDED",
			turns: []memory.Turn{
				toolResultTurn(0, resultBlock("tu_1", "status: failed (exit 2)\ncould not clone", true, true)),
			},
			want: []string{"agent:status: failed (exit 2)\ncould not clone"},
		},
		{
			name: "one relay turn, mixed blocks: only the shown one contributes",
			turns: []memory.Turn{
				toolResultTurn(0,
					resultBlock("tu_1", "id,tags\n1,foo", false, false),
					resultBlock("tu_2", report, true, false),
					resultBlock("tu_3", "namespace demo-ns deleted", false, false),
				),
			},
			want: []string{"agent:" + report},
		},
		{
			name: "two shown results in one batch join with a blank line",
			turns: []memory.Turn{
				toolResultTurn(0,
					resultBlock("tu_1", "first run", true, false),
					resultBlock("tu_2", "second run", true, false),
				),
			},
			want: []string{"agent:first run\n\nsecond run"},
		},
		{
			name: "shown but empty payload: contributes nothing, not a blank bubble",
			turns: []memory.Turn{
				textTurn(0, "user", "go"),
				toolResultTurn(1, resultBlock("tu_1", "   ", true, false)),
			},
			want: []string{"user:go"},
		},
		{
			name: "an ordinary conversation is untouched by the rule",
			turns: []memory.Turn{
				textTurn(0, "user", "hello"),
				respondTurn(1, "hi there"),
			},
			want: []string{"user:hello", "agent:hi there"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, texts(turn.VisibleMessages(tc.turns)))
		})
	}
}

// TestVisibleTimelineRenderedLiveStripsEnvelope proves the untrusted-output
// envelope the MODEL sees never reaches the human. Asserted apart from the
// table because the failure it guards is a rendering leak — the raw tag and its
// nonce inside a chat bubble — not a wrong-message-count bug.
func TestVisibleTimelineRenderedLiveStripsEnvelope(t *testing.T) {
	msgs := turn.VisibleMessages([]memory.Turn{
		toolResultTurn(0, resultBlock("tu_1", "the sub-agent's report", true, false)),
	})
	require.Len(t, msgs, 1)
	assert.Equal(t, "the sub-agent's report", msgs[0].Text)
	assert.NotContains(t, msgs[0].Text, toolenvelope.Tag)
	assert.NotContains(t, msgs[0].Text, "nonce")
}

// TestVisibleTimelineRenderedLiveUnenveloped covers content stored WITHOUT the
// envelope. toolenvelope.Unwrap reports "not enveloped" rather than failing, and
// the payload must pass through whole rather than being mangled or dropped.
func TestVisibleTimelineRenderedLiveUnenveloped(t *testing.T) {
	msgs := turn.VisibleMessages([]memory.Turn{{
		Index: 0, Role: "user",
		Content: []memory.ContentBlock{{Type: "tool_result", ToolResult: &memory.ToolResultBlock{
			ToolUseID: "tu_1", Content: "bare payload, no envelope", RenderedLive: true,
		}}},
		CreatedAt: time.Unix(0, 0).UTC(),
	}})
	require.Len(t, msgs, 1)
	assert.Equal(t, "bare payload, no envelope", msgs[0].Text)
}
