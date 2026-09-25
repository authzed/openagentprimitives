package sidecartoolbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// TestLLMToolNames_MatchesWhatSynthesizeActuallyNames is the anti-drift guard
// for the prediction the steelthread capture depends on.
//
// The capture names the tools its own fixture rewrite ungates, from the
// resolved snapshot alone, with no sidecar to probe. If that prediction ever
// disagrees with what Synthesize names — a changed prefix, a visibility rule
// applied on one side only — the bundle excuses tools the runner never offers
// AND stops excusing the ones it does, so the replay reports a regression that
// is really a naming bug.
func TestLLMToolNames_MatchesWhatSynthesizeActuallyNames(t *testing.T) {
	cases := []struct {
		name  string
		rt    spiceboxv1alpha1.ResolvedSidecarToolbox
		want  []string
		build bool // also assert against the real Synthesize
	}{
		{
			name: "the prefix is the toolbox's ref-name, not its spec name",
			rt: spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name: "k8s-mcp",
				Ref:  "k8s-mcp",
				Port: 19999,
				Spec: spiceboxv1alpha1.SidecarToolboxSpec{
					Name:  "an-unrelated-display-name",
					Tools: []spiceboxv1alpha1.MCPServerTool{{Name: "spicedb_pods"}, {Name: "get_alerts"}},
				},
			},
			want:  []string{"k8s-mcp_spicedb_pods", "k8s-mcp_get_alerts"},
			build: true,
		},
		{
			name: "an app-only tool is never the model's: sidecars do not opt into the split",
			rt: spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name: "box",
				Ref:  "box",
				Port: 19999,
				Spec: spiceboxv1alpha1.SidecarToolboxSpec{
					Tools: []spiceboxv1alpha1.MCPServerTool{
						{Name: "search"},
						{Name: "load_page", Visibility: []string{"app"}},
					},
				},
			},
			want:  []string{"box_search"},
			build: true,
		},
		{
			name: "no allowlist: no names, and nothing to excuse",
			rt: spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name: "empty", Ref: "empty", Port: 19999,
			},
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, LLMToolNames(tc.rt), "predicted names")
			if !tc.build {
				return
			}
			live := make([]probe.Tool, 0, len(tc.rt.Spec.Tools))
			for _, tt := range tc.rt.Spec.Tools {
				live = append(live, probe.Tool{Name: tt.Name, InputSchema: []byte(`{"type":"object"}`)})
			}
			tools, err := Synthesize(tc.rt, live, nil)
			require.NoError(t, err, "Synthesize")
			got := make([]string, 0, len(tools))
			for _, tt := range tools {
				got = append(got, tt.Name())
			}
			assert.Equal(t, tc.want, got, "the prediction must equal what Synthesize actually names")
		})
	}
}

// TestSynthCR_IsTheSameCRBothPathsUse pins the reason synthCR exists: naming
// depends on metadata.Name and the allowlist, and both callers must read them
// off one construction. The URL is the only thing that differs between them,
// and it has no bearing on a name.
func TestSynthCR_IsTheSameCRBothPathsUse(t *testing.T) {
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "k8s-mcp", Ref: "k8s-mcp", Port: 19999,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:  "display",
			Tools: []spiceboxv1alpha1.MCPServerTool{{Name: "pods"}},
		},
	}
	withURL := synthCR(rt, "http://127.0.0.1:19999")
	withoutURL := synthCR(rt, "")

	assert.Equal(t, withURL.Name, withoutURL.Name)
	assert.Equal(t, withURL.Spec.Tools, withoutURL.Spec.Tools)
	assert.Equal(t, "k8s-mcp", withURL.Name, "the LLM prefix is the ref-name the AgentClass author chose")
}
