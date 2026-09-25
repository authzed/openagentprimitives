package spec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const minimalSpec = `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: []
`

func TestLoadBytes_Valid(t *testing.T) {
	sp, err := LoadBytes([]byte(minimalSpec))
	require.NoError(t, err, "LoadBytes")
	assert.Equal(t, "s", sp.Name, "Name")
	assert.NotNil(t, sp.AllowSubcommands, "AllowSubcommands should be non-nil even if empty")
}

func TestLoadBytes_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "missing name yields name-required error", yaml: `version: "1"`, wantErr: "name is required"},
		{name: "missing toolkit yields toolkit-name-required error", yaml: `name: x` + "\n" + `version: "1"`, wantErr: "toolkit.name is required"},
		{name: "missing allowSubcommands yields allowSubcommands-required error", yaml: `
name: x
version: "1"
toolkit: {name: t, revision: "r"}
`, wantErr: "allowSubcommands is required"},
		{name: "override references unknown rule path yields invalid-path error", yaml: `
name: x
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: []
exceptions:
  - overrides: [deny.effects.nonexistent]
    when: 'true'
`, wantErr: `exceptions[0].overrides[0]: "deny.effects.nonexistent" is not a valid overridable rule path`},
		{name: "override references non-overridable rule yields not-overridable error", yaml: `
name: x
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: []
exceptions:
  - overrides: [allowSubcommands]
    when: 'true'
`, wantErr: `"allowSubcommands" is not overridable`},
		{name: "secretOutput with empty name yields name-required error", yaml: `
name: x
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: []
secretOutput:
  name: ""
  source: stdout
`, wantErr: "secretOutput.name is required"},
		{name: "secretOutput with invalid source yields source-invalid error", yaml: `
name: x
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: []
secretOutput:
  name: kubeconfig
  source: pipe
`, wantErr: `secretOutput.source must be "stdout" or "file:<absolute-path>"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.yaml))
			require.Error(t, err, "expected error containing %q", tc.wantErr)
			assert.ErrorContains(t, err, tc.wantErr, "error message")
		})
	}
}

func TestLoadBytes_SecretOutput_Valid(t *testing.T) {
	cases := []struct {
		name       string
		yaml       string
		wantSource string
	}{
		{
			name: "stdout source is accepted",
			yaml: minimalSpec + `
secretOutput:
  name: kubeconfig
  source: stdout
  description: "admin config"
`,
			wantSource: "stdout",
		},
		{
			name: "file: source with absolute path is accepted",
			yaml: minimalSpec + `
secretOutput:
  name: token
  source: "file:/var/run/secrets/token"
`,
			wantSource: "file:/var/run/secrets/token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, err := LoadBytes([]byte(tc.yaml))
			require.NoError(t, err, "LoadBytes must succeed for valid secretOutput")
			require.NotNil(t, sp.SecretOutput, "SecretOutput must be set")
			assert.Equal(t, tc.wantSource, sp.SecretOutput.Source, "SecretOutput.Source")
		})
	}
}

func TestLoadBytes_AllowSetTracking(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSet bool
	}{
		{
			name:    "destinations absent: Allow.Network.Set=false",
			yaml:    minimalSpec,
			wantSet: false,
		},
		{
			name: "destinations present but empty: Allow.Network.Set=true",
			yaml: minimalSpec + `
allow:
  network:
    destinations: []
`,
			wantSet: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, err := LoadBytes([]byte(tc.yaml))
			require.NoError(t, err, "LoadBytes")
			assert.Equal(t, tc.wantSet, sp.Allow.Network.Set, "Allow.Network.Set")
		})
	}
}

func TestLoadBytes_GenerationIgnoredByValidator(t *testing.T) {
	// A spec with an entirely bogus generation block should still load + validate.
	data := []byte(`
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
generation:
  source: "nonsense"
  warnings: ["this is LLM-authored"]
`)
	sp, err := LoadBytes(data)
	require.NoError(t, err, "LoadBytes")
	require.NotNil(t, sp.Generation, "Generation should round-trip")
	assert.Equal(t, "nonsense", sp.Generation.Source, "Generation.Source")
}
