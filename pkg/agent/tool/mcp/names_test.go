package mcp_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

func TestModelVisible(t *testing.T) {
	cases := []struct {
		name       string
		visibility []string
		want       bool
	}{
		{name: "no visibility declared: the model's", visibility: nil, want: true},
		{name: `"model": the model's`, visibility: []string{"model"}, want: true},
		{name: `"app" and "model": still the model's`, visibility: []string{"app", "model"}, want: true},
		{name: `"app" alone: NOT the model's`, visibility: []string{"app"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mcp.ModelVisible(spiceboxv1alpha1.MCPServerTool{Visibility: tc.visibility})
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestLLMToolNames_MatchesWhatSynthesizeActuallyNames is the anti-drift guard
// for the prediction the steelthread capture depends on. LLMToolNames answers
// "what will these be called" from a spec alone; the moment it stops agreeing
// with Synthesize, a capture names tools the runner never offers, and the
// replay reports them as tools that vanished — a failure pointing at the wrong
// thing entirely.
//
// Both halves of the answer are exercised: which entries count (an app-only
// tool is not the model's, with or without the MCP-UI opt-in) and what each is
// called (prefix join plus normalization).
func TestLLMToolNames_MatchesWhatSynthesizeActuallyNames(t *testing.T) {
	cases := []struct {
		name      string
		crName    string
		appOptIn  bool
		tools     []spiceboxv1alpha1.MCPServerTool
		wantNames []string
	}{
		{
			name:   "plain allowlist under a prefix",
			crName: "k8s-mcp",
			tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "spicedb_pods"},
				{Name: "Get Alerts"},
			},
			wantNames: []string{"k8s-mcp_spicedb_pods", "k8s-mcp_get-alerts"},
		},
		{
			name:   "app-only tool with no opt-in: synthesized into neither set",
			crName: "box",
			tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "search"},
				{Name: "load_page", Visibility: []string{"app"}},
			},
			wantNames: []string{"box_search"},
		},
		{
			name:     "app-only tool WITH opt-in: an app tool, still not the model's",
			crName:   "box",
			appOptIn: true,
			tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "search"},
				{Name: "load_page", Visibility: []string{"app"}},
				{Name: "both", Visibility: []string{"app", "model"}},
			},
			wantNames: []string{"box_search", "box_both"},
		},
		{
			name:      "no prefix: the entry name stands alone",
			tools:     []spiceboxv1alpha1.MCPServerTool{{Name: "search"}},
			wantNames: []string{"search"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &spiceboxv1alpha1.MCPServer{}
			cr.Name = tc.crName
			cr.Spec.Server.URL = "https://x"
			cr.Spec.Tools = tc.tools
			if tc.appOptIn {
				cr.Spec.MCPUIAppTools = &spiceboxv1alpha1.MCPUIAppToolsSpec{Enabled: true}
			}
			live := make([]probe.Tool, 0, len(tc.tools))
			for _, tt := range tc.tools {
				live = append(live, probe.Tool{Name: tt.Name})
			}

			assert.Equal(t, tc.wantNames, mcp.LLMToolNames(cr), "predicted names")

			res, err := mcp.Synthesize(cr, live)
			require.NoError(t, err, "Synthesize")
			assert.Equal(t, tc.wantNames, toolNames(res.LLMTools),
				"the prediction must equal what Synthesize actually names")
		})
	}
}
