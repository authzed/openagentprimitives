package mcp_test

import (
	"context"
	"net/http"
	"testing"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolNames extracts Name() from a slice of tool.Tool, for order-insensitive
// membership assertions on a Synthesize split result.
func toolNames(tools []agenttool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return names
}

func TestSynthesize_BuildsToolPerEntry(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "linear"
	cr.Spec.Server.URL = "https://x"
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
		{Name: "search_issues", Intent: "find them", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
		{Name: "DELETE-issue", Intent: "remove", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
	}
	live := []probe.Tool{
		{Name: "search_issues", Description: "server-side desc"},
		{Name: "DELETE-issue"},
	}
	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")
	tools := res.LLMTools
	require.Len(t, tools, 2, "tools")
	assert.Equal(t, "linear_search_issues", tools[0].Name())
	assert.Equal(t, "linear_delete-issue", tools[1].Name(), "name should be lowercased and dashes preserved")
	assert.Contains(t, tools[0].Description(), "find them", "intent should appear in description")
	assert.Contains(t, tools[0].Description(), "server-side desc", "server-side desc should appear in description")
	assert.Empty(t, res.AppTools, "no app-visible tools declared; AppTools must be empty")
}

func TestSynthesize_AllowlistDriftFails(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "linear"
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{{Name: "missing"}}
	_, err := mcp.Synthesize(cr, nil)
	require.Error(t, err, "expected drift error")
	assert.Contains(t, err.Error(), "missing", "error should name the missing tool")
}

// TestSynthesize_SplitsAppVisibleTools is the core Task-4 routing test: with
// MCPUIAppTools.Enabled, a tool visible ONLY to "app" (and not "model") is
// routed into AppTools and withheld from LLMTools; a tool with no visibility
// (default both) or with visibility including "model" stays in LLMTools; a
// tool visible to both "app" and "model" stays LLM-visible (and does NOT also
// land in AppTools — Phase A's split is exclusive, not a fan-out).
func TestSynthesize_SplitsAppVisibleTools(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	// Name left unset (empty prefix) so synthesized tool names are unprefixed,
	// keeping the assertions below on the raw entry names.
	cr.Spec.Server.URL = "https://x"
	cr.Spec.MCPUIAppTools = &spiceboxv1alpha1.MCPUIAppToolsSpec{Enabled: true}
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
		{Name: "search"},
		{Name: "load_page", Visibility: []string{"app"}},
		{Name: "both_tool", Visibility: []string{"app", "model"}},
	}
	live := []probe.Tool{
		{Name: "search"},
		{Name: "load_page"},
		{Name: "both_tool"},
	}

	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")

	llmNames := toolNames(res.LLMTools)
	appNames := toolNames(res.AppTools)

	assert.Contains(t, llmNames, "search", "no-visibility tool defaults to LLM-visible")
	assert.Contains(t, llmNames, "both_tool", "app+model visibility stays LLM-visible")
	assert.NotContains(t, llmNames, "load_page", "app-only tool must NOT be LLM-visible")

	assert.Contains(t, appNames, "load_page", "app-only tool must land in AppTools")
	assert.NotContains(t, appNames, "search", "no-visibility tool must not also be an app tool")
	assert.NotContains(t, appNames, "both_tool", "app+model tool must not also land in AppTools")
}

// TestSynthesize_AppVisibleTool_RejectedWithoutOptIn verifies reject-by-default:
// with MCPUIAppTools unset (nil), an app-only tool is not synthesized into
// EITHER set — it is simply absent, exactly as if it were never declared.
func TestSynthesize_AppVisibleTool_RejectedWithoutOptIn(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Spec.Server.URL = "https://x"
	// cr.Spec.MCPUIAppTools left nil: no opt-in.
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
		{Name: "search"},
		{Name: "load_page", Visibility: []string{"app"}},
	}
	live := []probe.Tool{
		{Name: "search"},
		{Name: "load_page"},
	}

	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")

	llmNames := toolNames(res.LLMTools)
	assert.Contains(t, llmNames, "search", "no-visibility tool still synthesized")
	assert.NotContains(t, llmNames, "load_page", "app-only tool must not be LLM-visible without opt-in")
	assert.Empty(t, res.AppTools, "app-only tool must not be synthesized at all without opt-in")
}

