package validator

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// gitScopedPositionalsSpecYAML guards git POSITIONALS: pushes are confined to
// refs/heads/agent/*, and checkout may only touch pathspecs under src/. Both
// guards use the `<name> in call.positional` idiom — the only way to write a
// positional guard that doesn't blow up as a CEL "no such key" error when the
// slot is unset, and therefore the shape a real spec author reaches for.
//
// That idiom is exactly what an argv trick has to beat: if a `--` or an
// optional-argument flag can knock the value out of call.positional, the guard
// short-circuits to true and the call sails through.
const gitScopedPositionalsSpecYAML = `
name: git-scoped
version: "1"
intent: "git restricted to agent-owned refs and src/ pathspecs"
toolkit:
  name: git
  revision: "2026-04-24"
allowSubcommands:
  - clone
  - push
  - checkout
  - log
constraints:
  - cel: "call.subcommand != 'push' || (!('refspec' in call.positional) || call.positional['refspec'].startsWith('refs/heads/agent/'))"
    message: "push only to refs/heads/agent/*"
  - cel: "call.subcommand != 'checkout' || (!('pathspec' in call.positional) || call.positional['pathspec'].all(p, p.startsWith('src/')))"
    message: "checkout only under src/"
`

// TestGitPositionalGuardsSurviveArgvTricks proves the two argv-level parse
// divergences that used to strip a positional out of call.positional cannot be
// used to slip past a positional constraint:
//
//   - `--` (the option terminator): its tokens are still positional arguments
//     the binary acts on, so they must bind to the declared slots.
//   - an optional-argument flag (`--force-with-lease`, git's PARSE_OPT_OPTARG):
//     git does not consume the following token, so neither may the parser.
//
// Each "…denies" case below was ALLOWED before the parser was fixed: the guard
// found no `refspec`/`pathspec` key and short-circuited to true.
func TestGitPositionalGuardsSurviveArgvTricks(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")
	tk, err := toolkit.Load(filepath.Join(repoRoot, "toolkits", "git.yaml"))
	require.NoError(t, err, "load git toolkit")
	sp, err := spec.LoadBytes([]byte(gitScopedPositionalsSpecYAML))
	require.NoError(t, err, "load git-scoped spec")

	cases := []struct {
		name      string
		argv      []string
		wantAllow bool
	}{
		{
			name:      "in-scope push allows",
			argv:      []string{"push", "origin", "refs/heads/agent/x"},
			wantAllow: true,
		},
		{
			name:      "out-of-scope push denies",
			argv:      []string{"push", "origin", "refs/heads/main"},
			wantAllow: false,
		},
		{
			name:      "optional-argument flag cannot shift the refspec out of scope: denies",
			argv:      []string{"push", "--force-with-lease", "origin", "refs/heads/main"},
			wantAllow: false,
		},
		{
			name:      "option terminator cannot hide the refspec: denies",
			argv:      []string{"push", "--", "origin", "refs/heads/main"},
			wantAllow: false,
		},
		{
			name:      "attached optional-argument value leaves positionals in place: allows",
			argv:      []string{"push", "--force-with-lease=refs/heads/agent/x", "origin", "refs/heads/agent/x"},
			wantAllow: true,
		},
		{
			name:      "in-scope push behind the option terminator allows",
			argv:      []string{"push", "--", "origin", "refs/heads/agent/x"},
			wantAllow: true,
		},
		{
			name:      "option terminator cannot hide a checkout pathspec: denies",
			argv:      []string{"checkout", "--", "/etc/passwd"},
			wantAllow: false,
		},
		{
			name:      "in-scope checkout pathspec behind the terminator allows",
			argv:      []string{"checkout", "--", "src/main.go"},
			wantAllow: true,
		},
		{
			name:      "terminator-protected clone URL binds to repository: allows",
			argv:      []string{"clone", "--", "https://example.invalid/o/r.git"},
			wantAllow: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Check(tk, sp, Invocation{Command: "git", Argv: tc.argv, Cwd: "/work"})
			require.NoError(t, err, "Check (internal error)")
			assert.Equal(t, tc.wantAllow, d.Allow, "decision allow; failedOn=%+v", d.FailedOn)
			if !tc.wantAllow {
				// Prove a positional CONSTRAINT denied it — not a parse error or
				// some unrelated phase, which would pass for the wrong reason.
				require.NotNil(t, d.FailedOn, "a denial must record FailedOn")
				assert.True(t, strings.HasPrefix(d.FailedOn.Path, "constraints"),
					"expected a constraints[*] denial, got %q", d.FailedOn.Path)
			}
		})
	}
}
