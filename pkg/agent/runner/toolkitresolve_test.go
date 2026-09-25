package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func toolkitResolveScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

// customToolkitCR builds a minimal-but-valid SpiceboxToolkit CR that ToToolkit()
// round-trips — the shape an example agent's own CLI wrapper toolkit takes.
func customToolkitCR(name string) *spiceboxv1alpha1.SpiceboxToolkit {
	return &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            name,
			Version:         "1",
			ToolkitRevision: "2026-06-10",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: name + "-tool"},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{
				{Path: []string{"list-clusters"}, Description: "list", Permission: nil},
			},
		},
	}
}

func TestResolveToolkitForBundle(t *testing.T) {
	scheme := toolkitResolveScheme(t)

	t.Run("builtin resolves from the embedded catalog", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		tk, err := ResolveToolkitForBundle(context.Background(), c, "kubectl")
		require.NoError(t, err)
		require.NotNil(t, tk)
		assert.Equal(t, "kubectl", tk.Name)
	})

	t.Run("custom SpiceboxToolkit CR resolves when no builtin matches", func(t *testing.T) {
		// Regression: before the CR fallback, a non-builtin toolkit made the
		// runner exit ("did not resolve") → CrashLoopBackOff, blocking any
		// agent that uses its own CLI wrapper toolkit.
		cr := customToolkitCR("sre")
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
		tk, err := ResolveToolkitForBundle(context.Background(), c, "sre")
		require.NoError(t, err)
		require.NotNil(t, tk)
		assert.Equal(t, "sre", tk.Name)
		assert.Equal(t, "sre-tool", tk.Target.Binary)
	})

	t.Run("neither builtin nor CR: error names the missing toolkit", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		_, err := ResolveToolkitForBundle(context.Background(), c, "nonexistent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nonexistent")
		assert.Contains(t, err.Error(), "did not resolve")
	})
}

func TestToolkitAllowed(t *testing.T) {
	effWithToolkits := func(names []string) *spiceboxv1alpha1.EffectiveSettings {
		return &spiceboxv1alpha1.EffectiveSettings{AllowedToolkits: names}
	}
	cases := []struct {
		name        string
		eff         *spiceboxv1alpha1.EffectiveSettings
		toolkitName string
		want        bool
	}{
		// nil eff → unconstrained → allow all
		{
			name:        "nil eff: unconstrained → allow all",
			eff:         nil,
			toolkitName: "git",
			want:        true,
		},
		// nil AllowedToolkits (zero-value EffectiveSettings) → unconstrained → allow all
		{
			name:        "nil AllowedToolkits: unconstrained → allow all",
			eff:         &spiceboxv1alpha1.EffectiveSettings{},
			toolkitName: "git",
			want:        true,
		},
		// empty (non-nil) AllowedToolkits → allow none
		{
			name:        "empty AllowedToolkits: allow none",
			eff:         effWithToolkits([]string{}),
			toolkitName: "git",
			want:        false,
		},
		// non-empty AllowedToolkits containing the name → allow
		{
			name:        "non-empty AllowedToolkits containing name: allow",
			eff:         effWithToolkits([]string{"git", "github-cli"}),
			toolkitName: "git",
			want:        true,
		},
		// non-empty AllowedToolkits not containing the name → deny
		{
			name:        "non-empty AllowedToolkits not containing name: deny",
			eff:         effWithToolkits([]string{"github-cli"}),
			toolkitName: "git",
			want:        false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ToolkitAllowed(tc.eff, tc.toolkitName))
		})
	}
}
