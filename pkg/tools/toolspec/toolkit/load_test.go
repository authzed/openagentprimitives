package toolkit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadBytes_Valid(t *testing.T) {
	data := []byte(`
name: echo
version: "1"
toolkitRevision: "2026-04-24"
target: {binary: echo, versionRange: ">=0.0.0"}
parser: {kind: declarative}
env: {allowed: []}
subcommands:
  - path: [say]
    positional: [{name: msg, type: string, required: true}]
    flags: []
    effects:
      destructive: false
      reads: []
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`)
	tk, err := LoadBytes(data)
	require.NoError(t, err, "LoadBytes")
	assert.Equal(t, "echo", tk.Name, "Name")
}

func TestLoadBytes_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing name yields name-required error",
			yaml:    `version: "1"`,
			wantErr: "name is required",
		},
		{
			name:    "missing toolkitRevision yields revision-required error",
			yaml:    `name: x` + "\n" + `version: "1"`,
			wantErr: "toolkitRevision is required",
		},
		{
			name: "unknown parser kind yields parser.kind-must-be error",
			yaml: `
name: x
version: "1"
toolkitRevision: "r"
target: {binary: x}
parser: {kind: magical}
env: {allowed: []}
subcommands: []
`,
			wantErr: `parser.kind must be "declarative" or "builtin"`,
		},
		{
			name: "builtin parser without name yields parser.name-required error",
			yaml: `
name: x
version: "1"
toolkitRevision: "r"
target: {binary: x}
parser: {kind: builtin}
env: {allowed: []}
subcommands: []
`,
			wantErr: "parser.name is required when parser.kind is builtin",
		},
		{
			name: "creds.required references unknown env yields not-in-allowed error",
			yaml: `
name: x
version: "1"
toolkitRevision: "r"
target: {binary: x}
parser: {kind: declarative}
env: {allowed: [{name: FOO}]}
subcommands:
  - path: [a]
    effects:
      destructive: false
      reads: []
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [BAR], writes: []}
`,
			wantErr: "creds.required references BAR which is not in env.allowed",
		},
		{
			// optionalValue means "the value is only accepted attached with =",
			// which is meaningless on a flag that never carries a value. Loudly
			// rejecting the combination surfaces a mis-declared flag type.
			name: "optionalValue on a bool flag yields invalid-on-bool error",
			yaml: `
name: x
version: "1"
toolkitRevision: "r"
target: {binary: x}
parser: {kind: declarative}
env: {allowed: []}
subcommands:
  - path: [a]
    flags:
      - {long: verbose, type: bool, optionalValue: true}
    effects:
      destructive: false
      reads: []
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`,
			wantErr: `flag "verbose": optionalValue is invalid on type bool`,
		},
		{
			// The post-`--` tail binds starting at ONE slot; two candidates
			// would make the parser pick arbitrarily.
			name: "two afterDashDash positionals yield an ambiguity error",
			yaml: `
name: x
version: "1"
toolkitRevision: "r"
target: {binary: x}
parser: {kind: declarative}
env: {allowed: []}
subcommands:
  - path: [a]
    positional:
      - {name: first,  type: string, afterDashDash: true}
      - {name: second, type: string, afterDashDash: true}
    effects:
      destructive: false
      reads: []
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`,
			wantErr: `positionals "first" and "second" both set afterDashDash`,
		},
		{
			// Same rule on globalFlags, which validateFlags also covers.
			name: "optionalValue on a bool global flag yields invalid-on-bool error",
			yaml: `
name: x
version: "1"
toolkitRevision: "r"
target: {binary: x}
parser: {kind: declarative}
env: {allowed: []}
globalFlags:
  - {short: q, type: bool, optionalValue: true}
subcommands: []
`,
			wantErr: `flag "-q": optionalValue is invalid on type bool`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.yaml))
			require.Error(t, err, "expected error containing %q", tc.wantErr)
			assert.ErrorContains(t, err, tc.wantErr, "error message")
		})
	}
}

