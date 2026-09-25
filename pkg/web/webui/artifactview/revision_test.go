package artifactview_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
)

// fakeRevisions returns a two-element revision list for use in revision tests.
func fakeRevisions() []artifactview.RevisionMeta {
	return []artifactview.RevisionMeta{
		{Seq: 1, RevisionID: "rev-1", RenderName: "ar-1", ChangeDescription: "initial", CreatedAt: "2026-01-01T00:00:00Z"},
		{Seq: 2, RevisionID: "rev-2", RenderName: "ar-2", ChangeDescription: "update", CreatedAt: "2026-01-02T00:00:00Z"},
	}
}

// fakeAVWithRevisions returns a fakeAV whose ListRevisions returns fakeRevisions().
func fakeAVWithRevisions() *fakeDeps {
	av := fakeAV()
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
		return fakeRevisions(), nil
	}
	// Override SignContentToken to produce a predictable token for the chosen render.
	av.signContentToken = func(ns, sess, renderName, artifactID string) (string, error) {
		return "tok", nil
	}
	av.sandboxBaseURL = func() string { return "https://sandbox.example" }
	return av
}

func TestRevision_HappyPath_Returns200WithContentURL(t *testing.T) {
	av := fakeAVWithRevisions()
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-2"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))

	url, ok := body["contentUrl"]
	assert.True(t, ok, "response must have a contentUrl field")
	assert.Contains(t, url, "/content?ct=", "contentUrl must point at the sandbox content endpoint")
	assert.Contains(t, url, "https://sandbox.example", "contentUrl must use the sandbox base")

	hostURL, hasHostURL := body["hostUrl"]
	assert.True(t, hasHostURL, "response must have a hostUrl field")
	assert.Contains(t, hostURL, "/artifact-host?ct=", "hostUrl must point at the sandbox artifact-host endpoint")
	assert.Contains(t, hostURL, "https://sandbox.example", "hostUrl must use the sandbox base")

	// RenderName must never reach the browser — it is a server-side capability
	// input. Assert the JSON does not leak it.
	raw := rec.Body.String()
	assert.NotContains(t, raw, "ar-2", "renderName must not appear in the JSON response")
	assert.NotContains(t, raw, "renderName", "renderName key must not appear in the JSON response")
}

