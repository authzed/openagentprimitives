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

// gitRWSpecYAML is the inner `.spec` of the `git-rw` SpiceboxToolspec, inlined
// so this test exercises the scoped-`-c` guard WITHOUT reading an on-disk spec
// file (tests must not depend on shipped spec files). Keep the scoped-`-c`
// constraint here in sync with the git-rw toolspec the operator ships — this
// is the guard under test.
const gitRWSpecYAML = `
name: git-rw
version: "1"
intent: "git for clone, status, diff, add, commit, push, branch"
toolkit:
  name: git
  revision: "2026-04-24"
allowSubcommands:
  - clone
  - status
  - diff
  - log
  - add
  - commit
  - push
  - branch
  - checkout
constraints:
  - cel: "call.subcommand != 'clone' || !call.positional['repository'].contains('@')"
    message: "Do not embed credentials in the clone URL."
  - cel: "!call.hasFlag('c') || call.flags['c'].all(v, v.startsWith('user.name=') || v.startsWith('user.email='))"
    message: "git -c may only set user.name= or user.email=."
sensitive:
  env:
    - GIT_TOKEN
`

// gitRWSpecFixture loads the inlined git-rw spec through the real spec loader,
// so the test drives the SHIPPED guard logic (the spec loader + CEL evaluation)
// over an inline fixture rather than an example file.
func gitRWSpecFixture(t *testing.T) *spec.Spec {
	t.Helper()
	sp, err := spec.LoadBytes([]byte(gitRWSpecYAML))
	require.NoError(t, err, "load git-rw spec")
	return sp
}

// TestGitCommitterIdentityGuard proves the scoped `git -c` flow: the toolkit
// admits the repeatable `-c <key>=<value>` global flag, and the git-rw guard
// allows ONLY user.name=/user.email= values (so the agent can set the
// committer) while rejecting any other key (which would be RCE / cred theft
// bypassing GIT_CONFIG_GLOBAL=/dev/null).
func TestGitCommitterIdentityGuard(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")
	tk, err := toolkit.Load(filepath.Join(repoRoot, "toolkits", "git.yaml"))
	require.NoError(t, err, "load git toolkit")
	sp := gitRWSpecFixture(t)

	cases := []struct {
		name      string
		argv      []string
		wantAllow bool
	}{
		{
			name:      "scoped identity: -c user.name + -c user.email on commit allows",
			argv:      []string{"-c", "user.name=Alice", "-c", "user.email=alice@example.com", "commit", "-m", "x"},
			wantAllow: true,
		},
		{
			name:      "single -c user.email on commit allows",
			argv:      []string{"-c", "user.email=alice@example.com", "commit", "-m", "x"},
			wantAllow: true,
		},
		{
			name:      "no -c on commit allows (guard short-circuits on absent flag)",
			argv:      []string{"commit", "-m", "x"},
			wantAllow: true,
		},
		{
			name:      "malicious -c core.sshCommand on commit denies",
			argv:      []string{"-c", "core.sshCommand=pwned", "commit", "-m", "x"},
			wantAllow: false,
		},
		{
			name:      "mixed identity + malicious -c on commit denies (every value must match)",
			argv:      []string{"-c", "user.name=Alice", "-c", "core.sshCommand=pwned", "commit", "-m", "x"},
			wantAllow: false,
		},
		{
			name:      "malicious -c credential.helper on commit denies",
			argv:      []string{"-c", "credential.helper=!sh -c pwn", "commit", "-m", "x"},
			wantAllow: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Check(tk, sp, Invocation{Command: "git", Argv: tc.argv})
			require.NoError(t, err, "Check (internal error)")
			assert.Equal(t, tc.wantAllow, d.Allow, "decision allow; failedOn=%+v", d.FailedOn)
			if !tc.wantAllow {
				// Prove it was the CEL guard that denied (parse + allowSubcommands
				// must have passed first), not some unrelated phase.
				require.NotNil(t, d.FailedOn, "a denial must record FailedOn")
				assert.True(t, strings.HasPrefix(d.FailedOn.Path, "constraints"),
					"expected a constraints[*] denial, got %q", d.FailedOn.Path)
			}
		})
	}
}
