// Package skillfetch abstracts cloning a git repo at a ref and returning its
// files + the resolved commit SHA. The interface lets the SkillSource
// reconciler stay unit-testable (inject a Fake) while a go-git backend does the
// real network fetch.
package skillfetch

import "context"

// Request describes what to fetch.
type Request struct {
	RepoURL string // as written on the SkillSource (may include scheme)
	Ref     string // tag/branch/sha; empty = default branch
	Token   string // optional auth token (e.g. GitHub PAT); empty = anonymous
}

// Result is the fetched tree.
type Result struct {
	SHA   string            // resolved full commit SHA
	Files map[string][]byte // repo-root-relative path → file content
}

// Fetcher clones a repo at a ref and returns its files + resolved SHA.
type Fetcher interface {
	Fetch(ctx context.Context, req Request) (Result, error)
}

// Fake is a test Fetcher that returns a canned Result and records the request.
type Fake struct {
	Result      Result
	Err         error
	LastRequest Request
}

var _ Fetcher = (*Fake)(nil)

func (f *Fake) Fetch(_ context.Context, req Request) (Result, error) {
	f.LastRequest = req
	if f.Err != nil {
		return Result{}, f.Err
	}
	return f.Result, nil
}
