package toolscmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

func TestExplain_PrintsTrace(t *testing.T) {
	dir := t.TempDir()
	tk := aptest.WriteFile(t, dir, "tk.yaml", cliToolkit)
	sp := aptest.WriteFile(t, dir, "sp.yaml", cliSpec)
	code, stdout, _ := runToolspecCLI(t, "explain", "--toolkit", tk, "--spec", sp, "--", "t", "say", "hi")
	require.Equalf(t, 0, code, "exit; stdout=%s", stdout)
	assert.Contains(t, stdout, "parse", "trace should include 'parse' step")
	assert.Contains(t, stdout, "allowSubcommands", "trace should include 'allowSubcommands' step")
}

// secretEnvToolkit declares one sensitive env var, plus a subcommand whose
// only positional slot takes the post-`--` args. That shape is what puts the
// same value in TWO places on the Decision at once: bound into Parsed.Positional
// (which the validator redacts) and kept verbatim in Parsed.Tail.
const secretEnvToolkit = `
name: t
version: "1"
toolkitRevision: "r"
target: {binary: t}
parser: {kind: declarative}
env:
  allowed:
    - {name: DEMO_TOKEN, sensitive: true, credential: demo-cred, description: demo API token}
subcommands:
  - path: [say]
    positional: [{name: words, type: stringList, required: true, afterDashDash: true}]
    effects:
      destructive: false
      reads: []
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`

// denyAllSpec parses fine but allows no subcommand, so every invocation is
// denied — which is what makes `explain` print its "Parsed call:" block.
const denyAllSpec = `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: []
`

// TestExplain_RedactsASensitiveValueFromEveryPrintedField is the asymmetry
// test: `oap tools toolspec explain` already routes the env map through
// redactedEnv, and the validator already redacts Parsed.Flags/Positional
// before the Decision leaves its package — but Parsed.Tail is a second,
// verbatim copy of values that ALSO appear redacted two lines above it, and
// the parse-failure branch prints the raw invocation argv.
//
// The bar is one output, one rule: if the operator asked for a value to be
// treated as sensitive, no printed field may echo it. A trace that masks a
// token in `env:` and prints it in `tail:` is worse than one that masks
// neither, because it reads as if the redaction worked.
func TestExplain_RedactsASensitiveValueFromEveryPrintedField(t *testing.T) {
	const secret = "s3cr3t-demo-token"
	dir := t.TempDir()
	tk := aptest.WriteFile(t, dir, "tk.yaml", secretEnvToolkit)
	sp := aptest.WriteFile(t, dir, "sp.yaml", denyAllSpec)

	t.Run("parsed call: the tail must not echo what env already masked", func(t *testing.T) {
		code, stdout, _ := runToolspecCLI(t, "explain",
			"--toolkit", tk, "--spec", sp,
			"--env", "DEMO_TOKEN="+secret,
			"--", "t", "say", "--", secret)
		require.Equalf(t, 0, code, "explain itself must succeed on a deny; stdout=%s", stdout)
		require.Contains(t, stdout, "Parsed call:", "a denied call prints its parsed form")
		require.Contains(t, stdout, "tail:", "this fixture puts the value in the post-`--` tail")
		assert.NotContains(t, stdout, secret,
			"the value is declared sensitive; no printed field may echo it")
		assert.Contains(t, stdout, "<redacted", "it must be replaced by a redaction token, not dropped")
		// The positional slot is the one the AUDIT called already-safe. It is
		// not: the validator's scrub does walk Parsed.Positional, but its
		// recursive worker handles string / map[string]any / []any and returns
		// everything else untouched — and a stringList slot binds to []string,
		// so this value survived the walk. Pinned separately from the tail so a
		// future change cannot fix one and regress the other unnoticed.
		assert.Contains(t, stdout, `positional: map[words:[<redacted`,
			"a []string positional must be redacted too, not just a scalar one")
	})

	t.Run("parse did not complete: the raw argv must not echo it either", func(t *testing.T) {
		// An unknown subcommand fails the parse phase, so Parsed is nil and
		// explain falls back to printing inv.Argv — the invocation as typed,
		// which never passed through the validator's redactor.
		code, stdout, _ := runToolspecCLI(t, "explain",
			"--toolkit", tk, "--spec", sp,
			"--env", "DEMO_TOKEN="+secret,
			"--", "t", "no-such-subcommand", secret)
		require.Equalf(t, 0, code, "explain itself must succeed on a deny; stdout=%s", stdout)
		require.Contains(t, stdout, "Parsed call: (parse did not complete)")
		assert.NotContains(t, stdout, secret,
			"the raw argv is printed here and must be redacted like the env map beside it")
	})
}
