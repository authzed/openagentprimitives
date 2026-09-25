package skillfetch

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hexSHA matches a full 40-char hex commit SHA.
var hexSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// buildLocalRepo creates a real on-disk git repo in a temp dir, commits the
// given files with a FIXED author signature (so the test is deterministic), and
// returns the repo dir. We use a real on-disk repo rather than the in-memory
// storer because go-git's HTTP/file transports clone over the wire from a path;
// a "file://" URL pointed at this dir is fetchable with no network access.
func buildLocalRepo(t *testing.T, branch string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err, "PlainInit")

	wt, err := repo.Worktree()
	require.NoError(t, err, "Worktree")

	for name, content := range files {
		f, err := wt.Filesystem.Create(name)
		require.NoError(t, err, "create %s", name)
		_, err = f.Write([]byte(content))
		require.NoError(t, err, "write %s", name)
		require.NoError(t, f.Close(), "close %s", name)
		_, err = wt.Add(name)
		require.NoError(t, err, "add %s", name)
	}

	// Fixed signature keeps the commit (and thus its SHA, given fixed content)
	// deterministic across runs and machines.
	sig := &object.Signature{
		Name:  "Alice",
		Email: "alice@example.com",
		When:  time.Unix(1700000000, 0).UTC(),
	}
	_, err = wt.Commit("seed", &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err, "Commit")

	// If a non-default branch was requested, point a branch ref at HEAD so a
	// by-ref fetch has something to resolve.
	if branch != "" {
		head, err := repo.Head()
		require.NoError(t, err, "Head")
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), head.Hash())
		require.NoError(t, repo.Storer.SetReference(ref), "set branch ref")
	}

	return dir
}

func TestGoGitFetchClonesDefaultBranch(t *testing.T) {
	repoDir := buildLocalRepo(t, "", map[string]string{
		"skills/x/SKILL.md": "---\nname: x\ndescription: d\n---\nbody",
		"docs/README.md":    "ignore me",
	})

	res, err := NewGoGit().Fetch(context.Background(), Request{RepoURL: "file://" + repoDir})
	require.NoError(t, err, "Fetch default branch")

	require.Regexp(t, hexSHA, res.SHA, "resolved SHA is a 40-hex commit hash")
	require.Contains(t, res.Files, "skills/x/SKILL.md", "SKILL.md is walked into Files")
	assert.Equal(t, "---\nname: x\ndescription: d\n---\nbody", string(res.Files["skills/x/SKILL.md"]))
	assert.Contains(t, res.Files, "docs/README.md", "all regular files are walked, not just SKILL.md")
	// The .git directory must never leak into the returned tree.
	for p := range res.Files {
		assert.NotContains(t, p, ".git/", "walk skips the .git dir")
	}
}

func TestGoGitFetchByBranchRef(t *testing.T) {
	repoDir := buildLocalRepo(t, "feature", map[string]string{
		"skills/x/SKILL.md": "---\nname: x\ndescription: d\n---\non a branch",
	})

	res, err := NewGoGit().Fetch(context.Background(), Request{
		RepoURL: "file://" + repoDir,
		Ref:     "feature",
	})
	require.NoError(t, err, "Fetch by branch ref")

	require.Regexp(t, hexSHA, res.SHA)
	require.Contains(t, res.Files, "skills/x/SKILL.md")
	assert.Equal(t, "---\nname: x\ndescription: d\n---\non a branch", string(res.Files["skills/x/SKILL.md"]))
}

// TestGoGitFetch_RefusesLinkLocalOverHTTP pins the SSRF guard on the operator's
// git-clone path. spec.repoURL is a tenant-writable free-form string on a
// namespaced CR with no scheme guard, dialed by the OPERATOR process, so
// http://169.254.169.254/… reaches the cloud-metadata endpoint from the most
// privileged process in the install. gogit.go's init() installs
// safehttp.Client() as go-git's http/https transport to refuse exactly that.
//
// The other clone tests use file://, which go-git resolves through its built-in
// FILE transport — the http/https InstallProtocol guard is never exercised by
// them, so removing it breaks none of them. This test uses http:// so the
// guarded transport IS reached.
//
// The assertion is on "blocked", not merely on require.Error: with the guard
// removed, go-git attempts a real dial to the metadata IP, which still errors
// (connection refused / the bounded-context deadline) — but with a network
// error that does NOT say "blocked". Asserting the safehttp refusal text is what
// makes this fail when the guard is gone rather than pass on any dial failure.
func TestGoGitFetch_RefusesLinkLocalOverHTTP(t *testing.T) {
	// Bounded so that if the guard is ever removed, the real dial this would then
	// attempt fails fast instead of hanging the suite on a cloud-metadata IP.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	_, err := NewGoGit().Fetch(ctx, Request{RepoURL: "http://169.254.169.254/latest/meta-data/"})
	require.Error(t, err, "a clone of the cloud-metadata endpoint must be refused")
	assert.ErrorContains(t, err, "blocked",
		"the refusal must come from the safehttp guard (a blocked private/link-local address), "+
			"not from an ordinary dial failure — that distinction is what regresses if init()'s "+
			"InstallProtocol guard is removed")
}

func TestGoGitFetchByCommitSHA(t *testing.T) {
	repoDir := buildLocalRepo(t, "", map[string]string{
		"skills/x/SKILL.md": "---\nname: x\ndescription: d\n---\nbody",
	})

	// Resolve the seeded commit so we can fetch by raw SHA (not a branch/tag).
	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	sha := head.Hash().String()

	res, err := NewGoGit().Fetch(context.Background(), Request{
		RepoURL: "file://" + repoDir,
		Ref:     sha,
	})
	require.NoError(t, err, "Fetch by raw commit SHA")
	assert.Equal(t, sha, res.SHA, "resolved SHA matches the requested commit")
	require.Contains(t, res.Files, "skills/x/SKILL.md")
}
