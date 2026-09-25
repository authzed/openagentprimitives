package runner_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// reqHasTool reports whether req's tool list contains a def named `name`.
func reqHasTool(req llm.Request, name string) bool {
	for _, td := range req.Tools {
		if td.Name == name {
			return true
		}
	}
	return false
}

// countToolDef counts how many defs named `name` appear in req (a duplicate
// guard — a tool appended twice would show up twice).
func countToolDef(req llm.Request, name string) int {
	n := 0
	for _, td := range req.Tools {
		if td.Name == name {
			n++
		}
	}
	return n
}

// TestLoop_ToolRefresher_AddsToolMidSessionCallableOnce drives a three-turn
// loop. The ToolRefresher returns a brand-new tool ("sidecar_echo") on its
// SECOND consultation (modeling a secret-gated sidecar going Ready between
// turn 1 and turn 2). The test asserts:
//   - turn 1's LLM request does NOT carry the new tool def;
//   - turn 2's request DOES (the tool was injected into the live set);
//   - the LLM successfully calls the new tool on turn 2 (it is dispatchable);
//   - the refresher returning the tool only once means the def appears exactly
//     once (no duplication across the remaining turns).
func TestLoop_ToolRefresher_AddsToolMidSessionCallableOnce(t *testing.T) {
	const newToolName = "sidecar_echo"

	// The newly-ready tool. countingTool (authz_test.go) records executions.
	newTool := &countingTool{
		name: newToolName,
		perm: authz.Permission{StateImpact: authz.Passthrough},
	}

	// LLM script:
	//   turn 1 → call update_status (any pre-existing tool) — new tool not yet present
	//   turn 2 → call the newly-injected sidecar_echo
	//   turn 3 → agent_work_complete
	script := []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID: "tu_1", Name: "update_status", Input: json.RawMessage(`{"text":"working"}`),
			}}},
			StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		{Resp: llm.Response{
			Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID: "tu_2", Name: newToolName, Input: json.RawMessage(`{}`),
			}}},
			StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		{Resp: llm.Response{
			Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID: "tu_3", Name: "agent_work_complete", Input: json.RawMessage(`{"summary":"done"}`),
			}}},
			StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
	}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", Generation: 1},
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	provider := llmfake.New(script)
	key := memory.NamespacedName{Namespace: "default", Name: "s1"}
	store := runner.LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key)

	// Pre-existing tools include update_status so turn 1 has something to call.
	tools := append(meta.Load(), meta.NewUpdateStatus(meta.UpdateStatusConfig{}))

	refreshCalls := 0
	l := &runner.Loop{
		Provider:     provider,
		Memory:       store,
		Status:       runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
		Tools:        tools,
		System:       "you are a test agent",
		UserPrompt:   "do the thing",
		Budget:       runner.NewBudget(spiceboxv1alpha1.BudgetConfig{MaxTurns: 50, MaxTokens: 100000, MaxDuration: metav1.Duration{Duration: time.Hour}}, nil, time.Now()),
		Model:        "claude-test",
		MaxTokens:    1024,
		SessionKey:   key,
		ToolAuthMode: runner.ToolAuthModeDisabled,
		ToolRefresher: func(_ context.Context) (runner.ToolRefreshResult, error) {
			refreshCalls++
			// First consultation (top of turn 1): nothing ready yet.
			// Second consultation (top of turn 2): the sidecar is Ready —
			// return it exactly once. Subsequent calls return nothing (dedup
			// is the refresher's responsibility).
			if refreshCalls == 2 {
				return runner.ToolRefreshResult{Added: []tool.Tool{newTool}}, nil
			}
			return runner.ToolRefreshResult{}, nil
		},
	}

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 3, "expected at least 3 LLM requests (3 turns)")

	// Turn 1's request must NOT carry the new tool def — it wasn't ready yet.
	assert.False(t, reqHasTool(reqs[0], newToolName),
		"turn 1 request must not carry the not-yet-ready sidecar tool")

	// Turn 2's request MUST carry the new tool def — the refresher injected it.
	assert.True(t, reqHasTool(reqs[1], newToolName),
		"turn 2 request must carry the newly-ready sidecar tool")

	// The new tool was actually dispatched on turn 2 (it is callable).
	assert.Equal(t, 1, newTool.executes, "newly-injected tool must be dispatchable and called once")

	// No duplication: across every request the def appears at most once.
	for i, req := range reqs {
		assert.LessOrEqual(t, countToolDef(req, newToolName), 1,
			"tool def must never be duplicated in request %d", i)
	}
}

