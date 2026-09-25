package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// textBlock and useBlock build the assistant-turn content blocks the loop
// hands to needsDeliveryNudge, so each case reads as the turn the model
// actually emitted.
func textBlock(s string) llm.ContentBlock {
	return llm.ContentBlock{Type: "text", Text: s}
}

func useBlock(name string) llm.ContentBlock {
	return llm.ContentBlock{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "toolu_" + name, Name: name}}
}

// TestNeedsDeliveryNudge covers the guard that catches a channel-attached
// round ending with the model's answer stranded in assistant prose. Both
// "nudge" cases are verbatim reproductions of live pirate-agent sessions:
// the model wrote its reply as a bare text block and reached straight for a
// round-terminating tool, so the channel received nothing while the system
// recorded success.
func TestNeedsDeliveryNudge(t *testing.T) {
	cases := []struct {
		name            string
		channelAttached bool
		noRespondTool   bool // respond_to_user absent from the session's tool list
		delivered       bool
		blocks          []llm.ContentBlock
		toolName        string
		want            bool
	}{
		{
			name:            "prose + agent_work_complete, nothing delivered: nudge",
			channelAttached: true,
			blocks: []llm.ContentBlock{
				textBlock("Arrr, matey! I be but a humble pirate-speak translator, savvy?"),
				useBlock("agent_work_complete"),
			},
			toolName: "agent_work_complete",
			want:     true,
		},
		{
			// Without this, the guard would demand a call the model cannot
			// make and block every terminal attempt until the turn budget
			// died — a hard wedge, strictly worse than the silence the guard
			// exists to prevent. Never ask for something unavailable.
			name:            "respond_to_user not in the tool list: no nudge, never wedge",
			channelAttached: true,
			noRespondTool:   true,
			blocks: []llm.ContentBlock{
				textBlock("Arrr, matey!"),
				useBlock("agent_work_complete"),
			},
			toolName: "agent_work_complete",
			want:     false,
		},
		{
			name:            "prose + await_user_message, nothing delivered: nudge",
			channelAttached: true,
			blocks: []llm.ContentBlock{
				textBlock("Arr, that be a fine question ye pose, matey!"),
				useBlock("await_user_message"),
			},
			toolName: "await_user_message",
			want:     true,
		},
		{
			name:            "respond_to_user in the same turn: no nudge",
			channelAttached: true,
			blocks: []llm.ContentBlock{
				textBlock("Arrr, matey!"),
				useBlock("respond_to_user"),
				useBlock("agent_work_complete"),
			},
			toolName: "agent_work_complete",
			want:     false,
		},
		{
			name:            "already delivered earlier this round: no nudge",
			channelAttached: true,
			delivered:       true,
			blocks: []llm.ContentBlock{
				textBlock("Anything else ye be needin'?"),
				useBlock("agent_work_complete"),
			},
			toolName: "agent_work_complete",
			want:     false,
		},
		{
			name:            "no prose to strand: no nudge",
			channelAttached: true,
			blocks:          []llm.ContentBlock{useBlock("agent_work_complete")},
			toolName:        "agent_work_complete",
			want:            false,
		},
		{
			name:            "whitespace-only prose is not an answer: no nudge",
			channelAttached: true,
			blocks: []llm.ContentBlock{
				textBlock("   \n  "),
				useBlock("agent_work_complete"),
			},
			toolName: "agent_work_complete",
			want:     false,
		},
		{
			name:            "kubectl-driven session: no nudge",
			channelAttached: false,
			blocks: []llm.ContentBlock{
				textBlock("Here is the report."),
				useBlock("agent_work_complete"),
			},
			toolName: "agent_work_complete",
			want:     false,
		},
		{
			name:            "non-terminal tool: no nudge",
			channelAttached: true,
			blocks: []llm.ContentBlock{
				textBlock("Translating the message into pirate dialect..."),
				useBlock("update_status"),
			},
			toolName: "update_status",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := needsDeliveryNudge(tc.channelAttached, !tc.noRespondTool, tc.delivered, tc.blocks, tc.toolName)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDispatch_DeliveryGuard_BlocksTerminalWithNothingDelivered proves the
// guard is actually WIRED into dispatch, not merely implemented. The unit
// test above covers the predicate; this covers the join — that a blocked
// terminal tool's Execute never runs.
//
// Executing and correcting afterwards would be useless: await_user_message
// blocks and agent_work_complete submits the terminal result, so by the time
// a post-hoc check noticed, the round would already be over.
func TestDispatch_DeliveryGuard_BlocksTerminalWithNothingDelivered(t *testing.T) {
	for _, toolName := range []string{"agent_work_complete", "await_user_message"} {
		t.Run(toolName+": not executed, IsError returned", func(t *testing.T) {
			term := &fakeDispatchTool{name: toolName, kind: tool.KindMeta, result: tool.Result{Content: "done", Terminal: true}}
			// respond_to_user is in the tool list, as it is in every real
			// channel-attached session — the guard only fires when the model
			// actually has the call it is being told to make.
			respond := &fakeDispatchTool{name: "respond_to_user", kind: tool.KindMeta, result: tool.Result{Content: "sent"}}
			l := &Loop{
				Tools:           []tool.Tool{term, respond},
				ChannelAttached: true,
				SessionKey:      memory.NamespacedName{Namespace: "default", Name: "disp"},
			}
			// The assistant turn the live pirate sessions actually emitted:
			// the reply stranded in prose, then straight to a terminal tool.
			blocks := []llm.ContentBlock{
				textBlock("Arrr, matey! I be but a humble pirate-speak translator."),
				useBlock(toolName),
			}
			uses := []llm.ToolUseBlock{{ID: "tu-1", Name: toolName, Input: json.RawMessage(`{"summary":"done"}`)}}

			results, anyTerm := l.dispatchToolUses(
				memory.WithSystemApproval(context.Background(), "test"), uses, dispatchTestSession(), 0, 0, nil, blocks)

			require.Len(t, results, 1)
			assert.Equal(t, 0, term.executed(), "the terminal tool must NOT run — once it does, the round is over")
			assert.True(t, results[0].IsError, "the model must get a correctable error, not a success")
			assert.False(t, anyTerm, "a blocked call must not terminate the round")
			assert.Contains(t, results[0].Content, "respond_to_user",
				"the nudge must name the tool the model has to call")
		})
	}
}

// TestDispatch_DeliveryGuard_AllowsTerminalAfterRespond is the other half of
// the contract: once respond_to_user has delivered, terminal calls must pass.
// A guard that blocked here would wedge every session permanently, which is a
// far worse failure than the silence it exists to prevent.
func TestDispatch_DeliveryGuard_AllowsTerminalAfterRespond(t *testing.T) {
	respond := &fakeDispatchTool{name: "respond_to_user", kind: tool.KindMeta, result: tool.Result{Content: "sent"}}
	term := &fakeDispatchTool{name: "agent_work_complete", kind: tool.KindMeta, result: tool.Result{Content: "done", Terminal: true}}
	l := &Loop{
		Tools:           []tool.Tool{respond, term},
		ChannelAttached: true,
		SessionKey:      memory.NamespacedName{Namespace: "default", Name: "disp"},
	}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Round 1, turn A: deliver.
	blocksA := []llm.ContentBlock{useBlock("respond_to_user")}
	usesA := []llm.ToolUseBlock{{ID: "tu-1", Name: "respond_to_user", Input: json.RawMessage(`{"text":"Arrr!"}`)}}
	resA, _ := l.dispatchToolUses(ctx, usesA, dispatchTestSession(), 0, 0, nil, blocksA)
	require.Len(t, resA, 1)
	require.False(t, resA[0].IsError, "respond_to_user itself must never be blocked")
	require.Equal(t, 1, respond.executed())

	// Round 1, turn B: terminate. Delivery already happened, so this proceeds
	// even though the turn carries prose and no respond_to_user of its own.
	blocksB := []llm.ContentBlock{textBlock("Anything else, matey?"), useBlock("agent_work_complete")}
	usesB := []llm.ToolUseBlock{{ID: "tu-2", Name: "agent_work_complete", Input: json.RawMessage(`{"summary":"done"}`)}}
	resB, anyTerm := l.dispatchToolUses(ctx, usesB, dispatchTestSession(), 1, 1, nil, blocksB)
	require.Len(t, resB, 1)
	assert.Equal(t, 1, term.executed(), "a delivered round must be allowed to end")
	assert.False(t, resB[0].IsError)
	assert.True(t, anyTerm)
}

// TestDeliveryGuard_ReArmsOnNewRound covers the round boundary. Without the
// reset, one reply early in a long conversation would excuse every later
// round from answering — the guard would protect the first message of a
// thread and nothing after it.
func TestDeliveryGuard_ReArmsOnNewRound(t *testing.T) {
	l := &Loop{ChannelAttached: true}
	l.markDelivered()
	require.True(t, l.hasDeliveredThisRound(), "precondition: delivery recorded")

	l.beginDeliveryRound()
	assert.False(t, l.hasDeliveredThisRound(),
		"a new inbound user message must re-arm the guard for the new round")
}

// TestDispatch_DeliveryGuard_ResumeDedupCountsAsDelivered covers the restart
// path. When the runner dies mid-turn and resumes, an already-published
// respond_to_user is short-circuited by the resume dedup instead of being
// re-executed. That skip must still count as this round's delivery: otherwise
// the guard sees a silent round and makes the model re-send a message the user
// already received — turning a crash into a duplicate post.
func TestDispatch_DeliveryGuard_ResumeDedupCountsAsDelivered(t *testing.T) {
	respond := &fakeDispatchTool{name: "respond_to_user", kind: tool.KindMeta, result: tool.Result{Content: "sent"}}
	term := &fakeDispatchTool{name: "agent_work_complete", kind: tool.KindMeta, result: tool.Result{Content: "done", Terminal: true}}
	l := &Loop{
		Tools:           []tool.Tool{respond, term},
		ChannelAttached: true,
		SessionKey:      memory.NamespacedName{Namespace: "default", Name: "disp"},
	}

	// The turn as replayed after a restart: respond_to_user already went out
	// (so it is in the delivered set), agent_work_complete never ran.
	blocks := []llm.ContentBlock{
		textBlock("Arrr, matey!"),
		useBlock("respond_to_user"),
		useBlock("agent_work_complete"),
	}
	uses := []llm.ToolUseBlock{
		{ID: "tu-1", Name: "respond_to_user", Input: json.RawMessage(`{"text":"Arrr!"}`)},
		{ID: "tu-2", Name: "agent_work_complete", Input: json.RawMessage(`{"summary":"done"}`)},
	}

	results, _ := l.dispatchToolUses(memory.WithSystemApproval(context.Background(), "test"),
		uses, dispatchTestSession(), 0, 0, map[string]bool{"tu-1": true}, blocks)

	require.Len(t, results, 2)
	assert.Equal(t, 0, respond.executed(), "the deduped respond_to_user must not re-publish")
	assert.True(t, l.hasDeliveredThisRound(),
		"a deduped respond_to_user still counts as delivered — it reached the user before the restart")
	assert.Equal(t, 1, term.executed(), "the round must be allowed to end")
	assert.False(t, results[1].IsError)
}
