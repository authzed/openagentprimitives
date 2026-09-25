package toolscmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

const describeToolkit = `
name: t
version: "1"
toolkitRevision: "r"
target: {binary: t}
parser: {kind: declarative}
env: {allowed: []}
subcommands:
  - path: [say]
    description: "Say a phrase"
    effects:
      destructive: false
      reads: [network]
      writes: []
      network: {destinations: [example.com]}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`

const describeSpec = `
name: describe-ex
version: "1"
intent: "a hand-written example"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
constraints:
  - cel: 'call.subcommand == "say"'
    message: "only the say subcommand is permitted"
`

func TestDescribe_Text(t *testing.T) {
	dir := t.TempDir()
	aptest.WriteFile(t, dir, "t.yaml", describeToolkit)
	spPath := aptest.WriteFile(t, dir, "s.yaml", describeSpec)
	code, stdout, _ := runToolspecCLI(t, "describe", spPath, "--toolkits", dir)
	require.Equalf(t, 0, code, "exit; stdout=%s", stdout)
	assert.Contains(t, stdout, "WHAT YOU CAN DO", "text output should contain header")
	assert.Contains(t, stdout, "only the say subcommand is permitted", "text output should include constraint message")
}

func TestDescribe_Markdown(t *testing.T) {
	dir := t.TempDir()
	aptest.WriteFile(t, dir, "t.yaml", describeToolkit)
	spPath := aptest.WriteFile(t, dir, "s.yaml", describeSpec)
	code, stdout, _ := runToolspecCLI(t, "describe", spPath, "--toolkits", dir, "--format", "markdown")
	require.Equalf(t, 0, code, "exit; stdout=%s", stdout)
	assert.Contains(t, stdout, "## What you can do", "markdown output should include heading")
}

func TestDescribe_MissingToolkit(t *testing.T) {
	dir := t.TempDir()
	spPath := aptest.WriteFile(t, dir, "s.yaml", describeSpec)
	// --toolkits points at empty dir
	code, _, _ := runToolspecCLI(t, "describe", spPath, "--toolkits", t.TempDir())
	assert.NotEqual(t, 0, code, "missing toolkit should yield non-zero exit")
}
