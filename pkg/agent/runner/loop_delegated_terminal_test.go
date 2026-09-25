package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Two arms of Run park a channel-attached session at Idle rather than
// terminalizing it: the cold-start "nothing to place" arm, and the no-tool_use
// recovery arm. Idle is right for a session a PERSON is attached to — the
// conversation survives and the next message wakes it — and wrong for a
// delegated child, which has no person and whose SubagentRequest stays Running
// until the parent's own delegate / reply_to_subagent poll hits its 30-minute
// ceiling. reconcileChild resolves a delegation only on a terminal child phase.
//
// Both arms therefore FAIL a delegated child, each with its own reason, and
// leave every other session exactly as it was. The reason is the load-bearing
// half: reconcileChild copies status.failureReason into the request's
// determination, so it is the only text the parent's tool result carries.

// newNoToolUseLoop builds a channel-attached Loop whose single scripted
// response carries text and stop_reason=end_turn — no tool_use, and no
// agent_work_complete — which is exactly the recovery arm's trigger.
//
// Cold start is deliberately NOT wired (ColdStartRequestPublish is nil), so
// coldStartEligibility() is false and turn 0 is placed verbatim; this fixture
// reaches the model, which the cold-start fixture below must never do.
func newNoToolUseLoop(t *testing.T) (*Loop, *llmfake.Provider) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	key := memory.NamespacedName{Namespace: "default", Name: "ntu1"}

	provider := llmfake.New([]llmfake.Step{{Resp: llm.Response{
		Content:    []llm.ContentBlock{{Type: "text", Text: "I'm done."}},
		StopReason: "end_turn",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}})

	l := &Loop{
		Memory:             LocalMemoryAdapter(mem, key),
		Mem:                mem,
		SessionKey:         key,
		StartedByCanonical: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Engine:             engine.New(engine.Deps{Memory: mem}),
		Provider:           provider,
		Status:             LocalStatusPatcher(),
		Budget:             NewBudget(spiceboxv1alpha1.BudgetConfig{MaxTurns: 5, MaxTokens: 10000, MaxDuration: metav1.Duration{Duration: time.Hour}}, nil, time.Now()),
		System:             "you are a test agent",
		UserPrompt:         "do the thing",
		Model:              "claude-test",
		MaxTokens:          1024,
		ChannelAttached:    true,
		AgentClass: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Authz: &spiceboxv1alpha1.AuthzBlock{
					ApprovalTimeout: &metav1.Duration{Duration: 50 * time.Millisecond},
				},
			},
		},
	}
	return l, provider
}

func TestNoToolUse_DelegatedChildFailsStalledInsteadOfParkingIdle(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	l, provider := newNoToolUseLoop(t)
	l.DelegatedChild = true

	require.NoError(t, l.Run(ctx), "Run returns nil on a clean terminal write; the failure is recorded as status")
	require.Len(t, provider.Requests(), 1, "precondition: the model was called once and answered without a tool_use")

	fail := l.Status.LocalFailure()
	require.NotNil(t, fail, "a delegated child must terminalize here: parking Idle leaves its SubagentRequest Running until the parent's poll times out")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionStalled, fail.Reason,
		"the reason is the only text reconcileChild carries back to the parent, and Stalled names this arm alone")
	assert.Contains(t, fail.Message, "agent_work_complete was not called",
		"the message must say what the child failed to do")
	assert.False(t, l.Status.LocalIdle(), "the delegated child must not also park Idle")
}