func TestSynthesize_PermissionFromMCPServerTool(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "linear"
	cr.Spec.Server.URL = "https://x"
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
		{
			Name: "create_issue",
			Permission: &authz.Permission{
				StateImpact: authz.Readwrite,
				Check: &authz.PermissionCheck{
					ResourceType:       "linear_team",
					ResourceIDTemplate: "{teamId}",
					Permission:         "write",
				},
			},
		},
	}
	live := []probe.Tool{
		{Name: "create_issue", Description: "Create an issue"},
	}
	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")
	tools := res.LLMTools
	require.Len(t, tools, 1, "tools")

	got := tools[0].Permission()
	assert.Equal(t, authz.Readwrite, got.StateImpact)
	require.NotNil(t, got.Check, "Check should be set")
	assert.Equal(t, "write", got.Check.Permission)
}

// TestSynthesize_OriginKeysOnCRName_NotLLMPrefix is the regression guard for
// the in-flight MCP revoke no-op: the runner overwrites the fetched CR's
// metadata.name with the AgentClass LLM-prefix before synthesizing (so the
// LLM-facing tool names carry that prefix), but every revoke publisher and the
// runner's restart filter key on the REAL CR name. If Origin() keyed on the
// (overwritten) serverName/LLM-prefix instead of the CR name, the guard's
// revoked set ("mcpserver/<crName>") would never match the tool's Origin
// ("mcpserver/<llmPrefix>") and revocation would silently never fire.
//
// This reproduces the runner's exact pattern: real CR name "mcp-github",
// overwrite a copy's Name to the LLM-prefix "gh", and pass
// WithOriginName(realCRName).
func TestSynthesize_OriginKeysOnCRName_NotLLMPrefix(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "mcp-github" // the real MCPServer metadata.name (== AgentClass ref.Ref)
	cr.Spec.Server.URL = "https://x"
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{{Name: "list_repos"}}
	live := []probe.Tool{{Name: "list_repos", Description: "list repos"}}

	// Mirror internal/cmd/runner/main.go: copy the CR and overwrite metadata.name with
	// the AgentClass LLM-prefix, then pass the REAL CR name as the origin.
	cr2 := *cr
	cr2.Name = "gh" // LLM-prefix from AgentClass.MCPServers[].Name (ref.Name)
	res, err := mcp.Synthesize(&cr2, live, mcp.WithOriginName(cr.Name))
	require.NoError(t, err, "Synthesize")
	tools := res.LLMTools
	require.Len(t, tools, 1, "tools")

	// LLM-facing name still carries the prefix...
	assert.Equal(t, "gh_list_repos", tools[0].Name(), "LLM name keeps the LLM-prefix")

	// ...but the revocation Origin must key on the CR name, matching the
	// revoke publishers' "mcpserver/"+crName.
	ot, ok := tools[0].(agenttool.OriginTool)
	require.True(t, ok, "MCPTool must implement OriginTool")
	assert.Equal(t, "mcpserver/mcp-github", ot.Origin(),
		"Origin must key on the CR name, not the LLM-prefix; otherwise revocation is a silent no-op when name != ref")
	assert.NotEqual(t, "mcpserver/gh", ot.Origin(),
		"Origin must NOT key on the LLM-prefix")
}

// TestSynthesize_OriginDefaultsToCRName guards the default path: callers that
// pass an un-mangled CR (no WithOriginName) get Origin() == "mcpserver/"+cr.Name
// automatically.
func TestSynthesize_OriginDefaultsToCRName(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "mcp-github"
	cr.Spec.Server.URL = "https://x"
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{{Name: "list_repos"}}
	live := []probe.Tool{{Name: "list_repos", Description: "list repos"}}

	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")
	tools := res.LLMTools
	require.Len(t, tools, 1, "tools")

	ot, ok := tools[0].(agenttool.OriginTool)
	require.True(t, ok, "MCPTool must implement OriginTool")
	assert.Equal(t, "mcpserver/mcp-github", ot.Origin(),
		"Origin defaults to mcpserver/<cr.Name> when WithOriginName is not passed")
}

