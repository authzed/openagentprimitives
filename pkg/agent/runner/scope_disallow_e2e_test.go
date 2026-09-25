// pkg/agent/runner/scope_disallow_e2e_test.go
//
// End-to-end test for Layer-2 scope hard-deny enforcement, driving a real
// Loop.Run with a scripted fake LLM + a recording MCP tool + a seeded session
// scope. Proves the headline behaviors of the dynamic-scope disallow:
//
//   - It is enforced at tool dispatch even when toolCalls.mode=disabled (the
//     previous behavior skipped ALL enforcement in disabled mode).
//   - An id-glob ("ENG-*") blocks every matching resource; non-matching ones
//     pass through.
//   - idArg tools are blocked PRE-fetch (the tool never executes); resultIDField
//     tools are blocked POST-fetch (the tool runs but its result is discarded
//     before reaching the LLM).
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
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// recordingTool is a minimal MCP tool that records whether it executed and
// returns a fixed JSON result. Used to detect pre-fetch (never executed) vs
// post-fetch (executed, result discarded) blocking.
type recordingTool struct {
	name   string
	result string
	calls  int
}

func (r *recordingTool) Name() string                 { return r.name }
func (r *recordingTool) Kind() tool.Kind              { return tool.KindMCP }
func (r *recordingTool) Description() string          { return "fetch a record" }
func (r *recordingTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (r *recordingTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Readonly}
}
func (r *recordingTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (r *recordingTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	r.calls++
	return tool.Result{Content: r.result}, nil
}

// toolUseResp builds a single-tool_use assistant response.
func toolUseResp(id, name string, input string) llm.Response {
	return llm.Response{
		StopReason: "tool_use",
		Content: []llm.ContentBlock{{
			Type:    "tool_use",
			ToolUse: &llm.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)},
		}},
	}
}

// findToolResult returns the (content, isError) of the first tool_result block
// addressed to toolUseID across all messages in the request.
func findToolResult(t *testing.T, req llm.Request, toolUseID string) (string, bool) {
	t.Helper()
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolResult != nil && b.ToolResult.ToolUseID == toolUseID {
				return b.ToolResult.Content, b.ToolResult.IsError
			}
		}
	}
	t.Fatalf("no tool_result for %q in request", toolUseID)
	return "", false
}

func TestE2E_ScopeDisallow_EnforcedAtDispatch(t *testing.T) {
	const denyGlob = "ENG-*"

	cases := []struct {
		name         string
		idArg        bool   // true: reads.idArg=id (pre-fetch); false: reads.resultIDField=id (post-fetch)
		issueID      string // the issue the agent fetches
		wantBlocked  bool
		wantExecuted bool // whether the tool's Execute ran (pre-fetch block ⇒ false)
	}{
		{name: "idArg ENG issue: blocked PRE-fetch (tool never runs)", idArg: true, issueID: "ENG-369", wantBlocked: true, wantExecuted: false},
		{name: "idArg non-ENG issue: allowed", idArg: true, issueID: "L-140", wantBlocked: false, wantExecuted: true},
		{name: "resultIDField ENG issue: blocked POST-fetch (tool runs, result discarded)", idArg: false, issueID: "ENG-369", wantBlocked: true, wantExecuted: true},
		{name: "resultIDField non-ENG issue: allowed", idArg: false, issueID: "L-140", wantBlocked: false, wantExecuted: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			key := memory.NamespacedName{Namespace: "default", Name: "scope-e2e"}
			mem := memory.NewLocal(inmem.NewBackend())

			// Seed the session scope: hard-deny linear_issue "ENG-*".
			seeded := scope.ApplyDelta(scope.Scope{}, scope.ScopeDelta{
				HardDeny: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: denyGlob}}},
			}, scope.SourceMetaagentApproved, time.Time{})
			require.NoError(t, sessionscope.Put(ctx, mem, memory.Scope{Kind: "session", ID: "default/scope-e2e"}, seeded))

			ft := &recordingTool{
				name:   "linear_get_issue",
				result: `{"id":"` + tc.issueID + `","title":"the issue body"}`,
			}

			// Scripted conversation: fetch the issue, then complete.
			provider := llmfake.New([]llmfake.Step{
				{Resp: toolUseResp("tu-1", "linear_get_issue", `{"args":{"id":"`+tc.issueID+`"}}`)},
				{Resp: toolUseResp("tu-2", "agent_work_complete", `{"summary":"done"}`)},
			})

			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Generation: 1},
			}
			c := fake.NewClientBuilder().
				WithScheme(newScheme(t)).
				WithObjects(sess).
				WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
				Build()

			l := &runner.Loop{
				Provider:   provider,
				Memory:     runner.LocalMemoryAdapter(mem, key),
				Mem:        mem,
				Status:     runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
				Tools:      append(meta.Load(), ft),
				System:     "test agent",
				UserPrompt: "summarize issue " + tc.issueID,
				Budget: runner.NewBudget(spiceboxv1alpha1.BudgetConfig{
					MaxTurns: 50, MaxTokens: 100000, MaxDuration: metav1.Duration{Duration: time.Hour},
				}, nil, time.Now()),
				Model:      "claude-test",
				MaxTokens:  1024,
				SessionKey: key,
				// scope ENABLED but tool-call authz DISABLED — the disallow must
				// still be enforced (the regression this guards).
				AgentClass: &spiceboxv1alpha1.AgentClass{
					Spec: spiceboxv1alpha1.AgentClassSpec{
						Authz: &spiceboxv1alpha1.AuthzBlock{
							Scope:     &spiceboxv1alpha1.ScopeSpec{Enabled: true},
							ToolCalls: &spiceboxv1alpha1.ToolCallsAuthz{Mode: "disabled"},
						},
					},
				},
				ToolAuthMode: runner.ToolAuthModeDisabled,
				LookupToolMapping: func(name string) *spiceboxv1alpha1.ToolResourceMapping {
					if name != "linear_get_issue" {
						return nil
					}
					reads := &spiceboxv1alpha1.ToolReads{ResourceType: "linear_issue", Permission: "view"}
					if tc.idArg {
						reads.IDArg = "id"
					} else {
						reads.ResultIDField = "id"
					}
					return &spiceboxv1alpha1.ToolResourceMapping{Tool: "linear_get_issue", Reads: reads}
				},
			}

			require.NoError(t, l.Run(ctx), "Run should complete (agent_work_complete)")

			// The 2nd LLM request carries the tool_result for linear_get_issue.
			reqs := provider.Requests()
			require.GreaterOrEqual(t, len(reqs), 2, "LLM should have been called at least twice")
			content, isErr := findToolResult(t, reqs[1], "tu-1")

			assert.Equal(t, tc.wantExecuted, ft.calls > 0, "tool execution (pre-fetch block ⇒ not executed)")
			if tc.wantBlocked {
				assert.True(t, isErr, "blocked call must surface IsError to the LLM")
				assert.Contains(t, content, "disallowed", "blocked result must explain the scope deny")
				assert.Contains(t, content, "linear_issue:"+tc.issueID, "blocked result names the resource")
				assert.NotContains(t, content, "the issue body", "blocked result must NOT leak the resource content")
			} else {
				assert.False(t, isErr, "allowed call must not be an error")
				assert.Contains(t, content, "the issue body", "allowed call must deliver the tool result")
			}
		})
	}
}
