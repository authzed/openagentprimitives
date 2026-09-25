// Package leadflow_test is J2 on the 2026-08-07 agent-UI leads-console plan:
// it proves the join between what a real leadflow server announces over the
// wire and what a browser ends up able to call, by driving the REAL probe
// client and the REAL synthesize + grant path against a live httptest
// server — never a hand-built stand-in for either side.
package leadflow_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	mcpdispatch "github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	"github.com/authzed/openagentprimitives/pkg/web/uidemo/leadflow"
	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
)

// probeLive starts the real go-sdk session handshake against url and
// returns what the server actually announces — the loopback seam
// test/e2e/inprocess_runner_factory.go already uses
// (mcpprobe.Client{HTTP: http.DefaultClient, ...}); see task-1-brief.md
// Step 1(b).
func probeLive(t *testing.T, url string) []mcpprobe.Tool {
	t.Helper()
	live, err := (&mcpprobe.Client{HTTP: http.DefaultClient, URL: url}).ListTools(t.Context(), "", "")
	require.NoError(t, err, "the fake must speak the go-sdk session handshake the real probe uses")
	return live
}

// crmServerCR builds the "deals" MCPServer CR an operator would author
// against a running leadflow instance. Visibility and Effects.ReadOnly are
// threaded straight from the live probe result (`live`) rather than
// hardcoded independently — Visibility is "a server-declared fact ... not a
// judgment", extended here to Effects.ReadOnly too. This is what makes the
// mutations below (which edit
// only leadflow's served tool.Meta/Annotations) actually reach the CR: a CR
// that hardcoded these fields independently of the announcement would make
// Synthesize's CR-truth-wins behavior invisible to a server-side mutation.
func crmServerCR(t *testing.T, url string, live []mcpprobe.Tool, appToolsEnabled bool) spiceboxv1alpha1.MCPServer {
	t.Helper()
	byName := make(map[string]mcpprobe.Tool, len(live))
	for _, lt := range live {
		byName[lt.Name] = lt
	}
	toolFor := func(name string, allowed []string) spiceboxv1alpha1.MCPServerTool {
		lt, ok := byName[name]
		require.Truef(t, ok, "leadflow must announce a %q tool", name)
		return spiceboxv1alpha1.MCPServerTool{
			Name:       name,
			Intent:     "demo CRM tool",
			Args:       spiceboxv1alpha1.MCPServerToolArgs{AllowedFields: allowed},
			Effects:    spiceboxv1alpha1.MCPServerToolEffects{ReadOnly: lt.Annotations.ReadOnlyHint},
			Visibility: lt.Annotations.Visibility,
			Permission: &authz.Permission{
				StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{
					ResourceType:       "demo_crm_pipeline",
					Permission:         "view",
					ResourceIDTemplate: "pipeline",
				},
			},
		}
	}

	cr := spiceboxv1alpha1.MCPServer{}
	cr.Name = "deals"
	cr.Namespace = "demo-ns"
	cr.Spec.Name = "deals"
	cr.Spec.Server.URL = url
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
		toolFor(leadflow.ToolListLeads, []string{"from", "to", "stage"}),
		toolFor(leadflow.ToolStageBreakdown, []string{"from", "to"}),
		toolFor(leadflow.ToolAdvanceLeadStage, []string{"leadId", "note"}),
	}
	if appToolsEnabled {
		cr.Spec.MCPUIAppTools = &spiceboxv1alpha1.MCPUIAppToolsSpec{Enabled: true}
	}
	return cr
}

// toolNames extracts Name() from a slice of tool.Tool, for order-insensitive
// membership assertions on a Synthesize/Materialize result.
func toolNames(ts []agenttool.Tool) []string {
	out := make([]string, len(ts))
	for i, tl := range ts {
		out[i] = tl.Name()
	}
	return out
}

// byToolName indexes a []tool.Tool by Name() — the shape
// runner.UIToolOptions and Loop.AppTools both key on.
func byToolName(ts []agenttool.Tool) map[string]agenttool.Tool {
	m := make(map[string]agenttool.Tool, len(ts))
	for _, tl := range ts {
		m[tl.Name()] = tl
	}
	return m
}

