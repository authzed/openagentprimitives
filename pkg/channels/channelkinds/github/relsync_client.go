package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// defaultAPIBase is api.github.com's own REST base — used whenever
// params.Endpoint is empty. A GitHub Enterprise Server install sets
// RelationshipSourceSpec.BaseURL instead (e.g. "https://ghe.example.com/api/v3"),
// which is customer-hosted and legitimately private/loopback — see
// SyncKind's own doc on why this package never wraps its client in
// pkg/x/safehttp.
const defaultAPIBase = "https://api.github.com"

// githubAPIVersion is sent as X-GitHub-Api-Version on every request, per
// GitHub's REST API versioning contract — pinned so a future default bump
// upstream cannot silently change this kind's response shape out from under
// it.
const githubAPIVersion = "2022-11-28"

// httpClient returns k.HTTPClient, or http.DefaultClient when unset.
func (k *SyncKind) httpClient() *http.Client {
	if k.HTTPClient != nil {
		return k.HTTPClient
	}
	return http.DefaultClient
}

// doGet issues an authenticated GET against target, which is either a path
// relative to the resolved base (a fresh page: "/orgs/acme/teams?per_page=100")
// or an absolute URL (a resumed page: the exact "rel=\"next\"" URL a prior
// response's Link header carried — see nextLink). GitHub's Link-header URL is
// opaque and carried verbatim; this function never rebuilds one from a page
// number.
//
// The base resolves to params.Endpoint when set (a GHES install), else
// defaultAPIBase (github.com). Auth is "Authorization: Bearer <token>" — the
// same header GitHub's REST API accepts for both a classic PAT and a fine-
// grained one. The caller owns closing the response body.
func (k *SyncKind) doGet(ctx context.Context, params relsync.SourceParams, target string) (*http.Response, error) {
	u := target
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		base := strings.TrimRight(params.Endpoint, "/")
		if base == "" {
			base = defaultAPIBase
		}
		u = base + target
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("github: build request for %s: %w", target, err)
	}
	req.Header.Set("Authorization", "Bearer "+string(params.Token.UnderlyingValue()))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	// The rate limit is answered HERE, at the one choke point every GitHub
	// call in this kind goes through — enumeration and all three fetchers
	// alike — rather than at each decoder, so no call site can be added
	// later that forgets to honour it.
	if rl := rateLimitErrorFor(resp); rl != nil {
		resp.Body.Close()
		return nil, rl
	}
	return resp, nil
}

// githubRateLimitError carries a rate limit's server-specified backoff in the
// shape pkg/controllers/relationshipsource's RetryAfter interface matches via
// errors.As (RetryAfter() time.Duration), so a throttled pass requeues when
// GitHub said to instead of on the ordinary interval.
//
// The controller stays kind-agnostic on purpose (CLAUDE.md: a new variant
// gets registered, not branched on), so the adaptation from GitHub's own
// header vocabulary belongs here, in the one package that knows it —
// the same division slack's own slackRetryAfterError makes.
type githubRateLimitError struct {
	status     int
	url        string
	retryAfter time.Duration
}

func (e *githubRateLimitError) Error() string {
	return fmt.Sprintf("github: GET %s: rate limited (status %d); retry after %s", e.url, e.status, e.retryAfter)
}

func (e *githubRateLimitError) RetryAfter() time.Duration { return e.retryAfter }

// minRateLimitBackoff is what a rate limit reports when GitHub's own reset
// has already passed by the time the response is read. Zero would be
// indistinguishable from "no backoff reported" to retryAfterFrom, which
// would silently drop the signal; a second says "throttled, but the window
// is already open".
const minRateLimitBackoff = time.Second

