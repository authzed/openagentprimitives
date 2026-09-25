package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeApproveInstance is a minimal contentguard.Instance that runs at
// PostToolCall and always returns Approve, so the content_guard hook it
// drives builds a content_inspection ApprovalAsk.
type fakeApproveInstance struct{}

func (fakeApproveInstance) Points() []pipeline.Point { return []pipeline.Point{pipeline.PostToolCall} }
func (fakeApproveInstance) Inspect(_ context.Context, _ contentguard.Subject) (contentguard.Finding, error) {
	return contentguard.Finding{Action: contentguard.Approve, Reason: "needs review"}, nil
}

// fakePostInstance is a minimal contentguard.Instance that runs at PostToolCall
// and always returns Pass.
type fakePostInstance struct{}

func (fakePostInstance) Points() []pipeline.Point { return []pipeline.Point{pipeline.PostToolCall} }
func (fakePostInstance) Inspect(_ context.Context, _ contentguard.Subject) (contentguard.Finding, error) {
	return contentguard.Finding{Action: contentguard.Pass}, nil
}

// hookNames extracts the Name() strings from a slice of hooks.
func hookNames(hs []pipeline.Hook) []string {
	names := make([]string, 0, len(hs))
	for _, h := range hs {
		names = append(names, h.Name())
	}
	return names
}

// countContentGuardHooks counts hooks whose name contains "content_guard:" across
// both tool-call pipeline points. Used to assert default-off invariant without the
// vacuous-loop problem (an empty slice would make a per-element NotContains pass trivially).
func countContentGuardHooks(reg *pipeline.Registry) int {
	n := 0
	for _, p := range []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall} {
		for _, name := range hookNames(reg.Hooks(p)) {
			if strings.Contains(name, "content_guard:") {
				n++
			}
		}
	}
	return n
}

// setScopeEnabled sets AgentClass.Spec.Authz.Scope.Enabled on l.
func setScopeEnabled(l *Loop, enabled bool) {
	if l.AgentClass == nil {
		l.AgentClass = &spiceboxv1alpha1.AgentClass{}
	}
	if l.AgentClass.Spec.Authz == nil {
		l.AgentClass.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{}
	}
	l.AgentClass.Spec.Authz.Scope = &spiceboxv1alpha1.ScopeSpec{Enabled: enabled}
}

// fakeMCPTool is a minimal tool.Tool implementation with Kind() == KindMCP.
type fakeMCPTool struct{ name string }