// TestAnnouncedSurfaceMaterializesAsBrowserCallableTools is J2. It starts a
// real leadflow server, probes it with the real client, and runs the real
// synthesize + grant path, so a disagreement between what the server
// announces and what the CR allowlists cannot pass.
func TestAnnouncedSurfaceMaterializesAsBrowserCallableTools(t *testing.T) {
	srv := httptest.NewServer(leadflow.New(leadflow.Options{}).Handler())
	t.Cleanup(srv.Close)

	live := probeLive(t, srv.URL)
	cr := crmServerCR(t, srv.URL, live, true)
	res, err := mcpdispatch.Synthesize(&cr, live, mcpdispatch.WithHTTPClient(http.DefaultClient))
	require.NoError(t, err, "Synthesize errors when the CR allowlists a tool the server does not expose")

	assert.ElementsMatch(t, []string{"deals_list_leads", "deals_stage_breakdown", "deals_advance_lead_stage"},
		toolNames(res.AppTools), "all three are app-only, so none may reach the LLM registry")
	assert.Empty(t, res.LLMTools, "an app-visible-and-not-model-visible tool must never be offered to the model")

	// UIToolOptions takes a map keyed by tool name, the same shape
	// Loop.AppTools carries.
	opts := runner.UIToolOptions(byToolName(res.AppTools))
	assert.True(t, opts.ReadonlyTools["deals_list_leads"])
	assert.True(t, opts.ReadonlyTools["deals_stage_breakdown"])
	assert.False(t, opts.ReadonlyTools["deals_advance_lead_stage"],
		"the mutation must fail the readonly predicate, or a data binding could name it")
}

// TestWithoutTheServerOptInAppOnlyToolsVanishEntirely is the reject-by-default
// half: without MCPUIAppTools.Enabled, an app-only tool is synthesized into
// NEITHER registry — it does not fall back to LLM-visible.
func TestWithoutTheServerOptInAppOnlyToolsVanishEntirely(t *testing.T) {
	srv := httptest.NewServer(leadflow.New(leadflow.Options{}).Handler())
	t.Cleanup(srv.Close)

	live := probeLive(t, srv.URL)
	cr := crmServerCR(t, srv.URL, live, false)
	res, err := mcpdispatch.Synthesize(&cr, live, mcpdispatch.WithHTTPClient(http.DefaultClient))
	require.NoError(t, err)

	assert.Empty(t, res.AppTools, "app-only tools are rejected-by-default without the server opt-in")
	assert.Empty(t, res.LLMTools, "app-only tools do not fall back to LLM-visible either")
}

// TestMaterializeAppToolsIsTheIntersection is the three-way grant, from the
// same real synthesized tools: requested (spec.tools), origins
// (runner.AppToolOrigin from the real AppTools), and granted (the
// AgentClass grant).
func TestMaterializeAppToolsIsTheIntersection(t *testing.T) {
	srv := httptest.NewServer(leadflow.New(leadflow.Options{}).Handler())
	t.Cleanup(srv.Close)

	live := probeLive(t, srv.URL)
	cr := crmServerCR(t, srv.URL, live, true)
	res, err := mcpdispatch.Synthesize(&cr, live, mcpdispatch.WithHTTPClient(http.DefaultClient))
	require.NoError(t, err)

	all := []string{"deals_list_leads", "deals_stage_breakdown", "deals_advance_lead_stage"}
	enabledOrigin := runner.AppToolOrigin(cr.Name, true, res.AppTools)
	disabledOrigin := runner.AppToolOrigin(cr.Name, false, res.AppTools)
	logCtx := runner.AppToolsLogContext{Session: "demo-ns/demo-session", AgentClass: "demo-agentclass", AgentUI: "demo-console"}

	cases := []struct {
		name      string
		requested []string
		origins   []uigrant.Origin
		granted   []string
		want      []string
	}{
		{
			name:      "all three requested, granted, and app-enabled: all three materialize",
			requested: all,
			origins:   []uigrant.Origin{enabledOrigin},
			granted:   all,
			want:      all,
		},
		{
			name:      "one absent from requested (AgentUI.spec.tools): materialize excludes it",
			requested: []string{"deals_list_leads", "deals_stage_breakdown"},
			origins:   []uigrant.Origin{enabledOrigin},
			granted:   all,
			want:      []string{"deals_list_leads", "deals_stage_breakdown"},
		},
		{
			name:      "one absent from granted (AgentClass.grantedTools): materialize excludes it",
			requested: all,
			origins:   []uigrant.Origin{enabledOrigin},
			granted:   []string{"deals_list_leads", "deals_stage_breakdown"},
			want:      []string{"deals_list_leads", "deals_stage_breakdown"},
		},
		{
			name:      "the origin opted out (mcpUiAppTools.enabled=false): materialize is empty",
			requested: all,
			origins:   []uigrant.Origin{disabledOrigin},
			granted:   all,
			want:      nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant := &spiceboxv1alpha1.AgentClassUIGrant{Ref: "demo-console", GrantedTools: tc.granted}
			got := runner.MaterializeAppTools(nil, logCtx, tc.requested, tc.origins, grant, res.AppTools)
			assert.ElementsMatch(t, tc.want, toolNames(got))
		})
	}
}
