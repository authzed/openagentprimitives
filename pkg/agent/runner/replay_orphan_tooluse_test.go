package runner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// orphanToolUseIDs is an INDEPENDENT oracle for the well-formedness rule every
// provider adapter's input must satisfy: each `tool_use` block must be answered
// by a `tool_result` carrying the same ID in the IMMEDIATELY following user
// message. It is written here rather than reused from loop_replay.go on purpose — a
// test that asked the repair pass whether the repair pass was correct would
// prove nothing.
//
// Anthropic rejects the request with "tool_use ids were found without
// tool_result blocks immediately after"; the OpenAI-compatible adapter emits an
// assistant `tool_calls` entry with no following `tool` message, which that API
// rejects in turn. Either way the turn cannot be sent.
func orphanToolUseIDs(msgs []llm.Message) []string {
	var orphans []string
	for i, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		answered := map[string]bool{}
		if i+1 < len(msgs) && msgs[i+1].Role == "user" {
			for _, b := range msgs[i+1].Content {
				if b.Type == "tool_result" && b.ToolResult != nil {
					answered[b.ToolResult.ToolUseID] = true
				}
			}
		}
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ToolUse != nil && !answered[b.ToolUse.ID] {
				orphans = append(orphans, b.ToolUse.ID)
			}
		}
	}
	return orphans
}

// assertNoOrphanToolUses fails with the offending IDs and a rendering of the
// reconstructed conversation, so a failure reads as "the orphan survived
// replay" rather than a bare boolean.
func assertNoOrphanToolUses(t *testing.T, msgs []llm.Message) {
	t.Helper()
	orphans := orphanToolUseIDs(msgs)
	assert.Emptyf(t, orphans, "replay left %d tool_use block(s) unanswered: %s\nreconstructed conversation:\n%s",
		len(orphans), strings.Join(orphans, ","), renderMsgs(msgs))
}

func renderMsgs(msgs []llm.Message) string {
	var sb strings.Builder
	for i, m := range msgs {
		kinds := make([]string, 0, len(m.Content))
		for _, b := range m.Content {
			switch {
			case b.Type == "tool_use" && b.ToolUse != nil:
				kinds = append(kinds, "tool_use("+b.ToolUse.ID+")")
			case b.Type == "tool_result" && b.ToolResult != nil:
				kinds = append(kinds, fmt.Sprintf("tool_result(%s,isError=%v)", b.ToolResult.ToolUseID, b.ToolResult.IsError))
			default:
				kinds = append(kinds, b.Type)
			}
		}
		fmt.Fprintf(&sb, "  [%d] %s: %s\n", i, m.Role, strings.Join(kinds, " "))
	}
	return sb.String()
}

// toolResultFor returns the tool_result block answering id anywhere in msgs.
func toolResultFor(msgs []llm.Message, id string) *llm.ToolResultBlock {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolResult != nil && b.ToolResult.ToolUseID == id {
				return b.ToolResult
			}
		}
	}
	return nil
}

func userTurn(idx int, text string) memory.Turn {
	return memory.Turn{Index: idx, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: text}}}
}

func assistantToolUseTurn(idx int, ids ...string) memory.Turn {
	blocks := make([]memory.ContentBlock, 0, len(ids))
	for _, id := range ids {
		blocks = append(blocks, memory.ContentBlock{
			Type:    "tool_use",
			ToolUse: &memory.ToolUseBlock{ID: id, Name: "some_tool", Input: []byte(`{}`)},
		})
	}
	return memory.Turn{Index: idx, Role: "assistant", Content: blocks}
}

func toolResultTurn(idx int, ids ...string) memory.Turn {
	blocks := make([]memory.ContentBlock, 0, len(ids))
	for _, id := range ids {
		blocks = append(blocks, memory.ContentBlock{
			Type:       "tool_result",
			ToolResult: &memory.ToolResultBlock{ToolUseID: id, Content: "ok"},
		})
	}
	return memory.Turn{Index: idx, Role: "user", Content: blocks}
}

