package registry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

func TestResolveBuiltin_Echo(t *testing.T) {
	r, err := registry.NewWithBuiltins(nil) // nil client = built-ins only
	require.NoError(t, err, "NewWithBuiltins")
	tk, err := r.Resolve(context.Background(), "echo", "2026-04-25")
	require.NoError(t, err, "Resolve echo")
	assert.Equal(t, "echo", tk.Name, "tk.Name")
}

func TestResolveBuiltin_NotFoundCases(t *testing.T) {
	cases := []struct {
		name     string
		toolkit  string
		revision string
	}{
		{name: "known toolkit with bogus revision returns ErrNotFound", toolkit: "echo", revision: "9999-99-99"},
		{name: "unknown toolkit name returns ErrNotFound", toolkit: "nonexistent", revision: "anything"},
	}
	r, err := registry.NewWithBuiltins(nil)
	require.NoError(t, err, "NewWithBuiltins")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Resolve(context.Background(), tc.toolkit, tc.revision)
			require.Error(t, err, "expected error")
			assert.ErrorIs(t, err, registry.ErrNotFound, "err should wrap ErrNotFound")
		})
	}
}

func TestBuiltinsExposed(t *testing.T) {
	r, err := registry.NewWithBuiltins(nil)
	require.NoError(t, err, "NewWithBuiltins")
	assert.GreaterOrEqual(t, len(r.Builtins()), 4, "Builtins() should expose >= 4 entries")
}

func TestResolveCR(t *testing.T) {
	scheme := apiruntime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")

	tk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-tool-1"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: "custom-tool", Version: "1", ToolkitRevision: "v1",
			Target: spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/custom"},
			Parser: spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env:    spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{}},
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{
				{
					Path: []string{},
					Effects: spiceboxv1alpha1.ToolkitEffects{
						Network:    spiceboxv1alpha1.ToolkitNetworkEffect{Destinations: []string{}},
						Filesystem: spiceboxv1alpha1.ToolkitFsEffect{Paths: []string{}},
						Creds:      spiceboxv1alpha1.ToolkitCredsEffect{Required: []string{}, Writes: []string{}},
					},
				},
			},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tk).Build()
	r, err := registry.NewWithBuiltins(cli)
	require.NoError(t, err, "NewWithBuiltins")
	got, err := r.Resolve(context.Background(), "custom-tool", "v1")
	require.NoError(t, err, "Resolve")
	assert.Equal(t, "custom-tool", got.Name, "got.Name")
}
