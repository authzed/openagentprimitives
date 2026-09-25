package runner_test

// Tests for the per-tool authz gate wired into dispatchToolUses.
// Scenarios:
//
//   - TestToolDispatchAllow: SpiceDB returns HasPermission → tool.Execute fires.
//   - TestToolDispatchDeny:  SpiceDB returns NoPermission → tool.Execute does NOT
//     fire; Result fed back to LLM is IsError with a "permission denied" message.
//   - TestNoAuthzCli_FailsClosedInEnforcing: nil AuthzCli + enforcing mode →
//     dispatch fails closed (Execute not called, IsError surfaced).
//   - TestNoAuthzCli_DisabledModeBypassesCheck: nil AuthzCli + ToolAuthMode
//     "disabled" → Execute fires (the only sanctioned bypass path).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// fakeAuthzClient is a test-local SpiceDB CheckPermission client stub.
type fakeAuthzClient struct {
	answer v1.CheckPermissionResponse_Permissionship
	callsN int // how many CheckPermission calls were made
}

func (f *fakeAuthzClient) CheckPermission(_ context.Context, _ *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	f.callsN++
	return &v1.CheckPermissionResponse{
		Permissionship: f.answer,
		CheckedAt:      &v1.ZedToken{Token: "tok-test"},
	}, nil
}

// countingTool is a minimal tool.Tool whose Execute increments a counter.
// Used to verify whether the tool body ran (allow) or was short-circuited (deny).
type countingTool struct {
	name     string
	executes int
	perm     authz.Permission
	variants []authz.PermissionVariant
}

func (ct *countingTool) Name() string                 { return ct.name }
func (ct *countingTool) Kind() tool.Kind              { return tool.KindSandbox }
func (ct *countingTool) Description() string          { return "test tool" }
func (ct *countingTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (ct *countingTool) Permission() authz.Permission { return ct.perm }
func (ct *countingTool) PermissionVariants() []authz.PermissionVariant {
	return ct.variants
}
func (ct *countingTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	ct.executes++
	return tool.Result{Content: "executed", IsError: false, Terminal: false}, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// makeAuthzLoop builds a Loop with the given tool list and LLM script.
// It does NOT set AuthzCli — callers do that themselves.
func makeAuthzLoop(t *testing.T, tools []tool.Tool, script []llmfake.Step, sessAnnotations map[string]string) (*runner.Loop, *llmfake.Provider) {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "authz-sess",
			Namespace:   "default",
			Generation:  1,
			Annotations: sessAnnotations,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	provider := llmfake.New(script)
	key := memory.NamespacedName{Namespace: "default", Name: "authz-sess"}

	l := &runner.Loop{
		Provider:   provider,
		Memory:     runner.LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key),
		Status:     runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
		Tools:      tools,
		System:     "test",
		UserPrompt: "test",
		Budget: runner.NewBudget(spiceboxv1alpha1.BudgetConfig{
			MaxTurns:    10,
			MaxTokens:   100000,
			MaxDuration: metav1.Duration{Duration: time.Hour},
		}, nil, time.Now()),
		Model:      "claude-test",
		MaxTokens:  1024,
		SessionKey: key,
	}
	return l, provider
}

// workCompleteResp returns a scripted LLM response for agent_work_complete.
func workCompleteResp() llmfake.Step {
	return llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_done",
				Name:  "agent_work_complete",
				Input: json.RawMessage(`{"summary":"done"}`),
			},
		}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}
}

// ─── tests ───────────────────────────────────────────────────────────────────

// TestToolDispatchAllow verifies that when SpiceDB grants permission, the
// tool's Execute method fires and the result is not an error.
func TestToolDispatchAllow(t *testing.T) {
	cli := &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}

	testTool := &countingTool{
		name: "counted_tool",
		perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "read",
			},
		},
	}

	// Script: call counted_tool → agent_work_complete.
	toolCallStep := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_allow",
				Name:  "counted_tool",
				Input: json.RawMessage(`{"repo":"spicedb"}`),
			},
		}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}

	tools := append(meta.Load(), testTool)
	annotations := map[string]string{
		slack.LastInboundCanonicalIDAnnotationKey: "canonical-user-allow",
	}
	l, _ := makeAuthzLoop(t, tools, []llmfake.Step{toolCallStep, workCompleteResp()}, annotations)

	// Wire authz with a session that has the canonical annotation set.
	l.AuthzCli = cli
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, 1, testTool.executes, "tool Execute should fire once on allow")
	assert.Equal(t, 1, cli.callsN, "expected 1 SpiceDB call")
}

