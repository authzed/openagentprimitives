// Package checkruns is the installation-scoped half of this kind's GitHub REST
// surface: reading a pull request's head commit, and reading and writing the
// check runs on it.
//
// Separate from appprovision, which authenticates as the APP (a JWT signed with
// the App's private key) to read and repoint the App's own configuration. These
// calls authenticate as an INSTALLATION (a minted access token) because they
// act on a repository, and the two legs have different credentials, different
// lifetimes and different blast radii. Keeping them in different packages is
// what stops a future call from reaching for whichever token is nearest.
//
// Nothing here decides WHAT to write. The mapping from the framework's outcome
// vocabulary onto github's conclusions, the refusal of an unpublishable
// argument, and the find-or-create ordering all live in the kind
// (../triggerstatus.go); this package is the wire.
package checkruns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// defaultBaseURL is GitHub's API host. Overridden by WithBaseURL for a test's
// stand-in server or a self-hosted instance.
const defaultBaseURL = "https://api.github.com"

// defaultTimeout bounds one call. A check-run write happening at the end of a
// review must not be the thing that hangs a session.
const defaultTimeout = 30 * time.Second

// maxResponseBodyBytes bounds how much of a response this client reads, so a
// misbehaving or malicious server cannot force unbounded memory growth. The
// list endpoint is the largest of these and is already page-bounded below.
const maxResponseBodyBytes = 1 << 20 // 1 MiB

// apiVersion pins the REST schema this package decodes. GitHub dates its
// breaking changes; naming the version is what keeps a future default from
// silently changing the shapes below.
const apiVersion = "2022-11-28"

// Repo names a repository as GitHub addresses it.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Client talks to the installation-scoped REST API.
type Client struct {
	baseURL string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at a stand-in GitHub.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = u
		}
	}
}

// New returns a client for the real GitHub API unless told otherwise.
func New(opts ...Option) *Client {
	c := &Client{baseURL: defaultBaseURL, http: &http.Client{Timeout: defaultTimeout}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Run is one check run, reduced to what a caller needs to decide whether the
// commit already carries an answer and which run to write to.
type Run struct {
	ID int64
	// Status is github's lifecycle value: "queued", "in_progress" or
	// "completed".
	Status string
	// Conclusion is set only on a completed run.
	Conclusion string
}

// Completed reports whether this run already carries an answer.
func (r Run) Completed() bool { return r.Status == "completed" && r.Conclusion != "" }

// Write is the body of a check-run create or update. One shape for both: the
// fields github accepts are the same, and a second struct would be two places
// for a field to be spelled.
type Write struct {
	// Name and HeadSHA are set on a CREATE only; github derives both for an
	// update from the run being patched.
	Name    string `json:"name,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`

	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	DetailsURL string `json:"details_url,omitempty"`
	// ExternalID is the caller's own correlation handle. This kind records the
	// commit the answer is about, so a later round can diff against it.
	ExternalID string `json:"external_id,omitempty"`

	StartedAt   string  `json:"started_at,omitempty"`
	CompletedAt string  `json:"completed_at,omitempty"`
	Output      *Output `json:"output,omitempty"`
}

// Output is the rendered body a person reads on the pull request.
type Output struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// PullRequestHeadSHA reads the commit a pull request currently points at.
//
// Read LIVE rather than carried from the webhook that started the session on
// purpose: one session spans a pull request's whole life, so a SHA frozen at
// session creation is wrong from the second push onward — and a caller with no
// session at all (an operator closing a claim nobody finished) has no frozen
// value to carry in the first place.
func (c *Client) PullRequestHeadSHA(ctx context.Context, token string, repo Repo, number int) (string, error) {
	path := "/repos/" + repo.String() + "/pulls/" + strconv.Itoa(number)
	var resp struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.do(ctx, http.MethodGet, path, token, nil, &resp); err != nil {
		return "", err
	}
	if resp.Head.SHA == "" {
		return "", fmt.Errorf("github %s: response carried no head commit", path)
	}
	return resp.Head.SHA, nil
}

// FindByName returns the check run named name on commit sha, or (nil, nil) when
// there is none.
//
// Filtered server-side by check_name so a busy commit's other checks are never
// read at all, and capped at one page: this kind writes at most one run per
// (commit, name), so a second page would mean something else is writing under
// our name and picking one of those is not this function's call to make.
func (c *Client) FindByName(ctx context.Context, token string, repo Repo, sha, name string) (*Run, error) {
	path := "/repos/" + repo.String() + "/commits/" + sha + "/check-runs"
	var resp struct {
		Runs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	// Escaped rather than concatenated raw: the name comes from a CRD field, so
	// it is operator-authored and not attacker-chosen, but a query parameter
	// assembled by hand is how a filter silently stops filtering.
	query := "?check_name=" + url.QueryEscape(name) + "&per_page=100"
	if err := c.do(ctx, http.MethodGet, path+query, token, nil, &resp); err != nil {
		return nil, err
	}
	for _, r := range resp.Runs {
		// check_name is a server-side filter, but it is re-checked here: a
		// stand-in server, or a future API that loosens the filter to a
		// prefix match, would otherwise let this kind write to a run it does
		// not own.
		if r.Name != name {
			continue
		}
		return &Run{ID: r.ID, Status: r.Status, Conclusion: r.Conclusion}, nil
	}
	return nil, nil
}

// Create opens a new check run and returns it.
func (c *Client) Create(ctx context.Context, token string, repo Repo, w Write) (*Run, error) {
	path := "/repos/" + repo.String() + "/check-runs"
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, path, token, w, &resp); err != nil {
		return nil, err
	}
	return &Run{ID: resp.ID, Status: w.Status, Conclusion: w.Conclusion}, nil
}

// Update patches an existing check run.
func (c *Client) Update(ctx context.Context, token string, repo Repo, id int64, w Write) error {
	path := "/repos/" + repo.String() + "/check-runs/" + strconv.FormatInt(id, 10)
	return c.do(ctx, http.MethodPatch, path, token, w, nil)
}

// do performs one call. body is JSON-encoded when non-nil; out is decoded into
// when non-nil.
//
// The token never appears in a returned error. Neither does a response body: a
// provider error page can embed request context, and these errors travel back
// to a model as a tool result.
func (c *Client) do(ctx context.Context, method, path, token string, body, out any) error {
	var buf io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("github %s: encode request: %w", path, err)
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, buf)
	if err != nil {
		return fmt.Errorf("github %s: build request failed", path)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		// The error is not wrapped: *url.Error's message embeds the full URL,
		// and a caller logging it would spread the repository path into places
		// that only asked whether the call worked.
		return fmt.Errorf("github %s: call failed", path)
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBodyBytes))
	if err != nil {
		return fmt.Errorf("github %s: read response: %w", path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("github %s: status %d", path, res.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github %s: decode response: %w", path, err)
	}
	return nil
}
