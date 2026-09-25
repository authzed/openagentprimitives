package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping behavioral test")
	}
}

// initFixtureRepo creates a temp git repo with one commit and returns its path.
func initFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	authorEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = authorEnv
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("v1\n"), 0o644))
	run("add", "README.md")
	run("commit", "-m", "initial")
	return dir
}

// initBareRepo creates a bare git repo (a pushable remote) seeded from
// srcDir's current HEAD and returns its path.
func initBareRepo(t *testing.T, srcDir string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v (in %s): %s", args, dir, out)
	}
	run("", "init", "--bare", "-b", "main", bare)
	run(srcDir, "push", bare, "HEAD:main")
	return bare
}

// gitLogRefs returns the "git log --oneline" output for the given ref in the
// repo at gitDir (bare or non-bare, addressed via --git-dir).
func gitLogRefs(t *testing.T, gitDir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+gitDir, "log", "--oneline", ref)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git log %s in %s: %s", ref, gitDir, out)
	return string(out)
}

// runCommands executes the driver-produced commands and fails on any error.
func runCommands(t *testing.T, cmds []workspacekinds.Command) {
	t.Helper()
	for _, c := range cmds {
		cmd := exec.CommandContext(context.Background(), c.Argv[0], c.Argv[1:]...)
		cmd.Dir = c.Dir
		cmd.Env = c.Env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%v: %s", c.Argv, out)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	require.NoError(t, err, "read %s", p)
	return string(b)
}

func TestGit_MaterializeClonesSource(t *testing.T) {
	requireGit(t)
	src := initFixtureRepo(t)
	dest := filepath.Join(t.TempDir(), "base")

	cmds, err := git.New().MaterializeCommands(
		workspacekinds.Spec{Kind: "git", Locator: "file://" + src, Ref: "main"}, dest)
	require.NoError(t, err)
	runCommands(t, cmds)

	require.Equal(t, "v1\n", readFile(t, filepath.Join(dest, "README.md")),
		"materialized base must contain the source content")
}

// appendCommit adds/updates a file in the fixture repo's main branch.
func appendCommit(t *testing.T, repoDir, name, content string) {
	t.Helper()
	authorEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644))
	for _, args := range [][]string{{"add", name}, {"commit", "-m", "update " + name}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = authorEnv
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}

func TestGit_SyncFastForwardsToLatest(t *testing.T) {
	requireGit(t)
	src := initFixtureRepo(t)
	dest := filepath.Join(t.TempDir(), "base")
	spec := workspacekinds.Spec{Kind: "git", Locator: "file://" + src, Ref: "main"}

	mat, err := git.New().MaterializeCommands(spec, dest)
	require.NoError(t, err)
	runCommands(t, mat)
	require.Equal(t, "v1\n", readFile(t, filepath.Join(dest, "README.md")))

	appendCommit(t, src, "README.md", "v2\n")

	sync, err := git.New().SyncCommands(spec, dest)
	require.NoError(t, err)
	runCommands(t, sync)
	require.Equal(t, "v2\n", readFile(t, filepath.Join(dest, "README.md")),
		"sync must fast-forward the base to the new source revision")
}

func TestGit_ApplyPushesOverlayCommitToOrigin(t *testing.T) {
	requireGit(t)
	src := initFixtureRepo(t)
	// The bare repo is the pushable remote: apply pushes to "origin", and a
	// non-bare working-tree repo refuses a push to its checked-out branch.
	bare := initBareRepo(t, src)

	spec := workspacekinds.Spec{Kind: "git", Locator: "file://" + bare, Ref: "main"}

	// workDir is materialized from the bare origin, exactly as the shared
	// read-cache base would be — its "origin" remote already points at bare.
	workDir := filepath.Join(t.TempDir(), "overlay")
	mat, err := git.New().MaterializeCommands(spec, workDir)
	require.NoError(t, err)
	runCommands(t, mat)

	// Make a new commit in the overlay that does not yet exist upstream.
	appendCommit(t, workDir, "NOTES.md", "overlay edit\n")

	apply, err := git.New().ApplyCommands(spec, workDir)
	require.NoError(t, err)
	require.Len(t, apply, 1)
	assert.Equal(t, workDir, apply[0].Dir)
	// The argv now leads with an inline `-c credential.helper=` pair (the
	// framework-injected WORKSPACE_GIT_TOKEN auth path); it isn't invoked for
	// this file:// origin (no auth needed), but the trailing
	// push/--end-of-options/origin/dst still ends the argv the same way. The
	// runCommands below is what proves real git accepts the terminator here.
	require.GreaterOrEqual(t, len(apply[0].Argv), 4)
	assert.Equal(t, []string{"push", "--end-of-options", "origin", "HEAD:main"}, apply[0].Argv[len(apply[0].Argv)-4:])
	joinedEnv := strings.Join(apply[0].Env, "\n")
	assert.Contains(t, joinedEnv, "GIT_ALLOW_PROTOCOL=file:http:https",
		"apply must run with the hermetic gitEnv, not the ambient environment")

	runCommands(t, apply)

	log := gitLogRefs(t, bare, "main")
	assert.Contains(t, log, "update NOTES.md",
		"the overlay's new commit must land in the bare origin after apply")
}
