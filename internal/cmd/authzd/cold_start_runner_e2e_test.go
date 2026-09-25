// End-to-end test wiring the REAL ColdStartHandler to a REAL runner Loop in
// process. Proves the full cold-start → scope-apply → dispatch-enforcement
// pipeline: the runner publishes a cold-start request, the handler extracts a
// hard-deny ("ENG-*") and applies it to the session scope, the runner places
// the cleaned task and runs the agent, and a later access to an ENG issue is
// blocked by the just-applied disallow — while a non-ENG issue passes.
//
// Lives in internal/cmd/authzd (package main) because ColdStartHandler is unexported
// here; the runner is importable, and nothing imports cmd/*, so there is no
// cycle. The extractor and agent LLMs are faked; everything else is real.
package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// e2eRecordingTool is a minimal MCP tool that records execution and returns a
// fixed JSON body, so a test can prove a call was blocked BEFORE execution.
type e2eRecordingTool struct {
	calls map[string]int // issueID → exec count (resolved from args.id)
}

func (r *e2eRecordingTool) Name() string                 { return "linear_get_issue" }
func (r *e2eRecordingTool) Kind() tool.Kind              { return tool.KindMCP }
func (r *e2eRecordingTool) Description() string          { return "fetch a linear issue" }
func (r *e2eRecordingTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (r *e2eRecordingTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Readonly}
}
func (r *e2eRecordingTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (r *e2eRecordingTool) Execute(_ context.Context, input json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var env struct {
		Args struct {
			ID string `json:"id"`
		} `json:"args"`
	}
	_ = json.Unmarshal(input, &env)
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[env.Args.ID]++
	return tool.Result{Content: `{"id":"` + env.Args.ID + `","title":"the issue body"}`}, nil
}

func e2eToolUse(id, name, input string) llm.Response {
	return llm.Response{StopReason: "tool_use", Content: []llm.ContentBlock{{
		Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)},
	}}}
}

func e2eFindToolResult(t *testing.T, req llm.Request, toolUseID string) (string, bool) {
	t.Helper()
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolResult != nil && b.ToolResult.ToolUseID == toolUseID {
				return b.ToolResult.Content, b.ToolResult.IsError
			}
		}
	}
	t.Fatalf("no tool_result for %q", toolUseID)
	return "", false
}

