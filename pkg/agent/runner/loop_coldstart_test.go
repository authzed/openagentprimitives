package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// The pure status→placement helper (ColdStartTurnContent) and the envelope
// builder (ColdStartEnvelope) now live in pkg/authz/hooks and are covered by
// TestColdStartScope_TurnContent / TestColdStartScope_Envelope there. The
// publish/wait/map orchestration lives in the ColdStartScope hook, covered by
// TestColdStartScope_Eval. This file keeps only the runner-level predicate
// test (TestColdStartEligible) and the end-to-end fail-closed test
// (TestRunColdStartFailClosed_*), both of which exercise runner code that
// did not move.

// csFakeTool is a minimal tool.Tool used to populate the cold-start envelope.
// It is never Executed in these tests.
type csFakeTool struct{ name string }

func (f *csFakeTool) Name() string                 { return f.name }
func (f *csFakeTool) Kind() tool.Kind              { return tool.KindMCP }
func (f *csFakeTool) Description() string          { return "" }
func (f *csFakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f *csFakeTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}
func (*csFakeTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*csFakeTool) PermissionVariants() []authz.PermissionVariant { return nil }

// newRunColdStartLoop builds a *Loop wired for runner-driven cold start: a
// scope-enabled AgentClass, a real engine over a shared in-process memory, a
// recording ColdStartRequestPublish, plus two tools and a bound entity so the
// envelope carries content.
func newRunColdStartLoop(t *testing.T, scopeOn bool, coldStart string) (*Loop, memory.Memory, memory.Scope) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	key := memory.NamespacedName{Namespace: "default", Name: "rcs1"}
	l := &Loop{
		Memory:             LocalMemoryAdapter(mem, key),
		Mem:                mem,
		SessionKey:         key,
		StartedByCanonical: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Engine:             engine.New(engine.Deps{Memory: mem}),
		Tools: []tool.Tool{
			&csFakeTool{name: "github_list_prs"},
			&csFakeTool{name: "send_email"},
		},
		ColdStartRequestPublish: func(_ context.Context, _, _ string, _ []byte) error { return nil },
		AgentClass: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				BoundEntities: []spiceboxv1alpha1.BoundEntityType{
					{ResourceType: "github_repo", Permission: "read"},
				},
				Authz: &spiceboxv1alpha1.AuthzBlock{
					// Consolidated approval-WAIT timeout (sources the cold-start wait).
					ApprovalTimeout: &metav1.Duration{Duration: 200 * time.Millisecond},
					Scope: &spiceboxv1alpha1.ScopeSpec{
						Enabled:         scopeOn,
						ColdStart:       coldStart,
						MaxLLMLatencyMs: 200, // keep the auto-apply wait short in tests
					},
				},
			},
		},
	}
	return l, mem, l.bindingScope()
}

// TestRunColdStartFailClosed_HaltsSessionWithoutRunningAgent proves the
// fail-closed wiring end-to-end at the Run level (the swapped SessionStart
// path): when cold-start scope review cannot complete (no cold_start_task is
// ever written → timeout), Run must halt the session via l.fail with
// ReasonAgentSessionScopeReviewFailed and the LLM provider must NEVER be called
// — the agent does not run unscoped.
func TestRunColdStartFailClosed_HaltsSessionWithoutRunningAgent(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	// scopeOn + extractAndApprove + no cold_start_task seeded → scope review
	// times out → fail closed.
	l, _, _ := newRunColdStartLoop(t, true, "extractAndApprove")
	l.AgentClass.Spec.Authz.Scope.MaxLLMLatencyMs = 50 // resolve the wait fast
	l.AgentClass.Spec.Authz.ApprovalTimeout = &metav1.Duration{Duration: 50 * time.Millisecond}
	l.UserPrompt = "open 5 PRs and email the board"

	provider := llmfake.New(nil) // empty script: any Send call is a test failure
	l.Provider = provider
	l.Status = LocalStatusPatcher()
	l.Budget = NewBudget(spiceboxv1alpha1.BudgetConfig{}, nil, time.Now())

	var notices []string
	l.Notify = func(_ context.Context, text string) { notices = append(notices, text) }

	err := l.Run(ctx)
	assert.NoError(t, err, "Run returns nil on a clean terminal write (the failure is recorded as status)")

	fail := l.Status.LocalFailure()
	if assert.NotNil(t, fail, "session must be marked failed") {
		assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionScopeReviewFailed, fail.Reason,
			"fail-closed reason must be ScopeReviewFailed")
	}

	assert.Empty(t, provider.Requests(),
		"the LLM provider must NEVER be called when scope review fails (agent must not run unscoped)")
	assert.NotEmpty(t, notices, "the user must be notified the session was stopped")
}

// TestColdStartEligible is a table over the runner-driven cold-start predicate.
func TestColdStartEligible(t *testing.T) {
	base := func() *Loop {
		mem := memory.NewLocal(inmem.NewBackend())
		return &Loop{
			Mem:                     mem,
			Engine:                  engine.New(engine.Deps{Memory: mem}),
			ColdStartRequestPublish: func(_ context.Context, _, _ string, _ []byte) error { return nil },
			AgentClass: &spiceboxv1alpha1.AgentClass{
				Spec: spiceboxv1alpha1.AgentClassSpec{
					Authz: &spiceboxv1alpha1.AuthzBlock{
						Scope: &spiceboxv1alpha1.ScopeSpec{Enabled: true, ColdStart: "extractAndApprove"},
					},
				},
			},
		}
	}

	cases := []struct {
		name   string
		mutate func(l *Loop)
		want   bool
	}{
		{name: "fully wired + scope on + coldStart!=off: eligible", mutate: func(*Loop) {}, want: true},
		{name: "scope disabled: not eligible", mutate: func(l *Loop) { l.AgentClass.Spec.Authz.Scope.Enabled = false }, want: false},
		{name: "coldStart=off: not eligible", mutate: func(l *Loop) { l.AgentClass.Spec.Authz.Scope.ColdStart = "off" }, want: false},
		{name: "nil publish hook (kubectl/no NATS): not eligible", mutate: func(l *Loop) { l.ColdStartRequestPublish = nil }, want: false},
		{name: "nil Mem: not eligible", mutate: func(l *Loop) { l.Mem = nil }, want: false},
		{name: "nil Engine: not eligible", mutate: func(l *Loop) { l.Engine = nil }, want: false},
		{name: "nil AgentClass: not eligible", mutate: func(l *Loop) { l.AgentClass = nil }, want: false},
		{
			name: "forked session: not eligible (fork must not re-run cold start)",
			mutate: func(l *Loop) {
				l.AgentSession = &spiceboxv1alpha1.AgentSession{Spec: spiceboxv1alpha1.AgentSessionSpec{ForkedFrom: "parent"}}
			},
			want: false,
		},
		{
			name: "non-forked session: eligible",
			mutate: func(l *Loop) {
				l.AgentSession = &spiceboxv1alpha1.AgentSession{Spec: spiceboxv1alpha1.AgentSessionSpec{ForkedFrom: ""}}
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := base()
			tc.mutate(l)
			assert.Equal(t, tc.want, l.coldStartEligible())
		})
	}
}