func TestLoadBytes_SensitiveEnvRequiresCredential(t *testing.T) {
	// A toolkit body parameterized by a single env.allowed entry; the rest
	// is the minimal valid shape so the only variable is the env var.
	body := func(envEntry string) string {
		return `
name: x
version: "1"
toolkitRevision: "r"
target: {binary: x}
parser: {kind: declarative}
env: {allowed: [` + envEntry + `]}
subcommands: []
`
	}
	cases := []struct {
		name     string
		envEntry string
		wantErr  string // "" means LoadBytes must succeed
	}{
		{
			name:     "sensitive env without credential: rejected, error names the env",
			envEntry: `{name: X, sensitive: true}`,
			wantErr:  `env "X"`,
		},
		{
			name:     "sensitive env with credential: accepted",
			envEntry: `{name: X, sensitive: true, credential: x}`,
		},
		{
			name:     "non-sensitive env without credential: accepted",
			envEntry: `{name: Y}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(body(tc.envEntry)))
			if tc.wantErr == "" {
				require.NoError(t, err, "LoadBytes must succeed")
				return
			}
			require.Error(t, err, "expected error containing %q", tc.wantErr)
			assert.ErrorContains(t, err, tc.wantErr, "error message")
		})
	}
}

func TestEnvVarProviderAndPromptRoundTrip(t *testing.T) {
	yaml := `
name: test-tk
version: "1"
toolkitRevision: "test"
target: { binary: foo, versionRange: ">=1" }
parser: { kind: declarative }
env:
  allowed:
    - { name: FOO_TOKEN, sensitive: true, provider: github-pat, credential: foo-token }
    - { name: BAR_KEY, sensitive: true, prompt: "see internal docs", credential: bar-key }
subcommands: []
`
	tk, err := LoadBytes([]byte(yaml))
	require.NoError(t, err, "LoadBytes")
	require.Len(t, tk.Env.Allowed, 2, "Env.Allowed length")
	assert.Equal(t, "github-pat", tk.Env.Allowed[0].Provider, "Env.Allowed[0].Provider")
	assert.Equal(t, "see internal docs", tk.Env.Allowed[1].Prompt, "Env.Allowed[1].Prompt")
}

func TestLoadBytes_SubcommandTimeout(t *testing.T) {
	// A toolkit body parameterized by a single subcommand timeout value; the
	// rest is the minimal valid shape so the only variable is the timeout.
	body := func(timeout string) string {
		return `
name: x
version: "1"
toolkitRevision: "r"
target: {binary: x}
parser: {kind: declarative}
env: {allowed: []}
subcommands:
  - path: [a]
    timeout: ` + timeout + `
    effects:
      destructive: false
      reads: []
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`
	}
	cases := []struct {
		name    string
		timeout string
		wantErr string // "" means LoadBytes must succeed
		wantDur string // expected parsed value on success
	}{
		{name: "valid 10m accepted", timeout: `"10m"`, wantDur: "10m"},
		{name: "valid 30s accepted", timeout: `"30s"`, wantDur: "30s"},
		{name: "unparseable timeout rejected", timeout: `"banana"`, wantErr: `timeout "banana"`},
		{name: "zero timeout rejected as non-positive", timeout: `"0s"`, wantErr: "must be positive"},
		{name: "negative timeout rejected", timeout: `"-5m"`, wantErr: "must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk, err := LoadBytes([]byte(body(tc.timeout)))
			if tc.wantErr != "" {
				require.Error(t, err, "expected error containing %q", tc.wantErr)
				assert.ErrorContains(t, err, tc.wantErr, "error message")
				return
			}
			require.NoError(t, err, "LoadBytes must succeed")
			require.Len(t, tk.Subcommands, 1, "Subcommands length")
			assert.Equal(t, tc.wantDur, tk.Subcommands[0].Timeout, "parsed Timeout string")
		})
	}
}

func TestLoadBytes_EmptyPathSubcommand(t *testing.T) {
	// Tools like cat and echo have no subcommands: the binary itself is the
	// operative command. path: [] must be accepted by the validator.
	data := []byte(`
name: cat
version: "1"
toolkitRevision: "1"
target: { binary: cat }
parser: { kind: declarative }
env: { allowed: [] }
subcommands:
  - path: []
    positional:
      - {name: files, type: stringList}
    effects:
      destructive: false
      reads: ["filesystem"]
      writes: []
      network: { destinations: [] }
      filesystem: { paths: [] }
      creds: { required: [], writes: [] }
`)
	tk, err := LoadBytes(data)
	require.NoError(t, err, "LoadBytes for empty-path subcommand")
	require.Len(t, tk.Subcommands, 1, "Subcommands length")
	assert.Empty(t, tk.Subcommands[0].Path, "Subcommands[0].Path should be empty")
}
