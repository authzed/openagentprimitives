package toolscmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

const testToolkit = `
name: t
version: "1"
toolkitRevision: "r"
target: {binary: t}
parser: {kind: declarative}
env: {allowed: []}
subcommands:
  - path: [say]
    description: "Say"
    effects:
      destructive: false
      reads: []
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`

const testSpecAllPass = `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
generation:
  source: "llm:fake"
  testCases:
    - intent: "say hello is allowed"
      argv: [say]
      expectAllow: true
    - intent: "foo is denied"
      argv: [foo]
      expectAllow: false
`

const testSpecMismatch = `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
generation:
  source: "llm:fake"
  testCases:
    - intent: "say is disallowed (WRONG expectation)"
      argv: [say]
      expectAllow: false
`

const testSpecNoCases = `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
`

func TestCLI_Test(t *testing.T) {
	cases := []struct {
		name         string
		spec         string
		wantNonzero  bool
		wantInStdout []string
	}{
		{
			name:         "all cases pass: exit 0, '2/2'",
			spec:         testSpecAllPass,
			wantInStdout: []string{"2/2"},
		},
		{
			name:         "expectation mismatch: non-zero exit, names failing case",
			spec:         testSpecMismatch,
			wantNonzero:  true,
			wantInStdout: []string{"say is disallowed"},
		},
		{
			name:         "no testCases: exit 0 with 'no test cases'",
			spec:         testSpecNoCases,
			wantInStdout: []string{"no test cases"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			aptest.WriteFile(t, dir, "t.yaml", testToolkit)
			spPath := aptest.WriteFile(t, dir, "s.yaml", tc.spec)
			code, stdout, _ := runToolspecCLI(t, "test", spPath, "--toolkits", dir)
			if tc.wantNonzero {
				require.NotEqualf(t, 0, code, "expected non-zero exit; stdout=%s", stdout)
			} else {
				require.Equalf(t, 0, code, "exit; stdout=%s", stdout)
			}
			for _, want := range tc.wantInStdout {
				assert.Contains(t, stdout, want, "stdout should contain")
			}
		})
	}
}
