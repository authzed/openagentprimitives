package artifactview_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
)

// This file pins the DURABLE artifact link end to end: the URL
// channelkinds.ComposeArtifactViewURL composes is written somewhere whose
// audience is not known in advance and cannot be enumerated (a GitHub check
// run's details_url, read by everyone who can read the pull request), so the
// two halves of that promise are asserted against the real server rather than
// against either side alone:
//
//   - the composer and the route agree on the shape, so the link a check run
//     carries still resolves months later; and
//   - the link carries no authority, so authorization happens entirely on
//     arrival, from the visitor's authenticated subject.

// durableLinkRequest composes the durable link for this test server's host and
// returns it as a request. Composed rather than hand-written on purpose: a
// hand-written path here would keep passing after the composer drifted, which
// is exactly the failure that leaves a dead link on a public pull request.
func durableLinkRequest(t *testing.T, sessionRef, artifactID string) *http.Request {
	t.Helper()
	raw, err := channelkinds.ComposeArtifactViewURL("http://"+trustedHost, sessionRef, artifactID)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	r := httptest.NewRequest(http.MethodGet, raw, nil)
	r.Host = trustedHost
	return r
}

// TestDurableLink_ComposedURLResolvesForAnAuthorizedViewer is the contract
// between the composer and the route, asserted across the package boundary.
// The two are edited by different concerns — a URL shape in channelkinds, a
// route pattern in webui — and nothing else would notice them drifting apart
// until a maintainer clicked a link on a pull request and got a 404.
func TestDurableLink_ComposedURLResolvesForAnAuthorizedViewer(t *testing.T) {
	av := fakeAV()
	var verifyLinkCalled bool
	av.verifyLink = func(raw string) (string, string, string, error) {
		verifyLinkCalled = true
		return "", "", "", assert.AnError
	}
	var gotArtifactID, gotSubject string
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		gotArtifactID, gotSubject = artifactID, subject
		return true, nil
	}
	s := newServer(t, "user:maintainer", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, durableLinkRequest(t, "default/review-42", "artifact-report"))

	require.Equal(t, http.StatusOK, rec.Code, "the composed durable link must reach the viewer")
	assert.Contains(t, rec.Body.String(), `data-app="artifact-view"`, "the shell mounts for a durable link")
	assert.False(t, verifyLinkCalled, "a durable link carries no signature, so nothing should try to verify one")
	assert.Equal(t, "artifact-report", gotArtifactID, "the route reads the artifact the composer named")
	assert.Equal(t, "user:maintainer", gotSubject,
		"authorization runs against the AUTHENTICATED visitor, never against anything the URL carried")
}

// TestDurableLink_RefusesAnUnauthorizedViewerWith403 is the decision this
// design owes an explicit answer to, pinned so it cannot drift silently.
//
// 403, not 404. A 404 would be indistinguishable from a broken link, leaving a
// maintainer who legitimately needs access unable to tell "ask for access" from
// "report a bad link" — and it would buy no confidentiality here, because the
// check run carrying this link already tells everyone who can read the pull
// request that a report exists and what it concluded. The secret is the
// report's CONTENT, and 403 discloses none of it. It is also the same refusal
// the signed path already returns, so the two ways of addressing one page
// cannot come to disagree about what a denial looks like.
func TestDurableLink_RefusesAnUnauthorizedViewerWith403(t *testing.T) {
	av := fakeAV()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) { return false, nil }
	var probed bool
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
		probed = true
		return nil, nil
	}
	s := newServer(t, "user:stranger", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, durableLinkRequest(t, "default/review-42", "artifact-report"))

	assert.Equal(t, http.StatusForbidden, rec.Code, "a durable link is not a capability: an unauthorized visitor is refused")
	assert.NotContains(t, rec.Body.String(), "/content?ct=", "a refusal must not hand back the content URL")
	assert.NotContains(t, rec.Body.String(), "/artifact-host?ct=", "a refusal must not hand back the host URL")
	assert.False(t, probed, "a refused visitor must not reach the artifact store either")
}

// TestDurableLink_SameURLDifferentSubjects_DecidedPerVisitor is the security
// property this whole design rests on, stated as the one thing a reader would
// want proven: the URL is a constant, and the outcome is not a function of it.
// One string, two visitors, two different answers — so knowing the string is
// worth nothing on its own.
func TestDurableLink_SameURLDifferentSubjects_DecidedPerVisitor(t *testing.T) {
	const sessionRef, artifactID = "default/review-42", "artifact-report"

	allowed := fakeAV()
	allowed.checkView = func(ctx context.Context, _, subject string) (bool, error) {
		return subject == "user:maintainer", nil
	}
	refused := fakeAV()
	refused.checkView = func(ctx context.Context, _, subject string) (bool, error) {
		return subject == "user:maintainer", nil
	}

	okRec := httptest.NewRecorder()
	newServer(t, "user:maintainer", allowed).ServeHTTP(okRec, durableLinkRequest(t, sessionRef, artifactID))

	denyRec := httptest.NewRecorder()
	newServer(t, "user:stranger", refused).ServeHTTP(denyRec, durableLinkRequest(t, sessionRef, artifactID))

	assert.Equal(t, http.StatusOK, okRec.Code, "the participant sees the report")
	assert.Equal(t, http.StatusForbidden, denyRec.Code,
		"the same URL, forwarded to someone else, admits nobody it did not already admit")
}

// TestDurableLink_AuthzErrorIs500NotADenial: an unreachable authorizer must
// not read as "no access". A maintainer who is told 403 stops asking; one told
// 500 escalates, which is the correct response to a broken SpiceDB.
func TestDurableLink_AuthzErrorIs500NotADenial(t *testing.T) {
	av := fakeAV()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		return false, assert.AnError
	}
	s := newServer(t, "user:maintainer", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, durableLinkRequest(t, "default/review-42", "artifact-report"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