// TestToolDispatchDeny verifies that when SpiceDB denies permission:
//   - the tool's Execute method is NOT called
//   - the result fed back to the LLM is IsError=true with a "permission denied" message
func TestToolDispatchDeny(t *testing.T) {
	cli := &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}

	testTool := &countingTool{
		name: "counted_tool",
		perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "read",
			},
		},
	}

	// Script step 1: LLM calls counted_tool (will be denied).
	denyStep := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_deny",
				Name:  "counted_tool",
				Input: json.RawMessage(`{"repo":"private-repo"}`),
			},
		}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}

	tools := append(meta.Load(), testTool)
	annotations := map[string]string{
		slack.LastInboundCanonicalIDAnnotationKey: "canonical-user-deny",
	}
	l, provider := makeAuthzLoop(t, tools, []llmfake.Step{denyStep, workCompleteResp()}, annotations)

	l.AuthzCli = cli
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	assert.Equal(t, 0, testTool.executes, "tool Execute should NOT fire on deny")
	assert.Equal(t, 1, cli.callsN, "expected 1 SpiceDB call")

	// Inspect the second LLM request — it should include an IsError tool_result
	// with "permission denied" in the content.
	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 2, "expected at least 2 LLM requests")
	secondReq := reqs[1]
	var denyMsg string
	for _, msg := range secondReq.Messages {
		for _, blk := range msg.Content {
			if blk.ToolResult != nil && blk.ToolResult.IsError {
				denyMsg = blk.ToolResult.Content
			}
		}
	}
	require.NotEmpty(t, denyMsg, "no IsError tool_result found in second LLM request")
	assert.Contains(t, denyMsg, "permission denied")
}

// TestRunner_HonorsPermissionVariants_DenyOnContacts verifies the slice-4
// CheckWithVariants wiring through dispatchToolUses:
//
//   - The tool declares PermissionVariants that route on args.objectType:
//     "contacts" → Readonly Check against crm_company; "companies" →
//     Passthrough (no Check).
//   - When the LLM calls with objectType="contacts" and SpiceDB denies,
//     the tool's Execute is short-circuited (IsError to the LLM).
//   - When the LLM calls with objectType="companies", the Passthrough
//     variant skips SpiceDB entirely and Execute fires.
func TestRunner_HonorsPermissionVariants_DenyOnContacts(t *testing.T) {
	// Deny-everything fake: any contact-routed call is rejected.
	cli := &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}

	testTool := &countingTool{
		name: "crm_search",
		// Fallback Permission stays Passthrough; the variants do the
		// real routing.
		perm: authz.Permission{StateImpact: authz.Passthrough},
		variants: []authz.PermissionVariant{
			{
				When: `args.objectType == "contacts"`,
				Check: authz.Permission{
					StateImpact: authz.Readonly,
					Check: &authz.PermissionCheck{
						ResourceType:   "crm_company",
						ResourceIDExpr: `"acme"`,
						Permission:     "contact_access",
					},
				},
			},
			{
				When:  `args.objectType == "companies"`,
				Check: authz.Permission{StateImpact: authz.Passthrough},
			},
		},
	}

	// Script: call crm_search with contacts (denied) → call again with
	// companies (allowed via passthrough) → agent_work_complete.
	contactsStep := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_contacts",
				Name:  "crm_search",
				Input: json.RawMessage(`{"objectType":"contacts"}`),
			},
		}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}
	companiesStep := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_companies",
				Name:  "crm_search",
				Input: json.RawMessage(`{"objectType":"companies"}`),
			},
		}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}

	tools := append(meta.Load(), testTool)
	annotations := map[string]string{
		slack.LastInboundCanonicalIDAnnotationKey: "canonical-user-variant",
	}
	l, provider := makeAuthzLoop(t, tools, []llmfake.Step{contactsStep, companiesStep, workCompleteResp()}, annotations)

	l.AuthzCli = cli
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	// The contacts call (variant 1, Readonly) was denied. The companies
	// call (variant 2, Passthrough) skipped SpiceDB and ran. Net: exactly
	// 1 execute (companies), 1 SpiceDB call (contacts).
	assert.Equal(t, 1, testTool.executes, "expected exactly 1 Execute (companies-variant only)")
	assert.Equal(t, 1, cli.callsN, "expected exactly 1 SpiceDB call (contacts-variant only)")

	// Verify the contacts-variant deny was surfaced to the LLM as an
	// IsError tool_result. The second LLM request is the one carrying
	// the result of the first (contacts) tool call.
	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 2)
	var foundDeny bool
	for _, msg := range reqs[1].Messages {
		for _, blk := range msg.Content {
			if blk.ToolResult != nil && blk.ToolResult.IsError &&
				strings.Contains(blk.ToolResult.Content, "permission denied") {
				foundDeny = true
			}
		}
	}
	assert.True(t, foundDeny, "expected IsError tool_result with 'permission denied' for the contacts variant")
}