func (f *fakeMCPTool) Name() string                                  { return f.name }
func (f *fakeMCPTool) Kind() tool.Kind                               { return tool.KindMCP }
func (f *fakeMCPTool) Description() string                           { return "fake mcp tool" }
func (f *fakeMCPTool) InputSchema() json.RawMessage                  { return json.RawMessage("{}") }
func (f *fakeMCPTool) Permission() authz.Permission                  { return authz.Permission{} }
func (f *fakeMCPTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (f *fakeMCPTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}

// setMCPTools adds a fake MCP tool to l.Tools when hasMCP is true.
func setMCPTools(l *Loop, hasMCP bool) {
	if !hasMCP {
		return
	}
	l.Tools = append(l.Tools, &fakeMCPTool{name: "test_server_test_tool"})
}

// setLeakageEnforcing sets LeakageConfig to enforcing mode.
func setLeakageEnforcing(l *Loop) {
	l.LeakageConfig = &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"}
}

func TestBuildPipeline_ScopeActiveWhenScopeEnabled_EvenIfToolAuthDisabled(t *testing.T) {
	l := &Loop{ToolAuthMode: ToolAuthModeDisabled}
	setScopeEnabled(l, true)
	reg := l.buildPipelineRegistry()
	names := hookNames(reg.Hooks(pipeline.PreToolCall))
	assert.NotContains(t, names, "tool_call_authz", "disabled mode drops ToolCallAuthz")
	assert.Contains(t, names, "scope", "scope.enabled keeps Scope active in disabled mode (gap fix)")
}

func TestBuildPipeline_PreToolCallOrder(t *testing.T) {
	l := &Loop{ToolAuthMode: ToolAuthModeEnforcing}
	setScopeEnabled(l, true)
	setMCPTools(l, true)
	reg := l.buildPipelineRegistry()
	assert.Equal(t, []string{"mcp_trust", "tool_call_authz", "scope"}, hookNames(reg.Hooks(pipeline.PreToolCall)))
}

func TestBuildPipeline_PostToolCallOrder(t *testing.T) {
	l := &Loop{ToolAuthMode: ToolAuthModeEnforcing}
	setScopeEnabled(l, true)
	setLeakageEnforcing(l)
	reg := l.buildPipelineRegistry()
	assert.Equal(t, []string{"scope", "info_leak_read", "info_leak_audience"}, hookNames(reg.Hooks(pipeline.PostToolCall)))
}

func TestBuildPipeline_PreResponseHasOnlyAudience(t *testing.T) {
	l := &Loop{}
	setLeakageEnforcing(l)
	reg := l.buildPipelineRegistry()
	assert.Equal(t, []string{"info_leak_audience"}, hookNames(reg.Hooks(pipeline.PreResponse)))
}

func TestBuildPipeline_SessionEndHasSessionCleanup(t *testing.T) {
	l := &Loop{}
	reg := l.buildPipelineRegistry()
	// SessionCleanup is registered unconditionally (nil-safe + cheap).
	assert.Equal(t, []string{"session_cleanup"}, hookNames(reg.Hooks(pipeline.SessionEnd)))
}

func TestBuildPipeline_RevocationGuardActiveOnlyWhenRevokedOriginsSet(t *testing.T) {
	t.Run("RevokedOrigins nil: revocation_guard absent from PreToolCall", func(t *testing.T) {
		l := &Loop{}
		reg := l.buildPipelineRegistry()
		assert.NotContains(t, hookNames(reg.Hooks(pipeline.PreToolCall)), "revocation_guard")
	})
	t.Run("RevokedOrigins set: revocation_guard present at PreToolCall", func(t *testing.T) {
		l := &Loop{RevokedOrigins: toolorigin.New()}
		reg := l.buildPipelineRegistry()
		assert.Contains(t, hookNames(reg.Hooks(pipeline.PreToolCall)), "revocation_guard")
	})
}

func TestBuildPipeline_SessionStartEmptyInCachedRegistry(t *testing.T) {
	l := &Loop{}
	setScopeEnabled(l, true)
	reg := l.buildPipelineRegistry()
	// ColdStartScope is built per-call at the Run() site (host-bound placement
	// callback), so the cached registry has NO SessionStart hooks.
	assert.Empty(t, hookNames(reg.Hooks(pipeline.SessionStart)),
		"SessionStart is empty in the cached registry; ColdStartScope is per-call")
}

func TestBuildPipelineRegistry_ContentGuard(t *testing.T) {
	t.Run("one inspector: hook registered at PostToolCall", func(t *testing.T) {
		l := &Loop{
			ContentInspectors:   []contentguard.Instance{fakePostInstance{}},
			ContentInspectorIDs: []string{"url-allowlist"},
		}
		reg := l.buildPipelineRegistry()
		names := hookNames(reg.Hooks(pipeline.PostToolCall))
		assert.Contains(t, names, "content_guard:url-allowlist")
		assert.GreaterOrEqual(t, countContentGuardHooks(reg), 1, "one inspector ⇒ at least one content-guard hook registered")
	})

	t.Run("default-off: empty Loop has no content_guard hook", func(t *testing.T) {
		empty := (&Loop{}).buildPipelineRegistry()
		// countContentGuardHooks checks BOTH PreToolCall and PostToolCall with a
		// substring match, so this assertion is never vacuous — it fires even when
		// both hook slices are empty.
		assert.Zero(t, countContentGuardHooks(empty), "default-off: no inspectors ⇒ zero content-guard hooks at any tool-call point")
	})
}

// TestBuildPipelineRegistry_ContentGuard_TimeoutPolicyMatchesLifecycleTable is
// an anti-drift lock on the content_guard HookFactory's wiring in
// hooks_dataplane.go: it drives the REAL registered "content_guard" factory
// (not timeoutPolicyFor directly) end-to-end — Loop.ContentInspectors →
// buildPipelineRegistry → the produced hook's Eval → the built
// content_inspection ApprovalAsk — and checks the resulting OnTimeout against
// the lifecycle decisionParams table directly (bypassing timeoutPolicyFor, so
// this doesn't just re-assert the helper's own logic). It fails if
// hooks_dataplane.go stops passing timeoutPolicyFor("content_inspection") into
// contentguard.NewAdapter, or if the table and the helper diverge.
func TestBuildPipelineRegistry_ContentGuard_TimeoutPolicyMatchesLifecycleTable(t *testing.T) {
	l := &Loop{
		ContentInspectors:   []contentguard.Instance{fakeApproveInstance{}},
		ContentInspectorIDs: []string{"url-allowlist"},
	}
	reg := l.buildPipelineRegistry()

	var found pipeline.Hook
	for _, h := range reg.Hooks(pipeline.PostToolCall) {
		if h.Name() == "content_guard:url-allowlist" {
			found = h
			break
		}
	}
	require.NotNil(t, found, "content_guard hook for the configured inspector must be registered at PostToolCall")

	dec := found.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "fetch", Result: "x"},
	})
	require.NotNil(t, dec.Approval, "Approve finding must produce an ApprovalAsk")

	want := pipeline.TimeoutDeny
	if lifecyclecore.FailsClosedOnTimeout(lifecyclecore.DecisionContentInspect) {
		want = pipeline.TimeoutHalt
	}
	assert.Equal(t, want, dec.Approval.OnTimeout,
		"content_guard wiring must stamp the lifecycle-table-derived timeout policy onto the content_inspection ask")
}

