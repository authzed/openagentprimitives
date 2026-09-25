// GoGit is the real skillfetch.Fetcher: it clones a repo at a ref into an
// in-memory filesystem (no on-disk checkout) and returns every regular file
// plus the resolved commit SHA. It is a thin adapter over go-git v5; the
// SkillSource reconciler + discovery logic are tested independently with the
// Fake.
package skillfetch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	billyutil "github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// GoGit clones git repos using go-git, entirely in-memory.
type GoGit struct{}

// NewGoGit constructs the go-git-backed Fetcher.
func NewGoGit() *GoGit { return &GoGit{} }

// init installs an SSRF-GUARDED HTTP client as go-git's transport for both
// http and https.
//
// spec.repoURL is a tenant-writable free-form string on a namespaced CR, with
// no admission validation and no scheme guard, dialed by the OPERATOR process
// — so http://169.254.169.254/… reached the cloud-metadata endpoint from the
// most privileged process in the install. pkg/tools/discover/open_url.go
// reaches for the same guard on a strictly LESS privileged path.
//
// Registered at the transport rather than per CloneOptions: go-git resolves the
// client by URL scheme, so a guard attached at one call site is one a later
// call site silently does not inherit. Process-wide, and deliberately — there
// is no git clone in these binaries that should be reaching a private address.
func init() {
	client.InstallProtocol("https", githttp.NewClient(safehttp.Client()))
	client.InstallProtocol("http", githttp.NewClient(safehttp.Client()))
}

var _ Fetcher = (*GoGit)(nil)

// Fetch clones req.RepoURL at req.Ref into an in-memory filesystem and returns
// the worktree files keyed by repo-root-relative slash path, plus the resolved
// full HEAD commit SHA.
//
// Ref handling: a git ref name can be a branch, a tag, or a raw commit SHA, and
// they need different clone strategies. We try them in that order:
//
//  1. Branch: clone with ReferenceName = refs/heads/<ref> + SingleBranch.
//  2. Tag: if (1) fails, retry with ReferenceName = refs/tags/<ref>.
//  3. Raw commit SHA: if both ref-name clones fail, clone the default branch
//     (full history, no Depth limit so the commit is reachable) and check out
//     the commit in detached HEAD.
//
// When req.Ref is empty we clone the default branch (HEAD).
func (g *GoGit) Fetch(ctx context.Context, req Request) (Result, error) {
	auth := authMethod(req.Token)

	repo, err := cloneAtRef(ctx, req.RepoURL, req.Ref, auth)
	if err != nil {
		return Result{}, err
	}

	head, err := repo.Head()
	if err != nil {
		return Result{}, fmt.Errorf("skillfetch: resolve HEAD for %q: %w", req.RepoURL, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return Result{}, fmt.Errorf("skillfetch: open worktree for %q: %w", req.RepoURL, err)
	}

	files, err := walkWorktree(wt.Filesystem)
	if err != nil {
		return Result{}, fmt.Errorf("skillfetch: read worktree for %q: %w", req.RepoURL, err)
	}

	return Result{SHA: head.Hash().String(), Files: files}, nil
}

// authMethod returns a BasicAuth using the GitHub PAT convention when a token
// is present, or nil for anonymous clones. GitHub accepts a PAT as the password
// with any non-empty username; "x-access-token" is the documented convention.
func authMethod(token string) *githttp.BasicAuth {
	if token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: token}
}

// cloneInMem clones into a fresh in-memory storer + worktree filesystem.
func cloneInMem(ctx context.Context, opts *git.CloneOptions) (*git.Repository, error) {
	return git.CloneContext(ctx, memory.NewStorage(), memfs.New(), opts)
}

// cloneAtRef implements the branch → tag → commit-SHA strategy documented on
// Fetch. The error returned on total failure is the SHA-checkout error wrapped
// with context (the most informative of the three attempts for an arbitrary
// ref), since by that point we've established the ref is not a branch or tag.
//
// Auth errors (HTTP 401/403) short-circuit immediately: they indicate a bad
// credential, not a wrong ref name, so falling through to the tag/SHA attempts
// would mask the real cause and produce a confusing final error.
func cloneAtRef(ctx context.Context, repoURL, ref string, auth *githttp.BasicAuth) (*git.Repository, error) {
	if ref == "" {
		repo, err := cloneInMem(ctx, &git.CloneOptions{URL: repoURL, Auth: auth, Depth: 1})
		if err != nil {
			return nil, fmt.Errorf("skillfetch: clone %q default branch: %w", repoURL, err)
		}
		return repo, nil
	}

	// Branch, then tag: a single-branch shallow clone is enough and avoids
	// pulling full history.
	for i, refName := range []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(ref),
		plumbing.NewTagReferenceName(ref),
	} {
		repo, err := cloneInMem(ctx, &git.CloneOptions{
			URL:           repoURL,
			Auth:          auth,
			ReferenceName: refName,
			SingleBranch:  true,
			Depth:         1,
		})
		if err == nil {
			return repo, nil
		}
		// An auth error on the first attempt is definitive: the credential is
		// wrong regardless of which ref name we use. Surface it immediately so
		// the caller (and the SkillSource Ready condition) shows the real cause.
		if i == 0 && isAuthError(err) {
			return nil, fmt.Errorf("skillfetch: clone %q: authentication failed: %w", repoURL, err)
		}
	}

	// Raw commit SHA: clone the default branch with full history (no Depth, so
	// the target commit is reachable) and check it out in detached HEAD.
	repo, err := cloneInMem(ctx, &git.CloneOptions{URL: repoURL, Auth: auth})
	if err != nil {
		return nil, fmt.Errorf("skillfetch: clone %q for commit %q: %w", repoURL, ref, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("skillfetch: worktree for commit %q in %q: %w", ref, repoURL, err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(ref)}); err != nil {
		return nil, fmt.Errorf("skillfetch: checkout commit %q in %q (not a branch/tag/sha?): %w", ref, repoURL, err)
	}
	return repo, nil
}

// isAuthError reports whether err is a git authentication/authorisation failure.
// go-git's HTTP transport returns *githttp.Err with StatusCode 401 or 403 for
// auth failures; we unwrap to find it.
func isAuthError(err error) bool {
	var he *githttp.Err
	if errors.As(err, &he) {
		return he.StatusCode() == http.StatusUnauthorized ||
			he.StatusCode() == http.StatusForbidden
	}
	return false
}

// walkWorktree reads every regular file in the billy worktree into a map keyed
// by repo-root-relative slash path. The .git metadata dir is skipped — it is
// never part of the published tree.
func walkWorktree(fsys billy.Filesystem) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := billyutil.Walk(fsys, "/", func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, rerr := billyutil.ReadFile(fsys, path)
		if rerr != nil {
			return fmt.Errorf("read %q: %w", path, rerr)
		}
		// billy roots the walk at "/"; trim it to get a repo-root-relative key.
		key := path
		for len(key) > 0 && key[0] == '/' {
			key = key[1:]
		}
		out[key] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
