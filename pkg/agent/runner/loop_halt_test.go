package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHaltHook returns Halt at the configured point. Models a fail-closed gate
// (tool-guard halt, scope-review failure, hook panic) — a Halt verdict must
// stop the turn loop with no further LLM calls.
type fakeHaltHook struct {
	point  pipeline.Point
	reason string
}

func (h *fakeHaltHook) Name() string             { return "fake_halt" }
func (h *fakeHaltHook) Points() []pipeline.Point { return []pipeline.Point{h.point} }
func (h *fakeHaltHook) Eval(_ context.Context, in pipeline.Input) pipeline.Decision {
	if in.Point != h.point {
		return pipeline.Decision{}
	}
	return pipeline.Decision{Verdict: pipeline.Halt, Reason: h.reason}
}

// TestHookHaltStopsLoop verifies that a hook returning Halt at PreToolCall makes
// Run stop with NO further Provider.Send calls. Pre-fix: the loop proceeds to a
// second Send (the script then exhausts); post-fix: the dispatch-level halt breaks
// the loop after exactly one Send.
func TestHookHaltStopsLoop(t *testing.T) {
	key := memory.NamespacedName{Namespace: "default", Name: "halt"}
	provider := llmfake.New([]llmfake.Step{{Resp: llm.Response{
		Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
			ID: "tu-1", Name: "reader", Input: json.RawMessage(`{"args":{}}`),
		}}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 5, OutputTokens: 2},
	}}})

	l := &Loop{
		Provider:   provider,
		Memory:     LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key),
		Status:     LocalStatusPatcher(),
		Tools:      []tool.Tool{&fakeDispatchTool{name: "reader", kind: tool.KindMCP, result: tool.Result{Content: "x"}}},
		System:     "you are a test agent",
		UserPrompt: "do the thing",
		Budget: NewBudget(spiceboxv1alpha1.BudgetConfig{
			MaxTurns: 50, MaxTokens: 100000, MaxDuration: metav1.Duration{Duration: time.Hour},
		}, nil, time.Now()),
		Model:      "claude-test",
		MaxTokens:  1024,
		SessionKey: key,
	}

	reg := pipeline.NewRegistry()
	reg.Register(&fakeHaltHook{point: pipeline.PreToolCall, reason: "fail-closed halt"}, 10)
	loopWithInjectedExecutor(t, l, reg)

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, 1, len(provider.Requests()),
		"loop stops after Halt — no extra Provider.Send call")
}

// TestOnAwaitYieldPausesProgress verifies that OnAwaitYield freezes the live
// progress reporter so the heartbeat emits NO KindTurnProgress while the session
// is yielded waiting for the user, and that OnAwaitResume re-enables it.
func TestOnAwaitYieldPausesProgress(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2000, 0)}
	var emits int
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "default", Name: "await"}}
	l.progress = newProgressReporter(
		func(_, _ int64, _, _ int) { emits++ },
		4*time.Second, 4*time.Second, clk.now,
	)

	// Past activation, output advanced: first emit.
	clk.advance(5 * time.Second)
	l.progress.observeUsage(100, 200)
	require.Equal(t, 1, emits, "first emit after activation delay")

	// Yield to the user: the clock must freeze for the whole await block, so
	// neither the token path nor the heartbeat emits while parked.
	l.OnAwaitYield(memory.WithSystemApproval(context.Background(), "test"))
	clk.advance(30 * time.Second)
	l.progress.observeUsage(100, 999) // output advanced + interval elapsed...
	l.progress.tick()                 // ...heartbeat fires too — but paused
	assert.Equal(t, 1, emits, "no KindTurnProgress emitted while yielded on the user")

	// Resume: emits flow again.
	l.OnAwaitResume(memory.WithSystemApproval(context.Background(), "test"))
	clk.advance(10 * time.Second)
	l.progress.observeUsage(100, 1500)
	assert.Equal(t, 2, emits, "resume re-enables progress emits")
}