// TestLoop_ToolRefresher_ReplacesToolMidSession drives a three-turn loop where
// the ToolRefresher returns a tool named "sidecar_echo" on its first
// consultation (modeling a sidecar going Ready before turn 1) and then returns
// a DIFFERENT instance with the SAME name on its second consultation (modeling
// the operator replacing the sidecar pod on a token rotation). The test asserts
// REPLACE-by-name semantics:
//   - the new instance is the one dispatched on turn 2 (it replaced the old);
//   - the OLD instance is NOT executed after replacement;
//   - the def never appears more than once in any request (no duplication).
func TestLoop_ToolRefresher_ReplacesToolMidSession(t *testing.T) {
	const toolName = "sidecar_echo"

	oldTool := &countingTool{name: toolName, perm: authz.Permission{StateImpact: authz.Passthrough}}
	newTool := &countingTool{name: toolName, perm: authz.Permission{StateImpact: authz.Passthrough}}

	// LLM script:
	//   turn 1 → call update_status (a pre-existing tool); the refresher injects
	//            the OLD sidecar tool at the top of this turn.
	//   turn 2 → call sidecar_echo; the refresher REPLACED it with newTool at the
	//            top of this turn, so newTool must be the one dispatched.
	//   turn 3 → agent_work_complete.
	script := []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID: "tu_1", Name: "update_status", Input: json.RawMessage(`{"text":"working"}`),
			}}},
			StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		{Resp: llm.Response{
			Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID: "tu_2", Name: toolName, Input: json.RawMessage(`{}`),
			}}},
			StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		{Resp: llm.Response{
			Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID: "tu_3", Name: "agent_work_complete", Input: json.RawMessage(`{"summary":"done"}`),
			}}},
			StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
	}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", Generation: 1},
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	provider := llmfake.New(script)
	key := memory.NamespacedName{Namespace: "default", Name: "s1"}
	store := runner.LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key)

	tools := append(meta.Load(), meta.NewUpdateStatus(meta.UpdateStatusConfig{}))

	refreshCalls := 0
	l := &runner.Loop{
		Provider:     provider,
		Memory:       store,
		Status:       runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
		Tools:        tools,
		System:       "you are a test agent",
		UserPrompt:   "do the thing",
		Budget:       runner.NewBudget(spiceboxv1alpha1.BudgetConfig{MaxTurns: 50, MaxTokens: 100000, MaxDuration: metav1.Duration{Duration: time.Hour}}, nil, time.Now()),
		Model:        "claude-test",
		MaxTokens:    1024,
		SessionKey:   key,
		ToolAuthMode: runner.ToolAuthModeDisabled,
		ToolRefresher: func(_ context.Context) (runner.ToolRefreshResult, error) {
			refreshCalls++
			switch refreshCalls {
			case 1:
				return runner.ToolRefreshResult{Added: []tool.Tool{oldTool}}, nil // sidecar Ready before turn 1
			case 2:
				return runner.ToolRefreshResult{Added: []tool.Tool{newTool}}, nil // pod replaced: same name, new instance
			default:
				return runner.ToolRefreshResult{}, nil
			}
		},
	}

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	// The replacement instance is the one the LLM reached on turn 2; the stale
	// instance must never have run after being replaced.
	assert.Equal(t, 1, newTool.executes, "the replacement tool must be the one dispatched")
	assert.Equal(t, 0, oldTool.executes, "the replaced (stale) tool must not be dispatched")

	// No duplication: across every request the def appears at most once, even
	// though two instances of the same name passed through the refresher.
	for i, req := range provider.Requests() {
		assert.LessOrEqual(t, countToolDef(req, toolName), 1,
			"tool def must never be duplicated in request %d after replacement", i)
	}
}

// TestLoop_ToolRefresher_ErrorIsNonFatal verifies that a refresher error does
// not stall the loop: the turn proceeds with the existing tool set and the
// session completes normally.
func TestLoop_ToolRefresher_ErrorIsNonFatal(t *testing.T) {
	resp := llm.Response{
		Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
			ID: "tu_1", Name: "agent_work_complete", Input: json.RawMessage(`{"summary":"done"}`),
		}}},
		StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
	}
	l, _, _, _ := newLoopFixture(t, []llmfake.Step{{Resp: resp}})
	l.ToolRefresher = func(_ context.Context) (runner.ToolRefreshResult, error) {
		return runner.ToolRefreshResult{}, assertErr{}
	}

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")), "refresher error must not fail the loop")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, l.Status.Get(memory.WithSystemApproval(context.Background(), "test"), &got))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, got.Status.Phase,
		"session must complete despite the refresher error")
}

type assertErr struct{}

func (assertErr) Error() string { return "refresh boom" }
