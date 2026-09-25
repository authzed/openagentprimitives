package toolscmd

import (
	"bytes"
	"fmt"
	"testing"
)

// runToolspecCLI executes the root cobra command with "toolspec" prepended
// to args. Used by tests for the sandbox-toolspec authoring helpers
// (explain/check/describe/test/gen) under `oap tools toolspec`.
func runToolspecCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := newToolsCmd(t)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(append([]string{"toolspec"}, args...))
	code := 0
	if err := cmd.Execute(); err != nil {
		code = 1
		fmt.Fprintln(&errb, err)
	}
	return code, out.String(), errb.String()
}

// cliToolkit and cliSpec are shared toolkit / spec YAML fixtures used by
// the sandbox-toolspec authoring command tests.
const cliToolkit = `
name: t
version: "1"
toolkitRevision: "r"
target: {binary: t}
parser: {kind: declarative}
env: {allowed: []}
subcommands:
  - path: [say]
    positional: [{name: msg, type: string, required: true}]
    effects:
      destructive: false
      reads: []
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`

const cliSpec = `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
`
