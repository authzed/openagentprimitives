// pkg/memory/httpclient/sentinel_test.go
//
// The client half of the sentinel wire contract, for the responses a real
// httpsrv cannot produce: a server too old to stamp the discriminator, a server
// newer than this build, an empty body, and a sentinel arriving with a status
// that says "retry". The agreeing-with-the-server case is proved end to end
// against the real handler in httpsrv's sentinel_roundtrip_test.go.
package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/sentinel"
)

var sentinelScope = memory.Scope{Kind: "session", ID: "ns/n"}

// serveOnce answers every request with the given status, body and (when
// non-empty) discriminator — standing in for a peer this build does not
// control: an operator mid-upgrade, or an intermediary.
func serveOnce(t *testing.T, status int, code, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code != "" {
			w.Header().Set(sentinel.Header, code)
		}
		http.Error(w, body, status)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok")
}

// queryErr runs a plain list Query and returns its error.
func queryErr(t *testing.T, c *Client) error {
	t.Helper()
	_, err := c.Query(context.Background(), memory.Query{Scope: sentinelScope, Kinds: []string{"turn"}})
	return err
}

// TestDo_UnknownDiscriminator_NotReconstructed: a server newer than this build
// names a sentinel this build has never heard of. Guessing from the status —
// 403, so "probably a capability refusal" — would hand the caller a sentinel the
// server did not claim. It must stay an opaque transport error.
func TestDo_UnknownDiscriminator_NotReconstructed(t *testing.T) {
	err := queryErr(t, serveOnce(t, http.StatusForbidden, "some_future_sentinel", "refused"))

	require.Error(t, err, "precondition: a 403 is an error")
	assert.NotErrorIs(t, err, memory.ErrMissingApproval, "an unknown code names no sentinel this build knows")
	assert.NotErrorIs(t, err, memory.ErrKGScopeMismatch, "nor the other 403 sentinel")
	assert.Contains(t, err.Error(), "403", "the status must still reach the caller for diagnosis")
}

// TestDo_NoDiscriminator_NotReconstructed: the same 403 with no header at all —
// an intermediary proxy, or one of httpsrv's own non-sentinel refusals (the
// cross-session token check, handleKG's approval door). Fail toward unknown.
func TestDo_NoDiscriminator_NotReconstructed(t *testing.T) {
	err := queryErr(t, serveOnce(t, http.StatusForbidden, "", "forbidden"))

	require.Error(t, err, "precondition: a 403 is an error")
	assert.NotErrorIs(t, err, memory.ErrMissingApproval,
		"a genuine authorization refusal must not be laundered into a platform-authored capability verdict")
	assert.Contains(t, err.Error(), "403", "the status must still reach the caller for diagnosis")
}

// TestDo_StampedSentinel_EmptyBody_FallsBackToSentinelText: a refusal with no
// body must still say something. The sentinel's own text is the platform floor —
// an empty Error() would surface to the model as a blank tool failure.
func TestDo_StampedSentinel_EmptyBody_FallsBackToSentinelText(t *testing.T) {
	err := queryErr(t, serveOnce(t, http.StatusBadRequest, "invalid_query", ""))

	require.ErrorIs(t, err, memory.ErrInvalidQuery, "precondition: the stamped sentinel is rebuilt")
	assert.Equal(t, memory.ErrInvalidQuery.Error(), err.Error(),
		"with no body to carry, the sentinel's own text is what the caller gets")
}

// TestSearch_LegacyBare404_StillYieldsNoSearchProviders pins the mixed-version
// bridge. Before the discriminator existed, Search inferred
// ErrNoSearchProviders from a bare 404 — the only sentinel any caller ever got
// back. A cluster mid-upgrade runs a new runner against an old operator, so
// removing the inference would silently disable search_memory's "use
// query_memory instead" recovery copy for the length of the rollout.
//
// It also wraps rather than replaces now: the old code returned the bare
// sentinel and dropped the server's message.
func TestSearch_LegacyBare404_StillYieldsNoSearchProviders(t *testing.T) {
	c := serveOnce(t, http.StatusNotFound, "", "memory: no search providers configured")

	_, err := c.Search(context.Background(), memory.SearchRequest{Scopes: []memory.Scope{sentinelScope}})

	require.ErrorIs(t, err, memory.ErrNoSearchProviders,
		"an old operator's bare 404 on _search must keep resolving, or a rolling upgrade regresses")
	assert.Contains(t, err.Error(), "no search providers configured",
		"the server's message rides along instead of being replaced by the bare sentinel")
}

// TestQuery_Bare404_IsNotLaunderedIntoNoSearchProviders keeps the legacy bridge
// as narrow as it has always been: _search only. Every other route's 404 means
// something else entirely (an unroutable path, an empty ns or name).
func TestQuery_Bare404_IsNotLaunderedIntoNoSearchProviders(t *testing.T) {
	err := queryErr(t, serveOnce(t, http.StatusNotFound, "", "not a memory path"))

	require.Error(t, err, "precondition: a 404 is an error")
	assert.NotErrorIs(t, err, memory.ErrNoSearchProviders,
		"only the _search route's 404 carried that meaning, and only for an unstamped server")
}

// TestDo_StampedSentinelOn5xx_IsRetriedNotReconstructed: the discriminator names
// the sentinel, but the STATUS governs retry, and 5xx means "transient". A
// caller must not be handed a permanent verdict for a response the client is
// still retrying — it gives up with the transport error instead. No row in
// sentinel.Table is 5xx, so this is a guard against a future server, not
// today's.
func TestDo_StampedSentinelOn5xx_IsRetriedNotReconstructed(t *testing.T) {
	fastBackoff(t)
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set(sentinel.Header, "invalid_query")
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	err := queryErr(t, New(srv.URL, "tok"))

	require.Error(t, err, "precondition: every attempt failed")
	assert.Greater(t, attempts, 1, "5xx is transient: the client must have retried")
	assert.NotErrorIs(t, err, memory.ErrInvalidQuery,
		"a status that says 'retry' must not also deliver a permanent verdict")
}