// TestRunner_PermissionVariants_UnwrapsWrappedEnvelope pins the
// production wiring: tools that go through tool.WrapInputSchema
// (every MCP tool and every sandbox tool) emit args wrapped in
// {operation_id, _reason, args}. Authz CEL in MCPServer CRs is
// written against the INNER args, so variant `when` clauses must see
// the unwrapped map — otherwise no variant matches in production,
// the tool falls back to its (passthrough) Permission, and every
// contacts query slips through without a Check. That was the live
// "why didn't approval fire" footgun.
func TestRunner_PermissionVariants_UnwrapsWrappedEnvelope(t *testing.T) {
	cli := &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}

	testTool := &countingTool{
		name: "crm_search",
		perm: authz.Permission{StateImpact: authz.Passthrough},
		variants: []authz.PermissionVariant{
			{
				When: `args.objectType == "contacts"`,
				Check: authz.Permission{
					StateImpact: authz.Readonly,
					Check: &authz.PermissionCheck{
						ResourceType:   "crm_company",
						ResourceIDExpr: `"acme"`,
						Permission:     "contact_access",
					},
				},
			},
		},
	}

	// Production-shape Input: the {operation_id, _reason, args} wrapper
	// every WrapInputSchema'd tool gets at synthesis time.
	contactsStep := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_wrapped",
				Name:  "crm_search",
				Input: json.RawMessage(`{"operation_id":"op-1","_reason":"fetch contacts on acme","args":{"objectType":"contacts"}}`),
			},
		}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}

	tools := append(meta.Load(), testTool)
	annotations := map[string]string{
		slack.LastInboundCanonicalIDAnnotationKey: "canonical-user-wrapped",
	}
	l, _ := makeAuthzLoop(t, tools, []llmfake.Step{contactsStep, workCompleteResp()}, annotations)
	l.AuthzCli = cli
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	// The contacts variant matched (because the runner unwrapped
	// before evaluating the When clause), the Readonly Check ran and
	// SpiceDB denied. So:
	//   - exactly 1 SpiceDB call (the contacts variant's Check).
	//   - 0 Execute calls (the deny short-circuited the dispatch).
	// If the unwrap regresses, the variant won't match, the tool
	// falls back to passthrough, no Check runs, and Execute fires.
	assert.Equal(t, 1, cli.callsN, "Readonly variant must fire its SpiceDB Check (got %d)", cli.callsN)
	assert.Equal(t, 0, testTool.executes, "denied call must NOT reach Execute (got %d)", testTool.executes)
}

