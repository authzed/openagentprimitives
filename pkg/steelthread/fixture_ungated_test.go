package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// RewriteFixture reports what dropping a gate let through, because the bundle's
// exemption from its own recorded tool catalog has to be DERIVED from the
// rewrite rather than restated. A transcribed list keeps excusing a tool long
// after the rewrite stops ungating it, and the whole point of pinning the
// catalog is to notice a gate that stopped withholding.
func TestRewriteFixture_UngatedTools(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*steelthread.FixtureInput)
		want   []string
	}{
		{
			name:   "a gated sidecar reports every LLM-facing tool the drop lets through",
			mutate: withSidecarToolbox,
			// Sorted, so a re-capture of one session is byte-identical.
			want: []string{"kube_nodes", "kube_pods"},
		},
		{
			name: "an UNgated sidecar reports nothing: it excuses nothing",
			mutate: func(f *steelthread.FixtureInput) {
				withSidecarToolbox(f)
				f.SidecarToolboxes[0].Spec.SecretInputs = nil
			},
			want: nil,
		},
		{
			name:   "no sidecars at all",
			mutate: func(*steelthread.FixtureInput) {},
			want:   nil,
		},
		{
			name: "an app-only tool is not reported: the model was never going to be offered it",
			mutate: func(f *steelthread.FixtureInput) {
				withSidecarToolbox(f)
				f.SidecarToolboxes[0].Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
					{Name: "pods"},
					{Name: "load_page", Visibility: []string{"app"}},
				}
			},
			want: []string{"kube_pods"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.RewriteFixture(liveFixture(t, tc.mutate))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.UngatedTools)
		})
	}
}

// The names must be the ones the runner will actually OFFER, not the raw
// allowlist entries: the replay compares against a catalog of LLM-facing names,
// so an un-prefixed name would excuse nothing and leave every ungated tool
// reported as an unexplained extra.
func TestRewriteFixture_UngatedToolsAreLLMFacingNames(t *testing.T) {
	got, err := steelthread.RewriteFixture(liveFixture(t, withSidecarToolbox))
	require.NoError(t, err)
	for _, n := range got.UngatedTools {
		assert.Contains(t, n, "kube_", "an ungated name must carry the sidecar's LLM prefix, got %q", n)
	}
}