// TestE2E_ColdStartAppliesScope_RunnerEnforcesDisallow is the full pipeline:
// real ColdStartHandler (faked extractor) + real runner Loop (faked agent LLM).
func TestE2E_ColdStartAppliesScope_RunnerEnforcesDisallow(t *testing.T) {
	ctx := approvedCtx()
	key := memory.NamespacedName{Namespace: "default", Name: "cs-e2e"}
	scopeRef := memory.Scope{Kind: "session", ID: "default/cs-e2e"}
	mem := memory.NewLocal(inmem.NewBackend())

	// Real ColdStartHandler: extractor returns an ENG-* hard-deny + cleaned task.
	handler := &ColdStartHandler{
		Metaagent: &Metaagent{Memory: mem},
		Extractor: &fakeColdStartExtractor{out: scope.ColdStartExtraction{
			ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "ENG-*"}}}},
			CleanedTask: "summarize L-140",
		}},
	}
	approveCleaned := func(context.Context, scope.MetaagentOutput, string) (string, error) {
		return ColdStartApproveCleaned, nil
	}

	ft := &e2eRecordingTool{}

	// Agent LLM: fetch L-140 (allowed), then ENG-369 (blocked), then complete.
	provider := llmfake.New([]llmfake.Step{
		{Resp: e2eToolUse("tu-1", "linear_get_issue", `{"args":{"id":"L-140"}}`)},
		{Resp: e2eToolUse("tu-2", "linear_get_issue", `{"args":{"id":"ENG-369"}}`)},
		{Resp: e2eToolUse("tu-3", "agent_work_complete", `{"summary":"done"}`)},
	})

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Generation: 1},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()

	l := &runner.Loop{
		Provider:   provider,
		Memory:     runner.LocalMemoryAdapter(mem, key),
		Mem:        mem,
		Engine:     engine.New(engine.Deps{Memory: mem}),
		Status:     runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
		Tools:      append(meta.Load(), ft),
		System:     "test agent",
		UserPrompt: "summarize L-140 and do not read any issues in ENG",
		Budget: runner.NewBudget(spiceboxv1alpha1.BudgetConfig{
			MaxTurns: 50, MaxTokens: 100000, MaxDuration: metav1.Duration{Duration: time.Hour},
		}, nil, time.Now()),
		Model:              "claude-test",
		MaxTokens:          1024,
		SessionKey:         key,
		StartedByCanonical: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		ToolAuthMode:       runner.ToolAuthModeDisabled,
		AgentClass: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Authz: &spiceboxv1alpha1.AuthzBlock{
					Slots:     []spiceboxv1alpha1.AuthzSlot{{ResourceType: "linear_issue", Permission: "view"}},
					Scope:     &spiceboxv1alpha1.ScopeSpec{Enabled: true, ColdStart: "extractAndApprove"},
					ToolCalls: &spiceboxv1alpha1.ToolCallsAuthz{Mode: "disabled"},
				},
			},
		},
		LookupToolMapping: func(name string) *spiceboxv1alpha1.ToolResourceMapping {
			if name != "linear_get_issue" {
				return nil
			}
			// idArg → resolved pre-fetch, so an ENG issue is blocked before it runs.
			return &spiceboxv1alpha1.ToolResourceMapping{
				Tool:  "linear_get_issue",
				Reads: &spiceboxv1alpha1.ToolReads{ResourceType: "linear_issue", IDArg: "id", Permission: "view"},
			}
		},
		// Wire the cold-start request straight into the real handler (synchronous):
		// Handle extracts, applies the scope, and writes the cold_start_task before
		// returning, so the runner's WaitForColdStartTask finds it on the first poll.
		ColdStartRequestPublish: func(ctx context.Context, ns, name string, payload []byte) error {
			var p struct {
				Requester string                   `json:"requester"`
				Text      string                   `json:"text"`
				Envelope  scope.AgentClassEnvelope `json:"envelope"`
			}
			require.NoError(t, json.Unmarshal(payload, &p))
			return handler.Handle(ctx, scopeRef, sessRefString(scopeRef), ColdStartRequest{
				Requester: p.Requester, RequestText: p.Text, Envelope: p.Envelope, InboxIdx: 0,
			}, approveCleaned)
		},
	}

	require.NoError(t, l.Run(ctx), "Run should complete via agent_work_complete")

	// 1. Cold-start applied the scope: session_scope has the ENG-* disallow.
	sc, found, err := sessionscope.Get(ctx, mem, scopeRef)
	require.NoError(t, err)
	require.True(t, found, "cold-start must have written the session scope")
	assert.True(t, sc.ResourceDisallowed("linear_issue", "ENG-369"), "ENG-* disallow applied")
	assert.False(t, sc.ResourceDisallowed("linear_issue", "L-140"), "L-140 not disallowed")

	// 2. The cold_start_task is approved_cleaned with the stripped task.
	cst, ok, err := coldstarttask.Get(ctx, mem, scopeRef)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status)
	assert.Equal(t, "summarize L-140", cst.CleanedText)

	// 3. The agent ran the cleaned task: L-140 fetched + delivered.
	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 3, "expected ≥3 LLM turns")
	l140Content, l140Err := e2eFindToolResult(t, reqs[1], "tu-1")
	assert.False(t, l140Err, "L-140 must be allowed")
	assert.Contains(t, l140Content, "the issue body")
	assert.Equal(t, 1, ft.calls["L-140"], "L-140 fetched")

	// 4. ENG-369 was blocked PRE-fetch by the cold-start-applied disallow.
	engContent, engErr := e2eFindToolResult(t, reqs[2], "tu-2")
	assert.True(t, engErr, "ENG-369 must be blocked")
	assert.Contains(t, engContent, "disallowed")
	assert.Contains(t, engContent, "linear_issue:ENG-369")
	assert.Equal(t, 0, ft.calls["ENG-369"], "ENG-369 must NOT be fetched (pre-fetch block)")
}