// rateLimitErrorFor returns a *githubRateLimitError when resp is GitHub
// refusing for RATE reasons, and nil for everything else — including a 403
// that is an ordinary permissions refusal, which must keep surfacing as a
// plain error rather than putting the source to sleep over something no wait
// can fix.
//
// GitHub's two limits are reported differently, and only reading both covers
// the case the design doc called the primary constraint:
//
//   - A SECONDARY limit (abuse detection) answers 403 or 429 WITH a
//     Retry-After header, in seconds.
//   - The PRIMARY hourly limit answers 403 or 429 with NO Retry-After at
//     all: it reports x-ratelimit-remaining: 0 and x-ratelimit-reset as a
//     Unix timestamp. Honouring only Retry-After would leave the common
//     case unhandled.
//
// Retry-After wins when both are present, per GitHub's own documented
// guidance.
func rateLimitErrorFor(resp *http.Response) *githubRateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	mk := func(d time.Duration) *githubRateLimitError {
		if d < minRateLimitBackoff {
			d = minRateLimitBackoff
		}
		return &githubRateLimitError{status: resp.StatusCode, url: resp.Request.URL.String(), retryAfter: d}
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return mk(time.Duration(secs) * time.Second)
		}
		// A non-numeric Retry-After is an HTTP-date, which GitHub does not
		// send but the RFC allows. Fall through to the reset headers rather
		// than guessing; if those are absent too, this is reported below as
		// a rate limit with the floor backoff rather than as an ordinary
		// error, because the header's PRESENCE already says throttled.
		if t, err := http.ParseTime(strings.TrimSpace(v)); err == nil {
			return mk(time.Until(t))
		}
		return mk(0)
	}
	if resp.Header.Get("x-ratelimit-remaining") == "0" {
		if sec, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("x-ratelimit-reset")), 10, 64); err == nil {
			return mk(time.Until(time.Unix(sec, 0)))
		}
		return mk(0)
	}
	return nil
}

// linkNextPattern matches the "rel=\"next\"" segment of an RFC 8288 Link
// header, e.g. `<https://api.github.com/orgs/acme/teams?page=2>; rel="next"`
// among other comma-separated segments (prev/last/first). GitHub's REST API
// emits exactly this shape for every paginated list endpoint.
var linkNextPattern = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// nextLink extracts the "rel=\"next\"" URL from resp's Link header, or "" when
// none is present (the last page, or an endpoint with only one page). The URL
// is returned verbatim — GitHub's own pagination token is opaque, the same
// shape as the Slack kind's conversations.list cursor (never parsed into a
// page number by this package), and it is carried as relsync.Cursor.Token
// unchanged.
func nextLink(resp *http.Response) string {
	link := resp.Header.Get("Link")
	if link == "" {
		return ""
	}
	m := linkNextPattern.FindStringSubmatch(link)
	if m == nil {
		return ""
	}
	return m[1]
}

// fetchAllGH pages a GitHub list endpoint (a bare JSON array, Link-header
// pagination) to COMPLETION and returns every item across every page. Used
// only by FetchScope's fetchers, never by ListScopes: FetchScope's own
// atomicity contract (relsync.Kind's own doc — "a complete ScopeContent or
// an error, never a partial one") needs the whole list assembled before a
// single tuple is built, where ListScopes instead returns one page and a
// resumable cursor for the caller to feed back. path is the first page's
// request target (a path relative to the resolved base); every subsequent
// page rides the opaque URL nextLink extracted from the prior response,
// exactly like doGet's own contract — never rebuilt from a guessed page
// number.
func fetchAllGH[T any](ctx context.Context, k *SyncKind, params relsync.SourceParams, path string) ([]T, error) {
	var out []T
	next := path
	for next != "" {
		resp, err := k.doGet(ctx, params, next)
		if err != nil {
			return nil, err
		}
		page, link, err := decodeGHListPage[T](resp)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		next = link
	}
	return out, nil
}

// decodeGHListPage reads and closes resp's body as a bare JSON array of T,
// returning the decoded page and the next page's opaque URL (empty when
// this was the last page) — fetchAllGH's own per-page step, factored out so
// the loop above stays about pagination, not decoding.
//
// A 404 surfaces as errGHNotFound (the same sentinel decodeGHTeamDetail
// uses for a deleted team), unwrapped by fetchAllGH's own passthrough, so a
// caller can errors.Is against it regardless of which list endpoint this
// page came from — fetchRepoScope does exactly that for a repository
// deleted upstream between enumeration and fetch.
func decodeGHListPage[T any](resp *http.Response) ([]T, string, error) {
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", errGHNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("github: GET %s: unexpected status %d", resp.Request.URL, resp.StatusCode)
	}
	var page []T
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("github: decode response: %w", err)
	}
	return page, nextLink(resp), nil
}