// TestNoToolUse_MessageNamesTheTerminalToolThisSessionWasOffered pins that the
// failure text is read off the session's own tool table.
//
// A delegated child is offered return_result IN PLACE OF agent_work_complete,
// and this message is the only text reconcileChild carries back to the parent —
// so naming a tool the child never had describes a failure that could not have
// happened. The case above keeps the default wording for a session whose table
// carries neither, which is every kubectl-driven fixture.
func TestNoToolUse_MessageNamesTheTerminalToolThisSessionWasOffered(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	l, _ := newNoToolUseLoop(t)
	l.DelegatedChild = true
	l.Tools = []tool.Tool{meta.NewReturnResult(meta.CompletionConfig{})}

	require.NoError(t, l.Run(ctx))

	fail := l.Status.LocalFailure()
	require.NotNil(t, fail)
	assert.Contains(t, fail.Message, "return_result was not called",
		"the parent must be told the child failed to call the tool it actually had")
	assert.NotContains(t, fail.Message, "agent_work_complete",
		"a tool this session was never offered must not appear in its failure")
}

func TestNoToolUse_NonDelegatedChannelSessionStillParksIdleWithoutFailing(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	l, provider := newNoToolUseLoop(t) // DelegatedChild stays false

	require.NoError(t, l.Run(ctx), "Run must succeed")
	require.Len(t, provider.Requests(), 1, "precondition: the model was called once and answered without a tool_use")

	assert.True(t, l.Status.LocalIdle(),
		"a person can still say something else and wake this session, so the silent recovery to Idle must survive unchanged")
	assert.Nil(t, l.Status.LocalFailure(),
		"a protocol slip must not post 'Agent failed: Stalled' to a session with a person on the other end")
}

// newColdStartDeniedLoop builds a scope-enabled, channel-attached Loop whose
// cold_start_task decision is already seeded as DENIED, so the SessionStart
// hook resolves at once and ColdStartTurnContent places nothing: there is no
// turn-0 and the agent must never run.
//
// llmfake.New(nil) is an empty script — any Send is a test failure, which is
// itself an assertion this arm never dispatches the agent.
func newColdStartDeniedLoop(t *testing.T) (*Loop, *llmfake.Provider) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")

	l, mem, scopeRef := newRunColdStartLoop(t, true, "extractAndApprove")
	require.NoError(t, coldstarttask.Put(ctx, mem, scopeRef, coldstarttask.Content{
		Status: coldstarttask.StatusDenied,
	}), "seed the approver's denial the hook reads back")

	provider := llmfake.New(nil)
	l.Provider = provider
	l.Status = LocalStatusPatcher()
	l.Budget = NewBudget(spiceboxv1alpha1.BudgetConfig{}, nil, time.Now())
	l.UserPrompt = "open 5 PRs and email the board"
	l.ChannelAttached = true
	l.Notify = func(context.Context, string) {}
	return l, provider
}

func TestColdStartDenied_DelegatedChildFailsColdStartDeniedInsteadOfParkingIdle(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	l, provider := newColdStartDeniedLoop(t)
	l.DelegatedChild = true

	require.NoError(t, l.Run(ctx), "Run returns nil on a clean terminal write; the failure is recorded as status")
	require.Empty(t, provider.Requests(), "precondition: a denied cold start never dispatches the agent")

	fail := l.Status.LocalFailure()
	require.NotNil(t, fail, "a refused delegation must terminalize; Idle would leave the parent polling to its own timeout")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionColdStartDenied, fail.Reason,
		"the reason must name this arm and not be confusable with ScopeReviewFailed, which means review could not complete at all")
	assert.Contains(t, fail.Message, "cold-start scope review",
		"the message must say the refusal happened before the agent started")
	assert.False(t, l.Status.LocalIdle(), "the delegated child must not also park Idle")
}

func TestColdStartDenied_NonDelegatedChannelSessionStillParksIdleWithoutFailing(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	l, provider := newColdStartDeniedLoop(t) // DelegatedChild stays false

	require.NoError(t, l.Run(ctx), "Run must succeed")
	require.Empty(t, provider.Requests(), "precondition: a denied cold start never dispatches the agent")

	assert.True(t, l.Status.LocalIdle(),
		"the conversation stays alive for a follow-up, so a denied first prompt must still park rather than fail")
	assert.Nil(t, l.Status.LocalFailure(),
		"authzd already told the requester it was denied; failing the session too would be a second, wrong ending")
}