// TestInfoLeakReadDeps_NoTaintMapsToBypassNotFloor spans the wiring join the
// hook-level tests cannot: a toolResourceMap noTaint entry must resolve to a
// BYPASS decl (BypassRequesterCheck set, no id args), NOT nil. nil now means
// "undeclared" and falls to the coarse floor, so if this wiring reverts to
// returning nil for noTaint a noTaint tool silently floor-taints — the exact
// latent bug the floor change fixed. This test fails when that regresses.
func TestInfoLeakReadDeps_NoTaintMapsToBypassNotFloor(t *testing.T) {
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "ns1", Name: "sess1"},
		LookupToolMapping: func(name string) *spiceboxv1alpha1.ToolResourceMapping {
			switch name {
			case "notaint_tool":
				return &spiceboxv1alpha1.ToolResourceMapping{Tool: "notaint_tool", NoTaint: true}
			case "declared_tool":
				return &spiceboxv1alpha1.ToolResourceMapping{Tool: "declared_tool", Reads: &spiceboxv1alpha1.ToolReads{ResourceType: "thing", IDArg: "id", Permission: "view"}}
			default:
				return nil
			}
		},
	}
	deps := l.infoLeakReadDeps()

	notaint := deps.LookupReads("notaint_tool")
	require.NotNil(t, notaint, "noTaint must resolve to a bypass decl, not nil (nil = undeclared = floor)")
	assert.True(t, notaint.BypassRequesterCheck, "noTaint decl must set BypassRequesterCheck")
	assert.Empty(t, notaint.ResourceType, "noTaint bypass decl must carry no resource")
	assert.Empty(t, notaint.IDArg)
	assert.Empty(t, notaint.ResultIDField)

	declared := deps.LookupReads("declared_tool")
	require.NotNil(t, declared)
	assert.Equal(t, "thing", declared.ResourceType)

	assert.Nil(t, deps.LookupReads("undeclared_tool"), "an undeclared tool resolves to nil → coarse floor")

	assert.Equal(t, "ns1/sess1", deps.SessionRef, "SessionRef is threaded from SessionKey")
}