// TestRevision_UnsignedParams_AdminPath covers the admin viewer path: the React
// client forwards window.location.search (artifactId+sessionRef, no d/sig) to
// /artifact-view/revision, so the handler must resolve the params directly and
// still gate on CheckView with the cookie subject.
func TestRevision_UnsignedParams_AdminPath(t *testing.T) {
	av := fakeAVWithRevisions()
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
	s := newServer(t, "user:admin", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?artifactId=artifact-x&sessionRef=default%2Fs1&rev=rev-2"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, verifyLinkCalled, "the unsigned path must not call VerifyLink")
	assert.Equal(t, "user:admin", gotSubject, "CheckView runs with the authenticated cookie subject")
	assert.Equal(t, "artifact-x", gotArtifactID, "CheckView runs with the query artifact id")
}

func TestRevision_UnknownRev_NotFound(t *testing.T) {
	av := fakeAVWithRevisions()
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=nope"))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "contentUrl", "no content URL on not-found")
}

func TestRevision_CheckViewFalse_Forbidden(t *testing.T) {
	av := fakeAVWithRevisions()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		return false, nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-2"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "contentUrl", "a denied request must not leak the content URL")
	// Ensure renderName is not in the response either.
	assert.NotContains(t, body, "ar-2")
}

func TestRevision_BadLink_Forbidden(t *testing.T) {
	av := fakeAVWithRevisions()
	av.verifyLink = func(raw string) (string, string, string, error) {
		return "", "", "", assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-2"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestRevision_ListRevisionsError_ForbiddenNotBadGateway pins the fail-closed
// half of the artifact→session binding. ListRevisions is the probe that RESOLVES
// which session an artifact lives in, and its "not in this scope" miss is a
// plain error indistinguishable from a store failure — so a failed probe is a
// refusal, not a 502. Serving on a probe that did not answer would put the
// unbound (artifact, session) pair back on the session-scoped calls downstream.
func TestRevision_ListRevisionsError_ForbiddenNotBadGateway(t *testing.T) {
	av := fakeAVWithRevisions()
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
		return nil, assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-2"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestRevision_CheckViewError_InternalServerError(t *testing.T) {
	av := fakeAVWithRevisions()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		return false, assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-2"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRevision_CheckViewRunsBeforeContentURL(t *testing.T) {
	// Verify the authorization gate (CheckView) is invoked before minting any
	// content token. If CheckView denies, SignContentToken must never be called.
	av := fakeAVWithRevisions()
	signCalled := false
	av.signContentToken = func(ns, sess, renderName, artifactID string) (string, error) {
		signCalled = true
		return "tok", nil
	}
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		return false, nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-2"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, signCalled, "SignContentToken must NOT be called when CheckView denies")
}

func TestRevision_CheckViewRunsBeforeListRevisions(t *testing.T) {
	// Verify the SpiceDB gate runs before we even query the revision list.
	// If CheckView denies, ListRevisions must never be called.
	av := fakeAVWithRevisions()
	listCalled := false
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
		listCalled = true
		return fakeRevisions(), nil
	}
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		return false, nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-2"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, listCalled, "ListRevisions must NOT be called when CheckView denies")
}

// TestRevision_BundledOnlyOlderRevision_Generating_NoContentURL verifies the
// security invariant for bundled-only kinds (svg/css) while an OLDER revision's
// preview is STILL GENERATING: PreviewChildRender reports not-ready, so the
// handler must NOT mint a content token for the raw render — the response
// carries an empty contentUrl and the raw render name never leaks.
func TestRevision_BundledOnlyOlderRevision_Generating_NoContentURL(t *testing.T) {
	av := fakeAVWithRevisions() // rev-1 (older) + rev-2 (newest)
	av.renderIsBundledOnly = func(ctx context.Context, ns, sess, artifactID string) bool { return true }
	av.previewChildRender = func(ctx context.Context, ns, sess, revID string) (string, bool, error) {
		return "", false, nil // older revision's preview not generated yet
	}
	signCalled := false
	av.signContentToken = func(ns, sess, renderName, artifactID string) (string, error) {
		signCalled = true
		return "tok", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-1")) // older rev

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &body))
	assert.Equal(t, "", body["hostUrl"], "a still-generating bundled-only revision must yield no host URL")
	assert.Equal(t, "", body["contentUrl"], "a still-generating bundled-only revision must yield no content URL")
	assert.False(t, signCalled, "no content token may be minted while the preview is generating")
	assert.NotContains(t, rec.Body.String(), "ar-1", "raw render name must never leak")
}

// TestRevision_BundledOnlyOlderRevision_Ready_FramesPreviewChild verifies that
// pinning an OLDER bundled-only revision now frames THAT revision's own preview
// child (each revision gets its own generated preview): PreviewChildRender
// reports ready, the handler mints a token for the PREVIEW render — never the
// raw svg/css render — and neither render name leaks.
func TestRevision_BundledOnlyOlderRevision_Ready_FramesPreviewChild(t *testing.T) {
	av := fakeAVWithRevisions() // rev-1 (older) + rev-2 (newest)
	av.renderIsBundledOnly = func(ctx context.Context, ns, sess, artifactID string) bool { return true }
	var gotRevID string
	av.previewChildRender = func(ctx context.Context, ns, sess, revID string) (string, bool, error) {
		gotRevID = revID
		return "ar-preview-r1", true, nil // rev-1's own generated preview child
	}
	var capturedRenderName string
	av.signContentToken = func(ns, sess, renderName, artifactID string) (string, error) {
		capturedRenderName = renderName
		return "tok", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-1")) // older rev

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "rev-1", gotRevID, "the handler must resolve THIS revision's preview child")
	assert.Equal(t, "ar-preview-r1", capturedRenderName, "an older bundled-only revision frames its own preview child render")
	body := rec.Body.String()
	assert.Contains(t, body, "/content?ct=", "a ready older-revision preview yields a content URL")
	assert.NotContains(t, body, "ar-1", "raw svg render name must never leak")
	assert.NotContains(t, body, "ar-preview-r1", "preview child render name must never leak")
}

// TestRevision_BundledOnlyNewestRevision_FramesPreviewChild verifies that pinning
// the NEWEST revision of a bundled-only artifact routes through ContentRender,
// framing the internal preview child render (not the raw svg/css render) and
// never leaking either render name.
func TestRevision_BundledOnlyNewestRevision_FramesPreviewChild(t *testing.T) {
	av := fakeAVWithRevisions() // rev-1 + rev-2 (newest)
	av.renderIsBundledOnly = func(ctx context.Context, ns, sess, artifactID string) bool { return true }
	av.contentRender = func(ctx context.Context, ns, sess, artifactID string) (string, bool, error) {
		return "ar-preview-child", true, nil // the internal html preview child
	}
	var capturedRenderName string
	av.signContentToken = func(ns, sess, renderName, artifactID string) (string, error) {
		capturedRenderName = renderName
		return "tok", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-2")) // newest rev

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ar-preview-child", capturedRenderName, "newest bundled-only revision frames the preview child render")
	body := rec.Body.String()
	assert.Contains(t, body, "/content?ct=", "a ready preview child yields a content URL")
	assert.NotContains(t, body, "ar-2", "raw svg render name must never leak")
	assert.NotContains(t, body, "ar-preview-child", "child render name must never leak")
}

func TestRevision_NoRenderNameInResponse(t *testing.T) {
	// The JSON response for the chosen revision must contain ONLY contentUrl —
	// renderName is a server-side capability and must never be exposed to the browser.
	av := fakeAVWithRevisions()
	var capturedRenderName string
	av.signContentToken = func(ns, sess, renderName, artifactID string) (string, error) {
		capturedRenderName = renderName
		return "tok", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/revision?d=AA&sig=BB&rev=rev-2"))

	require.Equal(t, http.StatusOK, rec.Code)
	// Confirm the server did use ar-2 internally.
	assert.Equal(t, "ar-2", capturedRenderName, "handler must use the correct renderName for rev-2")
	// But ar-2 must not appear in the response body.
	assert.NotContains(t, rec.Body.String(), "ar-2", "renderName must not leak into the response")

	// The only keys in the JSON should be hostUrl and contentUrl.
	var body map[string]string
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &body))
	_, hasContentURL := body["contentUrl"]
	assert.True(t, hasContentURL, "response must contain contentUrl")
	_, hasHostURL := body["hostUrl"]
	assert.True(t, hasHostURL, "response must contain hostUrl")
	assert.Len(t, body, 2, "response must contain only the hostUrl and contentUrl keys")
}
