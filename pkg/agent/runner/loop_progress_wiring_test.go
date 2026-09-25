package runner_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// A single-turn script that terminates immediately via agent_work_complete.
func progressWiringLoop(t *testing.T, progressPublish func(int64, int64, int, int)) (*runner.Loop, *llmfake.Provider) {
	t.Helper()
	script := []llmfake.Step{{Resp: llm.Response{
		Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
			ID: "t1", Name: "agent_work_complete", Input: json.RawMessage(`{"summary":"done"}`),
		}}},
		StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}}
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

	l := &runner.Loop{
		Provider:        provider,
		Memory:          store,
		Status:          runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
		Tools:           meta.Load(),
		System:          "you are a test agent",
		UserPrompt:      "do the thing",
		Budget:          runner.NewBudget(spiceboxv1alpha1.BudgetConfig{MaxTurns: 50, MaxTokens: 100000, MaxDuration: metav1.Duration{Duration: time.Hour}}, nil, time.Now()),
		Model:           "claude-test",
		MaxTokens:       1024,
		SessionKey:      key,
		ToolAuthMode:    runner.ToolAuthModeDisabled,
		ProgressPublish: progressPublish,
	}
	return l, provider
}

func TestLoop_Run_WiresOnEventWhenProgressPublishSet(t *testing.T) {
	l, provider := progressWiringLoop(t, func(_, _ int64, _, _ int) {})
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	reqs := provider.Requests()
	require.NotEmpty(t, reqs)
	assert.NotNil(t, reqs[0].OnEvent, "OnEvent must be wired when ProgressPublish is set")
}

func TestLoop_Run_NoOnEventWhenNoConsumers(t *testing.T) {
	l, provider := progressWiringLoop(t, nil) // no ProgressPublish, no OnStreamEvent
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	reqs := provider.Requests()
	require.NotEmpty(t, reqs)
	assert.Nil(t, reqs[0].OnEvent, "OnEvent stays nil when neither consumer is set")
}
