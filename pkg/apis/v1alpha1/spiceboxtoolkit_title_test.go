package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolkitEnvVarTitleRoundTrips(t *testing.T) {
	spec := SpiceboxToolkitSpec{
		Name:            "demo",
		Version:         "v0.1.0",
		ToolkitRevision: "2026-06-28",
		Target:          ToolkitTarget{Binary: "demo"},
		Parser:          ToolkitParserConfig{Kind: "declarative"},
		Env: ToolkitEnv{Allowed: []ToolkitEnvVar{
			{Name: "DEMO_TOKEN", Title: "Demo Service", Description: "a demo token", Sensitive: true, Credential: "demo-token"},
		}},
		Subcommands: []ToolkitSubcommand{},
	}
	tk, err := spec.ToToolkit()
	require.NoError(t, err, "ToToolkit must succeed")
	require.Len(t, tk.Env.Allowed, 1)
	assert.Equal(t, "Demo Service", tk.Env.Allowed[0].Title, "Title must survive the JSON round-trip into the library type")
	assert.Equal(t, "a demo token", tk.Env.Allowed[0].Description)
}
