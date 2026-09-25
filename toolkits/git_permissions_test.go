package toolkits_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// git had ONE permission block — the toolkit-level passthrough default — so
// `git status`, `git commit` and `git push` were authorized identically, which
// is to say not at all. Observed on a live session where the user asked the
// agent to READ only: nothing in the authorization layer could have enforced
// that, and the plan gate never saw a single git call.
//
// Severity has to differ per subcommand or the distinction cannot exist
// anywhere downstream: the plan gate tiers on StateImpact, and a phase's
// ceiling is a set of handles.
func TestGitToolkit_severityDiffersBySubcommand(t *testing.T) {
	git := findToolkit(t, "git")

	cases := []struct {
		path       []string
		wantImpact authz.StateImpact
		wantPerm   string
	}{
		{[]string{"status"}, authz.Readonly, "read"},
		{[]string{"log"}, authz.Readonly, "read"},
		{[]string{"diff"}, authz.Readonly, "read"},
		{[]string{"add"}, authz.Readwrite, "write"},
		{[]string{"commit"}, authz.Readwrite, "write"},
		{[]string{"checkout"}, authz.Readwrite, "write"},
		// Leaves the machine. EXTERNAL is the top tier, so a phase carrying it
		// can never auto-approve — which is the whole point for push.
		{[]string{"push"}, authz.External, "push"},
	}
	for _, tc := range cases {
		t.Run(tc.path[0], func(t *testing.T) {
			sc := findSubcommand(t, git, tc.path)
			require.NotNil(t, sc.Permission,
				"%v must declare its own permission; inheriting the passthrough default is how push went ungated", tc.path)
			assert.Equal(t, tc.wantImpact, sc.Permission.StateImpact)
			require.NotNil(t, sc.Permission.Check, "%v needs a Check, or it mints no handle to plan against", tc.path)
			assert.Equal(t, "git_repo", sc.Permission.Check.ResourceType)
			assert.Equal(t, tc.wantPerm, sc.Permission.Check.Permission)
		})
	}
}

// Distinct permissions are what let a plan admit reads and exclude pushes. If
// they collapsed to one, a phase cleared for `git log` would also clear
// `git push`.
func TestGitToolkit_readAndPushAreDifferentHandles(t *testing.T) {
	git := findToolkit(t, "git")
	read := findSubcommand(t, git, []string{"status"})
	push := findSubcommand(t, git, []string{"push"})

	assert.NotEqual(t, read.Permission.Check.Permission, push.Permission.Check.Permission,
		"a ceiling admitting reads must not admit pushes")
}

// Every subcommand carrying a check needs a resolvable id, or the check fails
// before SpiceDB. A sandbox session has exactly ONE workspace, so the instance
// axis is degenerate and a literal sentinel is honest — it avoids making the
// remote URL a value slot, which needs transform-collision work that has not
// landed.
func TestGitToolkit_everyCheckHasAResolvableResourceID(t *testing.T) {
	git := findToolkit(t, "git")
	for i := range git.Subcommands {
		sc := &git.Subcommands[i]
		if sc.Permission == nil || sc.Permission.Check == nil {
			continue
		}
		// Either form resolves an id. The remote-naming subcommands use an
		// EXPR because they must also refuse a call that named no URL — a
		// template can only substitute, it cannot reject.
		assert.True(t,
			sc.Permission.Check.ResourceIDTemplate != "" || sc.Permission.Check.ResourceIDExpr != "",
			"%v: a check with neither an id template nor an expr cannot resolve", sc.Path)
		if sc.Permission.Check.ResourceIDExpr != "" {
			assert.NotEmpty(t, sc.Permission.Check.ResourceIDHint,
				"%v: an expr that can yield \"\" must tell the agent how to retry", sc.Path)
		}
	}
}

func findToolkit(t *testing.T, name string) *toolkit.Toolkit {
	t.Helper()
	for _, tk := range toolkits.All() {
		if tk.Name == name {
			return &tk
		}
	}
	t.Fatalf("toolkit %q not registered", name)
	return nil
}

func findSubcommand(t *testing.T, tk *toolkit.Toolkit, path []string) *toolkit.Subcommand {
	t.Helper()
	for i := range tk.Subcommands {
		sc := &tk.Subcommands[i]
		if len(sc.Path) == len(path) {
			match := true
			for j := range path {
				if sc.Path[j] != path[j] {
					match = false
					break
				}
			}
			if match {
				return sc
			}
		}
	}
	t.Fatalf("subcommand %v not found", path)
	return nil
}
