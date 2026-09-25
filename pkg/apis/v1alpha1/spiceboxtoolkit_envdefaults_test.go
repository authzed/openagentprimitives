package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolkitEnvDefaultsRoundTrips pins the CR half of toolkit.Toolkit's
// envDefaults. ToToolkit is a JSON round-trip, so a field the CR spec does not
// declare is not "unset" — it is unrepresentable: a custom SpiceboxToolkit
// could never declare the hardening env a built-in toolkit YAML can.
func TestToolkitEnvDefaultsRoundTrips(t *testing.T) {
	spec := SpiceboxToolkitSpec{
		Name:            "demo",
		Version:         "v0.1.0",
		ToolkitRevision: "2026-08-25",
		Target:          ToolkitTarget{Binary: "demo"},
		Parser:          ToolkitParserConfig{Kind: "declarative"},
		Env:             ToolkitEnv{Allowed: []ToolkitEnvVar{}},
		EnvDefaults:     map[string]string{"DEMO_MAX_RETRIES": "1"},
		Subcommands:     []ToolkitSubcommand{},
	}
	tk, err := spec.ToToolkit()
	require.NoError(t, err, "ToToolkit must succeed")
	assert.Equal(t, map[string]string{"DEMO_MAX_RETRIES": "1"}, tk.EnvDefaults,
		"envDefaults must survive the JSON round-trip into the library type")
}

// TestToolkitEnvDefaultsRejectsCredentialShadow pins that the library's
// load-time refusal reaches CR-sourced toolkits too: a static default naming a
// sensitive env var would either fail every call with ToolCallEnvShadowsAgent
// or hand the CLI a fixed string in place of the credential.
func TestToolkitEnvDefaultsRejectsCredentialShadow(t *testing.T) {
	spec := SpiceboxToolkitSpec{
		Name:            "demo",
		Version:         "v0.1.0",
		ToolkitRevision: "2026-08-25",
		Target:          ToolkitTarget{Binary: "demo"},
		Parser:          ToolkitParserConfig{Kind: "declarative"},
		Env: ToolkitEnv{Allowed: []ToolkitEnvVar{
			{Name: "DEMO_TOKEN", Sensitive: true, Credential: "demo-token"},
		}},
		EnvDefaults: map[string]string{"DEMO_TOKEN": "not-a-real-secret"},
		Subcommands: []ToolkitSubcommand{},
	}
	_, err := spec.ToToolkit()
	require.Error(t, err, "ToToolkit must refuse a default that shadows a credential")
	assert.ErrorContains(t, err, "DEMO_TOKEN", "error must name the offending key")
}
