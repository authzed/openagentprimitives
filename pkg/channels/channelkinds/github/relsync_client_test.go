package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// MAJOR (final whole-branch review): the design doc calls GitHub's rate
// limits "the primary constraint, not an edge case", and the kind-agnostic
// backoff seam has existed since Slack's own 429 handling
// (pkg/controllers/relationshipsource's RetryAfter interface, matched via
// errors.As and honoured as the pass's RequeueAfter). This client implemented
// none of it: a 403 or 429 became "unexpected status", the reconciler retried
// on the ordinary interval, and a throttled source could never reach full
// fetch coverage — which is also what bounds the stale-bridge-edge window to
// "one full pass". Unthrottleable, that window is unbounded.
//
// The interface is declared locally and structurally, exactly as the Slack
// test declares it, so this proves the real shape without dragging the
// (deliberately kind-agnostic) controller package into a kind's own test.
type retryAfterer interface{ RetryAfter() time.Duration }

func TestGithubClient_RateLimitCarriesRetryAfter(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		status  int
		check   func(t *testing.T, err error)
	}{
		{
			name:    "429 with Retry-After: the documented secondary-limit shape",
			status:  http.StatusTooManyRequests,
			headers: map[string]string{"Retry-After": "42"},
			check: func(t *testing.T, err error) {
				var ra retryAfterer
				require.ErrorAs(t, err, &ra,
					"a 429 must surface a value implementing RetryAfter() time.Duration, the exact shape the reconciler matches, or its backoff honouring silently does nothing")
				assert.Equal(t, 42*time.Second, ra.RetryAfter())
			},
		},
		{
			name:    "403 with Retry-After: GitHub serves secondary limits as 403 too",
			status:  http.StatusForbidden,
			headers: map[string]string{"Retry-After": "12"},
			check: func(t *testing.T, err error) {
				var ra retryAfterer
				require.ErrorAs(t, err, &ra, "a 403 carrying Retry-After is a rate limit, not a permissions refusal")
				assert.Equal(t, 12*time.Second, ra.RetryAfter())
			},
		},
		{
			name:   "403 with an exhausted primary limit: backoff comes from x-ratelimit-reset",
			status: http.StatusForbidden,
			headers: map[string]string{
				"x-ratelimit-remaining": "0",
				"x-ratelimit-reset":     strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10),
			},
			check: func(t *testing.T, err error) {
				var ra retryAfterer
				require.ErrorAs(t, err, &ra,
					"the PRIMARY rate limit carries no Retry-After at all; honouring only that header would leave the common case unhandled")
				assert.Greater(t, ra.RetryAfter(), 80*time.Second, "the reset is ~90s out")
				assert.LessOrEqual(t, ra.RetryAfter(), 90*time.Second)
			},
		},
		{
			name:    "403 with no rate-limit headers: a permissions refusal, NOT a backoff",
			status:  http.StatusForbidden,
			headers: nil,
			check: func(t *testing.T, err error) {
				var ra retryAfterer
				assert.False(t, errors.As(err, &ra),
					"an SSO-enforcement or missing-scope 403 must never claim a backoff: the source would sleep instead of surfacing a problem no wait can fix")
			},
		},
		{
			name:    "500: an ordinary failure stays ordinary",
			status:  http.StatusInternalServerError,
			headers: nil,
			check: func(t *testing.T, err error) {
				var ra retryAfterer
				assert.False(t, errors.As(err, &ra), "an ordinary error must never satisfy RetryAfter, or every scope error would read as a rate limit")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/orgs/acme/members", func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			k := &SyncKind{}
			params := paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}})

			_, err := k.FetchScope(ctx, params, relsync.Scope{ID: "acme", ResourceType: githubOrgResourceType})
			require.Error(t, err, "the status must surface as an error, never be swallowed")
			tc.check(t, err)
		})
	}
}

// The rate limit has to reach the caller from EVERY route this kind drives,
// not just the one the table above happens to use: enumeration and each
// fetcher run through the same doGet, and a limit hit while enumerating is
// exactly the case that stops a pass from ever achieving full coverage.
func TestGithubClient_RateLimitOnListScopesCarriesRetryAfter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/acme/teams", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	k := &SyncKind{}
	_, err := k.ListScopes(ctx, paramsFor(t, srv, map[string]any{"orgs": []string{"acme"}}), relsync.Cursor{})
	require.Error(t, err)

	var ra retryAfterer
	require.ErrorAs(t, err, &ra, "a rate limit during enumeration must carry its backoff too")
	assert.Equal(t, 17*time.Second, ra.RetryAfter())
}
