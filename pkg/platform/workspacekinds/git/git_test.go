package git_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"
)

func TestGit_SelfRegisters(t *testing.T) {
	// Importing the git package runs its init(), which registers "git".
	got, ok := registry.Get("git")
	require.True(t, ok, "git driver must self-register")
	assert.Equal(t, "git", got.Name())
}

func TestGit_Validate(t *testing.T) {
	cases := []struct {
		name    string
		spec    workspacekinds.Spec
		wantErr string // substring; "" means no error
	}{
		{
			name: "valid: locator + relative scope path → no error",
			spec: workspacekinds.Spec{
				Locator: "https://example.com/org/repo.git",
				Scope:   workspacekinds.Scope{Paths: []workspacekinds.PathRule{{Path: "src", Writable: true}}},
			},
			wantErr: "",
		},
		{
			name:    "missing locator → error",
			spec:    workspacekinds.Spec{Locator: "   "},
			wantErr: "locator is required",
		},
		{
			// A dash-leading locator is the CWE-88 shape: git would read it as
			// an option (--upload-pack=<cmd>) rather than a repository.
			name:    "dash-leading locator → rejected",
			spec:    workspacekinds.Spec{Locator: "--upload-pack=/bin/sh"},
			wantErr: "locator must not begin with '-'",
		},
		{
			name:    "dash-leading ref → rejected",
			spec:    workspacekinds.Spec{Locator: "https://example.com/o/r.git", Ref: "--upload-pack=/bin/sh"},
			wantErr: "ref must not begin with '-'",
		},
		{
			name: "absolute scope path → rejected",
			spec: workspacekinds.Spec{
				Locator: "file:///srv/repo",
				Scope:   workspacekinds.Scope{Paths: []workspacekinds.PathRule{{Path: "/etc"}}},
			},
			wantErr: "must be relative",
		},
		{
			name: "traversal scope path → rejected",
			spec: workspacekinds.Spec{
				Locator: "file:///srv/repo",
				Scope:   workspacekinds.Scope{Paths: []workspacekinds.PathRule{{Path: "a/../../etc"}}},
			},
			wantErr: `must not contain ".."`,
		},
		{
			name: "home scope path → rejected",
			spec: workspacekinds.Spec{
				Locator: "file:///srv/repo",
				Scope:   workspacekinds.Scope{Paths: []workspacekinds.PathRule{{Path: "~/.ssh"}}},
			},
			wantErr: "home directory",
		},
		{
			name: "leading whitespace on scope path → rejected",
			spec: workspacekinds.Spec{
				Locator: "file:///srv/repo",
				Scope:   workspacekinds.Scope{Paths: []workspacekinds.PathRule{{Path: " /etc"}}},
			},
			wantErr: "whitespace",
		},
		{
			name: "trailing whitespace hiding traversal → rejected",
			spec: workspacekinds.Spec{
				Locator: "file:///srv/repo",
				Scope:   workspacekinds.Scope{Paths: []workspacekinds.PathRule{{Path: ".. "}}},
			},
			wantErr: "whitespace",
		},
		{
			name: "backslash-hidden traversal → rejected",
			spec: workspacekinds.Spec{
				Locator: "file:///srv/repo",
				Scope:   workspacekinds.Scope{Paths: []workspacekinds.PathRule{{Path: `a\..\etc`}}},
			},
			wantErr: "backslash",
		},
	}
	k := git.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := k.Validate(tc.spec)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestGit_Validate_AggregatesAndSortsProblems(t *testing.T) {
	// A spec with two independent problems — missing locator AND an invalid
	// scope path — must surface both, sorted for a stable status message.
	err := git.New().Validate(workspacekinds.Spec{
		Locator: "",
		Scope:   workspacekinds.Scope{Paths: []workspacekinds.PathRule{{Path: "/etc"}}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "locator is required")
	assert.Contains(t, err.Error(), "must be relative")
}

func TestGit_EnvIsHermetic(t *testing.T) {
	// A stray ambient credential must not leak into the git env we build.
	t.Setenv("GITHUB_TOKEN", "super-secret")

	env := git.GitEnvForTest()

	joined := strings.Join(env, "\n")
	assert.Contains(t, joined, "GIT_TERMINAL_PROMPT=0", "interactive prompts disabled")
	assert.Contains(t, joined, "GIT_CONFIG_NOSYSTEM=1", "system git config ignored")
	assert.Contains(t, joined, "GIT_CONFIG_GLOBAL=/dev/null", "global/user git config ignored")
	assert.Contains(t, joined, "GIT_ALLOW_PROTOCOL=file:http:https", "transport allowlist restricted")
	assert.NotContains(t, joined, "super-secret", "ambient credentials must not leak into git env")
	assert.NotContains(t, joined, "GITHUB_TOKEN", "ambient credential vars must not leak into git env")
}

func TestGit_MaterializeCommandShape(t *testing.T) {
	k := git.New()

	cmds, err := k.MaterializeCommands(
		workspacekinds.Spec{Locator: "https://example.com/org/repo.git", Ref: "main"},
		"/base",
	)
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	assert.Equal(t, []string{
		"git", "clone", "--branch", "main", "--end-of-options",
		"https://example.com/org/repo.git", "/base",
	}, cmds[0].Argv, "clone argv pins ref and terminates options with --end-of-options")
	assert.Contains(t, strings.Join(cmds[0].Env, "\n"), "GIT_ALLOW_PROTOCOL=file:http:https")

	// No ref → no --branch flag.
	cmds, err = k.MaterializeCommands(
		workspacekinds.Spec{Locator: "file:///srv/repo"}, "/base")
	require.NoError(t, err)
	assert.Equal(t, []string{"git", "clone", "--end-of-options", "file:///srv/repo", "/base"}, cmds[0].Argv)

	// Invalid spec → error, no commands.
	bad, err := k.MaterializeCommands(workspacekinds.Spec{Locator: ""}, "/base")
	assert.Error(t, err, "invalid spec must not yield commands")
	assert.Nil(t, bad, "invalid spec yields nil commands")
}

func TestGit_SyncCommandShape(t *testing.T) {
	cmds, err := git.New().SyncCommands(
		workspacekinds.Spec{Locator: "file:///srv/repo"}, "/base")
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	assert.Equal(t, "/base", cmds[0].Dir, "sync runs in the overlay/base dir")
	assert.Equal(t, []string{"git", "pull", "--ff-only"}, cmds[0].Argv,
		"sync is fast-forward-only: a divergent upstream errors, never silently merges")
	assert.Contains(t, strings.Join(cmds[0].Env, "\n"), "GIT_ALLOW_PROTOCOL=file:http:https",
		"sync must carry the hermetic gitEnv")

	// invalid spec → error, no commands
	bad, err := git.New().SyncCommands(workspacekinds.Spec{Locator: ""}, "/base")
	assert.Error(t, err, "invalid spec must not yield commands")
	assert.Nil(t, bad, "invalid spec yields nil commands")
}

func TestGit_ApplyCommandShape(t *testing.T) {
	var k any = git.New()
	ap, ok := k.(workspacekinds.Applier)
	require.True(t, ok, "git driver must be an Applier")

	cmds, err := ap.ApplyCommands(
		workspacekinds.Spec{Locator: "https://example.com/o/r.git", Ref: "main"}, "/workspace")
	require.NoError(t, err)
	require.NotEmpty(t, cmds)
	last := cmds[len(cmds)-1]
	assert.Equal(t, "/workspace", last.Dir)
	require.GreaterOrEqual(t, len(last.Argv), 4, "argv must at least carry push/terminator/origin/dst")
	assert.Equal(t, []string{"push", "--end-of-options", "origin", "HEAD:main"}, last.Argv[len(last.Argv)-4:],
		"apply pushes the overlay's HEAD to the source branch, options terminated")
	assert.Contains(t, last.Argv, "credential.helper="+`!f() { echo username=x-access-token; echo "password=$WORKSPACE_GIT_TOKEN"; }; f`,
		"argv must wire the inline credential helper that reads the token from the WORKSPACE_GIT_TOKEN env")
	// ApplyCommands never receives a token value (its signature is spec+workDir
	// only) so structurally it cannot place one on argv; the helper references
	// the env var by NAME ($WORKSPACE_GIT_TOKEN), which git expands at push time
	// from the env the framework injects, never from these Commands.
	for _, a := range last.Argv {
		assert.NotContains(t, a, "ghp_", "argv must never carry a literal token value")
	}
	assert.Contains(t, strings.Join(last.Env, "\n"), "GIT_ALLOW_PROTOCOL=file:http:https",
		"apply carries the hermetic gitEnv (scoped credential injected by the framework, not here)")

	// no ref → push to HEAD's upstream default
	cmds, err = ap.ApplyCommands(workspacekinds.Spec{Locator: "https://example.com/o/r.git"}, "/workspace")
	require.NoError(t, err)
	last = cmds[len(cmds)-1]
	assert.Equal(t, []string{"push", "--end-of-options", "origin", "HEAD"}, last.Argv[len(last.Argv)-4:])

	// invalid spec → error
	_, err = ap.ApplyCommands(workspacekinds.Spec{Locator: ""}, "/workspace")
	assert.Error(t, err)
}

// TestGit_ApplyCredential pins the driver's credential contract: the env var
// name ApplyCommands' inline helper reads the token from, and the Secret key
// (in the session credential Secret) that holds it — the framework wires
// these two together via workspacekinds.CredentialedApplier, never on argv.
func TestGit_ApplyCredential(t *testing.T) {
	ca, ok := any(git.New()).(workspacekinds.CredentialedApplier)
	require.True(t, ok, "git driver must be a CredentialedApplier")
	envVar, secretKey := ca.ApplyCredential()
	assert.Equal(t, "WORKSPACE_GIT_TOKEN", envVar)
	assert.Equal(t, "git-token", secretKey)
}