// TestRunner_PermissionVariants_FailsClosedOnCELError pins authz's
// fail-closed promise: a malformed variant `when` (CEL that errors at
// eval — here, referencing a missing field without has()) MUST NOT
// silently fall back to the tool's top-level Permission. Authz code
// without surfaced errors is exactly the silent-passthrough footgun
// the in-cluster contacts query was hitting. Errored variant ⇒ the
// dispatch is denied with the error reported to the LLM, not allowed
// via fallback.
func TestRunner_PermissionVariants_FailsClosedOnCELError(t *testing.T) {
	cli := &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}

	testTool := &countingTool{
		name: "crm_search",
		// Fallback is passthrough — without the fix the silent
		// swallow lets the tool through here.
		perm: authz.Permission{StateImpact: authz.Passthrough},
		variants: []authz.PermissionVariant{
			{
				// args.fieldThatDoesNotExist on a dyn map raises a
				// CEL "no such key" error during EvalBool.
				When:  `args.fieldThatDoesNotExist == "x"`,
				Check: authz.Permission{StateImpact: authz.Passthrough},
			},
		},
	}

	step := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_celerr",
				Name:  "crm_search",
				Input: json.RawMessage(`{"operation_id":"op-1","_reason":"r","args":{"objectType":"contacts"}}`),
			},
		}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}

	tools := append(meta.Load(), testTool)
	annotations := map[string]string{slack.LastInboundCanonicalIDAnnotationKey: "canonical-user-celerr"}
	l, provider := makeAuthzLoop(t, tools, []llmfake.Step{step, workCompleteResp()}, annotations)
	l.AuthzCli = cli
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	assert.Equal(t, 0, testTool.executes,
		"CEL-errored variant must fail closed — Execute MUST NOT fire (got %d)", testTool.executes)

	// The error must be visible to the LLM as an IsError tool_result.
	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 2, "expected the deny to feed back into the LLM")
	var foundErr bool
	for _, msg := range reqs[1].Messages {
		for _, blk := range msg.Content {
			if blk.ToolResult != nil && blk.ToolResult.IsError &&
				strings.Contains(strings.ToLower(blk.ToolResult.Content), "variant") {
				foundErr = true
			}
		}
	}
	assert.True(t, foundErr, "variant CEL error must surface as a visible IsError tool_result (not silently swallowed)")
}

// TestNoAuthzCli_FailsClosedInEnforcing verifies that when AuthzCli is nil
// AND ToolAuthMode is the default ("enforcing"), the dispatch FAILS CLOSED:
// the tool's Execute method is NOT called and an IsError tool_result is
// surfaced to the LLM. This is the desired fail-safe behavior — a missing
// or mis-wired SpiceDB client must never silently bypass per-tool checks.
// The only sanctioned bypass is the explicit ToolAuthMode "disabled" opt-in
// (covered by TestNoAuthzCli_DisabledModeBypassesCheck below).
func TestNoAuthzCli_FailsClosedInEnforcing(t *testing.T) {
	testTool := &countingTool{
		name: "counted_tool",
		perm: authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "write",
			},
		},
	}

	noAuthzStep := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_no_authz",
				Name:  "counted_tool",
				Input: json.RawMessage(`{"repo":"anything"}`),
			},
		}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}

	tools := append(meta.Load(), testTool)
	l, provider := makeAuthzLoop(t, tools, []llmfake.Step{noAuthzStep, workCompleteResp()}, nil)
	// AuthzCli is intentionally nil; ToolAuthMode left unset → defaults to enforcing.

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, 0, testTool.executes, "fail-closed: tool Execute must NOT fire when AuthzCli is nil in enforcing mode")

	// The LLM should see an IsError tool_result back, not a silent allow.
	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 2, "expected at least 2 LLM requests")
	var denyMsg string
	for _, msg := range reqs[1].Messages {
		for _, blk := range msg.Content {
			if blk.ToolResult != nil && blk.ToolResult.IsError {
				denyMsg = blk.ToolResult.Content
			}
		}
	}
	require.NotEmpty(t, denyMsg, "fail-closed: an IsError tool_result must be surfaced")
	assert.Contains(t, strings.ToLower(denyMsg), "authz", "deny message should mention authz so the model knows why")
}

// TestNoAuthzCli_DisabledModeBypassesCheck verifies that the ONLY sanctioned
// fail-open path is the explicit ToolAuthMode "disabled" opt-in: with that
// mode set, nil AuthzCli causes Execute to fire normally (preserving the
// kubectl-driven gen-agent / local-dev semantics).
func TestNoAuthzCli_DisabledModeBypassesCheck(t *testing.T) {
	testTool := &countingTool{
		name: "counted_tool",
		perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "read",
			},
		},
	}

	step := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_disabled",
				Name:  "counted_tool",
				Input: json.RawMessage(`{"repo":"anything"}`),
			},
		}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}

	tools := append(meta.Load(), testTool)
	l, _ := makeAuthzLoop(t, tools, []llmfake.Step{step, workCompleteResp()}, nil)
	l.ToolAuthMode = runner.ToolAuthModeDisabled

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, 1, testTool.executes, "disabled mode: tool Execute should fire once even without AuthzCli")
}