// TestSynthesize_ServerReadOnlyHint verifies the synthesized tool exposes the
// MCPServer CR's Effects.ReadOnly via ServerReadOnlyHint() — the server half of
// the MCP-UI app-tool readonly gate (D-B3). Set on every synthesized tool
// (LLM- and app-visible alike); the gate only ever consults it for app tools.
func TestSynthesize_ServerReadOnlyHint(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Spec.Server.URL = "https://x"
	cr.Spec.MCPUIAppTools = &spiceboxv1alpha1.MCPUIAppToolsSpec{Enabled: true}
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
		{Name: "reader", Visibility: []string{"app"}, Effects: spiceboxv1alpha1.MCPServerToolEffects{ReadOnly: true}},
		{Name: "writer", Visibility: []string{"app"}, Effects: spiceboxv1alpha1.MCPServerToolEffects{ReadOnly: false}},
	}
	live := []probe.Tool{{Name: "reader"}, {Name: "writer"}}

	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")
	require.Len(t, res.AppTools, 2, "both app-visible tools synthesized")

	byName := map[string]agenttool.Tool{}
	for _, tl := range res.AppTools {
		byName[tl.Name()] = tl
	}

	hint := func(tl agenttool.Tool) bool {
		h, ok := tl.(interface{ ServerReadOnlyHint() bool })
		require.True(t, ok, "synthesized MCPTool must expose ServerReadOnlyHint")
		return h.ServerReadOnlyHint()
	}
	assert.True(t, hint(byName["reader"]), "Effects.ReadOnly=true must surface as ServerReadOnlyHint()=true")
	assert.False(t, hint(byName["writer"]), "Effects.ReadOnly=false must surface as ServerReadOnlyHint()=false")
}

func TestSynthesize_PermissionZeroWhenNotSet(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "linear"
	cr.Spec.Server.URL = "https://x"
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
		{Name: "search_issues"},
	}
	live := []probe.Tool{
		{Name: "search_issues", Description: "Search issues"},
	}
	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")
	tools := res.LLMTools

	got := tools[0].Permission()
	assert.Empty(t, got.StateImpact, "expected zero Permission.StateImpact")
	assert.Nil(t, got.Check, "expected zero Permission.Check")
}

// TestSynthesize_WiresObservesFromCR proves the CR's `observes` blocks reach
// the synthesized tool through Synthesize's OWN struct literal
// (`observes: t.Observes` in synthesize.go) — the actual production wiring
// path. internal/cmd/runner never calls MCPTool.SetObserves (same as
// SetWritesRelationships: zero non-test callers — see SetObserves's doc
// comment), so a test that only drives SetObserves proves nothing about
// whether a real CR's declared observes block ever reaches a real dispatch.
// This test deliberately never calls SetObserves — only Synthesize and
// SetMemory (the latter IS how internal/cmd/runner wires memory, at session
// start, so it's legitimate setup here, not the thing under test).
func TestSynthesize_WiresObservesFromCR(t *testing.T) {
	srv, _ := newTextToolServer(t, "search_issues", prPayload, false)

	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "linear"
	cr.Spec.Server.URL = srv.URL
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{{
		Name:       "search_issues",
		Permission: &authz.Permission{StateImpact: authz.Passthrough},
		Observes:   prObserves(),
	}}
	live := []probe.Tool{{Name: "search_issues"}}
	// The loopback httptest server is refused by the production SSRF-guarded
	// client — same override mustSynthesize uses.
	res, err := mcp.Synthesize(cr, live, mcp.WithHTTPClient(http.DefaultClient))
	require.NoError(t, err, "Synthesize")
	require.Len(t, res.LLMTools, 1)
	mt := res.LLMTools[0].(*mcp.MCPTool)

	mem, scope := newObserveMemory(t, mt)

	_, opID, sess := newOpAndSess(t)
	execRes, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")
	require.False(t, execRes.IsError, "unexpected IsError; content=%q", execRes.Content)

	pr, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, pr,
		"Synthesize must wire the CR's observes block onto the tool it builds, with no SetObserves call anywhere in this test")
}