// TestReplay_OrphanToolUseIsAnswered covers every transcript shape an
// ungraceful runner death can leave behind. The assistant turn carrying
// tool_use blocks is appended to durable memory BEFORE dispatch and the paired
// tool_result turn only after, so an OOM, eviction, node loss, or a restart
// during a minutes-long approval Await persists an assistant turn nothing ever
// answered. replay must not hand that to a provider: every reconstructed
// tool_use needs a tool_result, or the session is permanently unresponsive —
// the retry replays the identical orphan and channelsd keeps routing new
// messages to the dead ToolCall.
func TestReplay_OrphanToolUseIsAnswered(t *testing.T) {
	cases := []struct {
		name  string
		prior []memory.Turn
		check func(t *testing.T, msgs []llm.Message)
	}{
		{
			name: "crash before the result turn: trailing orphan gets a synthesized error result",
			prior: []memory.Turn{
				userTurn(0, "run the deploy"),
				assistantToolUseTurn(1, "toolu_crash"),
			},
			check: func(t *testing.T, msgs []llm.Message) {
				tr := toolResultFor(msgs, "toolu_crash")
				require.NotNil(t, tr, "the orphaned tool_use must be answered")
				assert.True(t, tr.IsError, "an unknown-outcome tool call is an error result, so the model treats it as a step to redo rather than a success")
				assert.Equal(t, "user", msgs[len(msgs)-1].Role, "the request must still end on a user turn")
			},
		},
		{
			name: "orphan followed by a drained inbound turn: result lands ahead of the new user text",
			prior: []memory.Turn{
				userTurn(0, "run the deploy"),
				assistantToolUseTurn(1, "toolu_drain"),
				userTurn(2, "any update?"),
			},
			check: func(t *testing.T, msgs []llm.Message) {
				require.Len(t, msgs, 3, "the drained user turn absorbs the synthesized result rather than a fourth message appearing")
				require.Equal(t, "user", msgs[2].Role)
				require.NotEmpty(t, msgs[2].Content)
				require.Equal(t, "tool_result", msgs[2].Content[0].Type,
					"tool_result blocks must lead the user turn that answers the tool_use")
				joined := ""
				for _, b := range msgs[2].Content {
					joined += b.Text
				}
				assert.Contains(t, joined, "any update?", "the inbound message must survive the repair")
			},
		},
		{
			name: "respond_to_user already delivered: result says so and is not an error",
			prior: []memory.Turn{
				userTurn(0, "say hi"),
				memory.Turn{Index: 1, Role: "assistant", Content: []memory.ContentBlock{
					{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "toolu_sent", Name: "respond_to_user", Input: []byte(`{"text":"hi"}`)}},
				}},
				memory.Turn{Index: 2, Role: "system_note", Content: []memory.ContentBlock{
					{Type: "text", Text: `{"delivered":["toolu_sent"]}`},
				}},
			},
			check: func(t *testing.T, msgs []llm.Message) {
				tr := toolResultFor(msgs, "toolu_sent")
				require.NotNil(t, tr, "the orphaned tool_use must be answered")
				assert.False(t, tr.IsError, "the reply reached the user before the crash, so this is not a failed step")
				assert.Contains(t, strings.ToLower(tr.Content), "delivered",
					"the model must be told the reply landed, or it re-sends and the user sees it twice")
			},
		},
		{
			name: "partially answered turn: only the unanswered tool_use is synthesized",
			prior: []memory.Turn{
				userTurn(0, "do both"),
				assistantToolUseTurn(1, "toolu_done", "toolu_lost"),
				toolResultTurn(2, "toolu_done"),
			},
			check: func(t *testing.T, msgs []llm.Message) {
				done := toolResultFor(msgs, "toolu_done")
				require.NotNil(t, done)
				assert.Equal(t, "ok", done.Content, "an existing tool_result must not be rewritten")
				assert.False(t, done.IsError)
				lost := toolResultFor(msgs, "toolu_lost")
				require.NotNil(t, lost, "the unanswered tool_use must be answered")
				assert.True(t, lost.IsError)
			},
		},
		{
			name: "fully answered turn: replay is unchanged",
			prior: []memory.Turn{
				userTurn(0, "do it"),
				assistantToolUseTurn(1, "toolu_ok"),
				toolResultTurn(2, "toolu_ok"),
			},
			check: func(t *testing.T, msgs []llm.Message) {
				require.Len(t, msgs, 3, "a well-formed transcript must not grow a synthesized turn")
				require.Len(t, msgs[2].Content, 1)
				tr := toolResultFor(msgs, "toolu_ok")
				require.NotNil(t, tr)
				assert.Equal(t, "ok", tr.Content)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, _, _ := replay(tc.prior, "ns/orphan-session")
			assertNoOrphanToolUses(t, msgs)
			tc.check(t, msgs)
		})
	}
}

// TestReplay_OrphanRepairIsIdempotent proves the repair does not compound: the
// synthesized blocks are transient (never appended to the append-only,
// per-turn-signed transcript), so the SAME durable turns replayed again — the
// crash-loop case, where RestartPolicy: OnFailure re-enters replay on the same
// scope — must produce the same conversation, not a second synthesized result.
func TestReplay_OrphanRepairIsIdempotent(t *testing.T) {
	prior := []memory.Turn{
		userTurn(0, "run the deploy"),
		assistantToolUseTurn(1, "toolu_loop"),
	}
	first, firstNext, _ := replay(prior, "ns/orphan-session")
	second, secondNext, _ := replay(prior, "ns/orphan-session")

	assert.Equal(t, len(first), len(second), "replaying the same turns twice must not accumulate messages")
	assert.Equal(t, firstNext, secondNext, "the synthesized turn is transient and must not advance nextIndex")
	assert.Equal(t, 2, firstNext, "nextIndex tracks the durable turns only")
	assertNoOrphanToolUses(t, second)
}
