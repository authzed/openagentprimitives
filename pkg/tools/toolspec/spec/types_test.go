package spec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestUnmarshalSpec(t *testing.T) {
	data := []byte(`
name: gh-readonly
version: "1"
intent: "gh read-only"
toolkit: {name: gh, revision: "2026-04-24"}
require: {verifiedBinaryVersion: true}
allowSubcommands:
  - pr view
deny:
  effects:
    destructive: true
    writes: [network]
    creds: {writes: true}
allow:
  network:
    destinations: [api.github.com]
  filesystem:
    pathsUnder: [/work]
  creds:
    required: [GITHUB_TOKEN]
exceptions:
  - overrides: [deny.effects.destructive]
    when: 'call.subcommand == "pr merge"'
    message: "override"
constraints:
  - cel: 'call.flag("repo") == "x"'
    message: "bad repo"
sensitive:
  flags: []
  env: []
  positional: []
`)
	var sp Spec
	require.NoError(t, yaml.Unmarshal(data, &sp), "unmarshal")
	assert.Equal(t, "gh-readonly", sp.Name, "Name")
	assert.Equal(t, "2026-04-24", sp.Toolkit.Revision, "Toolkit.Revision")
	assert.True(t, sp.Require.VerifiedBinaryVersion, "Require.VerifiedBinaryVersion")
	assert.True(t, sp.Deny.Effects.Destructive, "Deny.Effects.Destructive")
	assert.Equal(t, []string{"network"}, sp.Deny.Effects.Writes, "Deny.Effects.Writes")
	assert.True(t, sp.Deny.Effects.Creds.Writes, "Deny.Effects.Creds.Writes")
	assert.Len(t, sp.Allow.Network.Destinations, 1, "Allow.Network.Destinations")
	require.Len(t, sp.Exceptions, 1, "Exceptions length")
	assert.Equal(t, "deny.effects.destructive", sp.Exceptions[0].Overrides[0], "Exceptions[0].Overrides[0]")
	require.Len(t, sp.Constraints, 1, "Constraints length")
	assert.Equal(t, `call.flag("repo") == "x"`, sp.Constraints[0].CEL, "Constraints[0].CEL")
}

func TestUnmarshalSpecWithSecretOutput(t *testing.T) {
	data := []byte(`
name: kubeconfig-gen
version: "1"
toolkit: {name: kubectl, revision: "2026-01-01"}
allowSubcommands: [config]
secretOutput:
  name: kubeconfig
  source: stdout
  description: "admin kubeconfig; expires 1h"
`)
	var sp Spec
	require.NoError(t, yaml.Unmarshal(data, &sp), "unmarshal")
	require.NotNil(t, sp.SecretOutput, "SecretOutput must be set")
	assert.Equal(t, "kubeconfig", sp.SecretOutput.Name, "SecretOutput.Name")
	assert.Equal(t, "stdout", sp.SecretOutput.Source, "SecretOutput.Source")
	assert.Equal(t, "admin kubeconfig; expires 1h", sp.SecretOutput.Description, "SecretOutput.Description")
}

func TestUnmarshalSpecWithGeneration(t *testing.T) {
	data := []byte(`
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
generation:
  source: "llm:gemini-3-flash-preview"
  generatedAt: "2026-04-24T15:00:00Z"
  warnings: ["example warning"]
  unmatched:
    - {request: "only on Tuesdays", reason: "no time-based constraints"}
  excluded:
    - {name: "shout", reason: "destructive"}
  descriptions:
    allowSubcommands[0]: "Say a phrase you name"
  testCases:
    - intent: "say hello is allowed"
      argv: [say, hello]
      expectAllow: true
    - intent: "shout hello is denied"
      argv: [shout, hello]
      expectAllow: false
      lastRunActual: false
`)
	var sp Spec
	require.NoError(t, yaml.Unmarshal(data, &sp), "unmarshal")
	require.NotNil(t, sp.Generation, "Generation")
	g := sp.Generation
	assert.Equal(t, "llm:gemini-3-flash-preview", g.Source, "Source")
	assert.Equal(t, []string{"example warning"}, g.Warnings, "Warnings")
	require.Len(t, g.Unmatched, 1, "Unmatched length")
	assert.Equal(t, "no time-based constraints", g.Unmatched[0].Reason, "Unmatched[0].Reason")
	require.Len(t, g.Excluded, 1, "Excluded length")
	assert.Equal(t, "shout", g.Excluded[0].Name, "Excluded[0].Name")
	assert.Equal(t, "Say a phrase you name", g.Descriptions["allowSubcommands[0]"], "Descriptions[allowSubcommands[0]]")
	require.Len(t, g.TestCases, 2, "TestCases length")
	tc := g.TestCases[1]
	assert.Equal(t, "shout hello is denied", tc.Intent, "TestCases[1].Intent")
	assert.False(t, tc.ExpectAllow, "TestCases[1].ExpectAllow")
	require.NotNil(t, tc.LastRunActual, "TestCases[1].LastRunActual must be non-nil")
	assert.False(t, *tc.LastRunActual, "TestCases[1].LastRunActual value")
}
