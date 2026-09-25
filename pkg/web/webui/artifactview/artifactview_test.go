package artifactview_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
)

const trustedHost = "trusted.example"
const sandboxHost = "sandbox.example"

// bakedCSP mirrors pkg/channels/channelassets/html's cspContent EXACTLY. The content
// endpoint hands render bytes to the kind's ServeTransform, which rewrites this
// string; the fake's ServeTransform here is a passthrough (the rewrite itself
// is unit-tested in the html package), so these tests only assert the content
// flow, not the CSP edit.
const bakedCSP = `default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline' 'self'; font-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox allow-same-origin;`

// fakeDeps implements artifactview.Deps via per-behavior func fields so each
// test overrides only the one or two it exercises. The methods satisfy the
// interface; an unset func field would panic, but fakeAV() populates the
// happy-path defaults so the shell + content flows succeed end to end.
type fakeDeps struct {
	verifyLink          func(raw string) (string, string, string, error)
	checkView           func(ctx context.Context, artifactID, subject string) (bool, error)
	checkInteract       func(ctx context.Context, ns, sess, subject string) (bool, error)
	artifactMeta        func(ctx context.Context, ns, sess, artifactID string) (string, string, error)
	channelKind         func(ctx context.Context, ns, sess string) (string, error)
	sessionViews        func(ctx context.Context, ns, sess string) []string
	resolveRender       func(ctx context.Context, ns, sess, artifactID string) (string, error)
	contentRender       func(ctx context.Context, ns, sess, artifactID string) (string, bool, error)
	previewChildRender  func(ctx context.Context, ns, sess, revID string) (string, bool, error)
	renderIsBundledOnly func(ctx context.Context, ns, sess, artifactID string) bool
	signContentToken    func(ns, sess, renderName, artifactID string) (string, error)
	verifyContentToken  func(token string) (string, string, string, error)
	resolveAssetURL     func(ctx context.Context, ns, sess, handle string) (string, bool, error)
	renderKind          func(ctx context.Context, ns, sess, renderName string) (string, error)
	verifyAssetToken    func(token string) (string, string, string, error)
	fetchRender         func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error)
	fetchRenderBundle   func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error)
	serveTransform      func(ctx context.Context, ns, sess, renderName string, content []byte) []byte
	listRevisions       func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error)
	trustedOrigin       func() string
	sandboxBaseURL      func() string
	logger              logr.Logger
}

func (f *fakeDeps) VerifyLink(raw string) (string, string, string, error) { return f.verifyLink(raw) }
func (f *fakeDeps) CheckView(ctx context.Context, artifactID, subject string) (bool, error) {
	return f.checkView(ctx, artifactID, subject)
}

// CheckInteract defaults to "a session participant" — the ordinary viewer these
// shell/content tests model. Only the mirrors consult it, so the shell flows are
// unaffected either way; a test covering the platform-admin case sets the field.
func (f *fakeDeps) CheckInteract(ctx context.Context, ns, sess, subject string) (bool, error) {
	if f.checkInteract == nil {
		return true, nil
	}
	return f.checkInteract(ctx, ns, sess, subject)
}
func (f *fakeDeps) ArtifactMeta(ctx context.Context, ns, sess, artifactID string) (string, string, error) {
	if f.artifactMeta == nil {
		return "", "", nil
	}
	return f.artifactMeta(ctx, ns, sess, artifactID)
}
func (f *fakeDeps) ChannelKind(ctx context.Context, ns, sess string) (string, error) {
	if f.channelKind == nil {
		return "", nil
	}
	return f.channelKind(ctx, ns, sess)
}
func (f *fakeDeps) ResolveRender(ctx context.Context, ns, sess, artifactID string) (string, error) {
	return f.resolveRender(ctx, ns, sess, artifactID)
}

// SessionViews defaults to nil (no session_views grant → read-only viewer),
// matching the "absent capability" happy-path default; a test exercising a
// granted class sets f.sessionViews directly.
func (f *fakeDeps) SessionViews(ctx context.Context, ns, sess string) []string {
	if f.sessionViews == nil {
		return nil
	}
	return f.sessionViews(ctx, ns, sess)
}

// ContentRender defaults to the standalone behavior (frame the newest render,
// ready=true) so existing tests are unchanged. When listRevisions is set it
// mirrors the real standalone resolver (newest revision's render); otherwise it
// falls back to resolveRender. A test exercising the bundled-only generating
// state sets f.contentRender directly.
func (f *fakeDeps) ContentRender(ctx context.Context, ns, sess, artifactID string) (string, bool, error) {
	if f.contentRender != nil {
		return f.contentRender(ctx, ns, sess, artifactID)
	}
	if f.listRevisions != nil {
		revs, err := f.listRevisions(ctx, ns, sess, artifactID)
		if err != nil {
			return "", false, err
		}
		if len(revs) == 0 {
			return "", false, nil
		}
		return revs[len(revs)-1].RenderName, true, nil
	}
	rn, err := f.resolveRender(ctx, ns, sess, artifactID)
	if err != nil {
		return "", false, err
	}
	return rn, true, nil
}
func (f *fakeDeps) RenderIsBundledOnly(ctx context.Context, ns, sess, artifactID string) bool {
	if f.renderIsBundledOnly == nil {
		return false // default: standalone
	}
	return f.renderIsBundledOnly(ctx, ns, sess, artifactID)
}

// PreviewChildRender defaults to "no preview generated yet" (ready=false). A
// test exercising an older bundled-only revision's preview sets
// f.previewChildRender directly.
func (f *fakeDeps) PreviewChildRender(ctx context.Context, ns, sess, revID string) (string, bool, error) {
	if f.previewChildRender == nil {
		return "", false, nil
	}
	return f.previewChildRender(ctx, ns, sess, revID)
}
func (f *fakeDeps) SignContentToken(ns, sess, renderName, artifactID string) (string, error) {
	return f.signContentToken(ns, sess, renderName, artifactID)
}
func (f *fakeDeps) VerifyContentToken(token string) (string, string, string, error) {
	return f.verifyContentToken(token)
}

// ResolveAssetURL defaults to "unresolvable" (ok=false, nil error) — the safe
// default for tests that don't exercise asset-ref rewriting. A test that does
// sets f.resolveAssetURL directly.
func (f *fakeDeps) ResolveAssetURL(ctx context.Context, ns, sess, handle string) (string, bool, error) {
	if f.resolveAssetURL == nil {
		return "", false, nil
	}
	return f.resolveAssetURL(ctx, ns, sess, handle)
}

// RenderKind defaults to "html" — every content-rewrite test in this file
// predates per-kind dispatch and implicitly assumed html; a test proving
// genericity (a kind without a RefRewriter) sets f.renderKind directly.
func (f *fakeDeps) RenderKind(ctx context.Context, ns, sess, renderName string) (string, error) {
	if f.renderKind == nil {
		return "html", nil
	}
	return f.renderKind(ctx, ns, sess, renderName)
}

// VerifyAssetToken defaults to a fixed happy-path (ns, sess, renderName) —
// mirrors verifyContentToken's role in fakeAV. A test that exercises the
// asset route's own token verification sets f.verifyAssetToken directly.
func (f *fakeDeps) VerifyAssetToken(token string) (string, string, string, error) {
	if f.verifyAssetToken == nil {
		return "default", "s1", "ar-1", nil
	}
	return f.verifyAssetToken(token)
}
func (f *fakeDeps) FetchRender(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
	return f.fetchRender(ctx, ns, sess, renderName)
}

// FetchRenderBundle defaults to FetchRender's own behavior — the "nothing to
// bundle" case every existing test implicitly exercises — so only a test
// that specifically cares about the bundle-vs-raw distinction needs to set
// f.fetchRenderBundle.
func (f *fakeDeps) FetchRenderBundle(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
	if f.fetchRenderBundle != nil {
		return f.fetchRenderBundle(ctx, ns, sess, renderName)
	}
	return f.fetchRender(ctx, ns, sess, renderName)
}
func (f *fakeDeps) ServeTransform(ctx context.Context, ns, sess, renderName string, content []byte) []byte {
	if f.serveTransform != nil {
		return f.serveTransform(ctx, ns, sess, renderName, content)
	}
	return content
}

// ListRevisions is what bindView probes to resolve an artifact's owning
// session, so EVERY handler reaches it. It defaults to a single-revision list
// for the artifact the happy-path link names, in the session that link names —
// the "the pair is bound" answer these shell/content tests all assume. A test
// exercising an unbound pair, or the revision sidebar itself, sets the field.
func (f *fakeDeps) ListRevisions(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
	if f.listRevisions == nil {
		return []artifactview.RevisionMeta{{Seq: 1, RevisionID: "rev-1", RenderName: "ar-1", Tags: []string{"latest"}}}, nil
	}
	return f.listRevisions(ctx, ns, sess, artifactID)
}
func (f *fakeDeps) WatchSessionStatus(ctx context.Context, ns, sess string) (<-chan artifactview.StatusSnapshot, error) {
	return nil, nil // shell tests don't exercise the live status stream
}
func (f *fakeDeps) WatchMessages(ctx context.Context, ns, sess string) (<-chan artifactview.MirrorMessage, error) {
	return nil, nil // shell tests don't exercise the chat mirror stream
}
func (f *fakeDeps) TrustedOrigin() string  { return f.trustedOrigin() }
func (f *fakeDeps) SandboxBaseURL() string { return f.sandboxBaseURL() }
func (f *fakeDeps) Logger() logr.Logger    { return f.logger }

// fakeAV returns a fully-populated fakeDeps whose happy-path values let the
// shell + content flows succeed end to end. Each test overrides only the one or
// two fields it exercises.
func fakeAV() *fakeDeps {
	return &fakeDeps{
		verifyLink: func(raw string) (string, string, string, error) {
			return "artifact-x", "default/s1", "", nil
		},
		checkView: func(ctx context.Context, artifactID, subject string) (bool, error) {
			return true, nil
		},
		channelKind: func(ctx context.Context, ns, sess string) (string, error) {
			return "slack", nil
		},
		resolveRender: func(ctx context.Context, ns, sess, artifactID string) (string, error) {
			return "ar-1", nil
		},
		signContentToken: func(ns, sess, renderName, artifactID string) (string, error) {
			return "tok", nil
		},
		verifyContentToken: func(token string) (string, string, string, error) {
			return "default", "s1", "ar-1", nil
		},
		fetchRender: func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
			return []byte(`<html><head><meta http-equiv="Content-Security-Policy" content="` + bakedCSP + `"></head><body>hi</body></html>`), "text/html", nil
		},
		trustedOrigin:  func() string { return "https://trusted.example" },
		sandboxBaseURL: func() string { return "https://sandbox.example" },
	}
}

// newServer wires the artifactview WebUI into a real webui.Server whose
// authenticate func always authenticates the given subject, handing av to the
// server as the opaque deps the artifactview WebUI casts to its Deps.
func newServer(t *testing.T, subject string, av artifactview.Deps) *webui.Server {
	t.Helper()
	authenticate := func(r *http.Request) (string, bool) { return subject, true }
	s, err := webui.NewServer(authenticate, nil,
		func() string { return trustedHost }, func() string { return sandboxHost },
		nil, av, []webui.WebUI{artifactview.New()})
	require.NoError(t, err)
	return s
}

func req(host, path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	r.Host = host
	return r
}

func TestShell_HappyPath_EmbedsContentURLAndGatesOnSubject(t *testing.T) {
	av := fakeAV()
	var gotArtifactID, gotSubject string
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		gotArtifactID, gotSubject = artifactID, subject
		return true, nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// The iframe is now client-rendered by the React app; the server hands it the
	// minted content URL through the bootstrap props instead of an inline <iframe>.
	assert.Contains(t, body, `data-app="artifact-view"`, "shell must mount the artifact-view React app")
	assert.Contains(t, body, `"contentUrl":"`, "bootstrap props must carry the content URL")
	assert.Contains(t, body, "https://sandbox.example/content?ct=tok", "content URL in props must point at the sandbox content endpoint")
	assert.Equal(t, "user:abc", gotSubject, "CheckView must run with the authenticated subject")
	assert.Equal(t, "artifact-x", gotArtifactID, "CheckView must run with the verified artifact id")
}

// TestDownload_HappyPath_ServesBytesAsAttachment verifies the direct-download
// endpoint gates on CheckView (like the view), then serves the render bytes
// with Content-Disposition: attachment (forces download — untrusted agent HTML
// never renders in webd's origin) + nosniff, with the filename from ?fn.
func TestDownload_HappyPath_ServesBytesAsAttachment(t *testing.T) {
	av := fakeAV()
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		return []byte("<html>one-pager</html>"), "text/html; charset=utf-8", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-download?d=AA&sig=BB&fn=CISO-One-Pager.html"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "<html>one-pager</html>", rec.Body.String())
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	cd := rec.Header().Get("Content-Disposition")
	assert.Contains(t, cd, "attachment", "must force download, never inline render")
	assert.Contains(t, cd, `filename="CISO-One-Pager.html"`, "filename comes from ?fn")
}

// TestDownload_FilenameFallbackFromMIME: with no ?fn the filename derives from
// the artifact id + a MIME-based extension, sanitized.
func TestDownload_FilenameFallbackFromMIME(t *testing.T) {
	av := fakeAV()
	av.verifyLink = func(raw string) (string, string, string, error) { return "art-xyz", "default/s1", "", nil }
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		return []byte("data"), "text/html", nil
	}
	s := newServer(t, "user:abc", av)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-download?d=AA&sig=BB"))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Disposition"), `filename="art-xyz.html"`)
}

// TestDownload_SpecificRevision: ?rev serves that revision's render, not the
// head — so a per-revision download icon downloads exactly that revision.
func TestDownload_SpecificRevision(t *testing.T) {
	av := fakeAV()
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
		return []artifactview.RevisionMeta{
			{RevisionID: "rev-1", RenderName: "ar-old"},
			{RevisionID: "rev-2", RenderName: "ar-new"},
		}, nil
	}
	var fetched string
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		fetched = renderName
		return []byte("old bytes"), "text/html", nil
	}
	s := newServer(t, "user:abc", av)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-download?artifactId=art&sessionRef=default/s1&rev=rev-1"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ar-old", fetched, "must fetch the requested revision's render, not the head")
	assert.Equal(t, "old bytes", rec.Body.String())
}

// TestDownload_RevisionNotFound: an unknown ?rev is a 404, not the head.
func TestDownload_RevisionNotFound(t *testing.T) {
	av := fakeAV()
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
		return []artifactview.RevisionMeta{{RevisionID: "rev-1", RenderName: "ar-old"}}, nil
	}
	s := newServer(t, "user:abc", av)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-download?artifactId=art&sessionRef=default/s1&rev=nope"))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestDownload_BundledZip_ContentTypeAndFilename: an html primary with
// resolvable artifact: refs comes back from FetchRenderBundle as
// application/zip. The download must reflect the ACTUAL delivered type —
// Content-Type: application/zip and a .zip filename — even when ?fn still
// carries the un-bundled ".html" hint minted before the server's per-request
// bundle-vs-raw decision.
func TestDownload_BundledZip_ContentTypeAndFilename(t *testing.T) {
	av := fakeAV()
	var gotArgs [3]string
	av.fetchRenderBundle = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		gotArgs = [3]string{ns, sess, renderName}
		return []byte("PK\x03\x04fake-zip-bytes"), "application/zip", nil
	}
	// fetchRender must NOT be consulted on the download path once
	// fetchRenderBundle is set — prove downloadHandler calls the bundle
	// method, not FetchRender, by making the latter fail the test if hit.
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		t.Fatal("downloadHandler must call FetchRenderBundle, not FetchRender")
		return nil, "", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-download?d=AA&sig=BB&fn=report.html"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "PK\x03\x04fake-zip-bytes", rec.Body.String())
	assert.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	cd := rec.Header().Get("Content-Disposition")
	assert.Contains(t, cd, "attachment")
	assert.Contains(t, cd, `filename="report.zip"`, "the stale .html hint's extension must be replaced with .zip")
	assert.Equal(t, [3]string{"default", "s1", "ar-1"}, gotArgs, "FetchRenderBundle must receive the resolved (ns, sess, renderName)")
}

// TestDownload_RefLessHTML_UnchangedFromFetchRender: an html primary with no
// refs (or a non-html primary) has nothing to bundle — FetchRenderBundle
// returns the exact same bytes+MIME FetchRender would have, and the
// existing filename/content-type behavior is unaffected. Proven by NOT
// setting av.fetchRenderBundle at all — fakeDeps.FetchRenderBundle falls
// back to fetchRender, and TestDownload_HappyPath_ServesBytesAsAttachment /
// TestDownload_FilenameFallbackFromMIME already assert that path's output
// byte-for-byte, so this test only needs to assert the fallback is reached.
func TestDownload_RefLessHTML_UnchangedFromFetchRender(t *testing.T) {
	av := fakeAV()
	called := false
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		called = true
		return []byte("<html>plain</html>"), "text/html; charset=utf-8", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-download?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, called, "FetchRenderBundle with no override must fall back to FetchRender's behavior")
	assert.Equal(t, "<html>plain</html>", rec.Body.String())
	assert.NotEqual(t, "application/zip", rec.Header().Get("Content-Type"))
}

// TestDownload_CheckViewFalse_Forbidden: the download is gated by the same
// CheckView as the live view — no view access, no bytes.
func TestDownload_CheckViewFalse_Forbidden(t *testing.T) {
	av := fakeAV()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) { return false, nil }
	s := newServer(t, "user:abc", av)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-download?d=AA&sig=BB"))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestMeta_NewestRevisionFacts: the meta route answers the newest revision's
// file facts (what ap:attachment shows), and the artifact's own name.
func TestMeta_NewestRevisionFacts(t *testing.T) {
	av := fakeAV()
	av.artifactMeta = func(ctx context.Context, ns, sess, artifactID string) (string, string, error) {
		return "demo-agent draft", "", nil
	}
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
		return []artifactview.RevisionMeta{
			{Seq: 1, RevisionID: "rev-1", RenderName: "ar-a", Filename: "old.oap", Size: 10, MIME: "application/x-old"},
			{Seq: 2, RevisionID: "rev-2", RenderName: "ar-b", Filename: "demo-agent.oap", Size: 4096, MIME: "application/vnd.agentprimitives.authzed.com.agent.v1"},
		}, nil
	}
	s := newServer(t, "user:abc", av)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/meta?d=AA&sig=BB"))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"name":"demo-agent draft","filename":"demo-agent.oap","size":4096,"mime":"application/vnd.agentprimitives.authzed.com.agent.v1"}`, rec.Body.String())
}

// TestMeta_NoRevisions_404: an artifact with no rendered revision has no file
// facts to answer with.
func TestMeta_NoRevisions_404(t *testing.T) {
	av := fakeAV()
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
		return nil, nil
	}
	s := newServer(t, "user:abc", av)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/meta?d=AA&sig=BB"))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestMeta_CheckViewFalse_Forbidden: gated by the same CheckView as the
// download — a page fill naming another session's artifact gets nothing.
func TestMeta_CheckViewFalse_Forbidden(t *testing.T) {
	av := fakeAV()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) { return false, nil }
	s := newServer(t, "user:abc", av)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view/meta?d=AA&sig=BB"))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestShellPageBuild_ProvidesHostURL verifies the shell now frames the
// sandbox-origin /artifact-host page (plan D1) instead of pointing straight at
// /content, alongside the existing contentUrl (Task 6 decides ContentURL's
// client-side fate; the server keeps emitting both for now).
func TestShellPageBuild_ProvidesHostURL(t *testing.T) {
	av := fakeAV()
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"hostUrl":"`, "bootstrap props must carry the host URL")
	assert.Contains(t, body, "https://sandbox.example/artifact-host?ct=tok", "shell frames the host, not /content directly")
	assert.Contains(t, body, "https://sandbox.example/content?ct=tok", "bootstrap props must still carry the content URL")
}

func TestShell_ShowsArtifactTitleAndDescription(t *testing.T) {
	av := fakeAV()
	av.artifactMeta = func(ctx context.Context, ns, sess, artifactID string) (string, string, error) {
		return "Quarterly Report", "Q3 revenue breakdown", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"artifactName":"Quarterly Report"`, "bootstrap props carry the artifact name, not the id")
	assert.Contains(t, body, `"artifactDescription":"Q3 revenue breakdown"`, "bootstrap props carry the artifact description")
	assert.Contains(t, body, "<title>Live view — Quarterly Report</title>", "document title carries the artifact name")
}

func TestShell_RendersBackLinkWhenPresent(t *testing.T) {
	av := fakeAV()
	av.verifyLink = func(raw string) (string, string, string, error) {
		return "artifact-x", "default/s1", "https://example.slack.com/archives/C1/p170?thread_ts=170&cid=C1", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// The back link is now passed to the client via props (json key "backLink");
	// the React app renders the "Back to thread" affordance. The bootstrap JSON is
	// HTML-escaped, so the URL's "&" appears as "&" — match the stable prefix.
	assert.Contains(t, body, `"backLink":"https://example.slack.com/archives/C1/p170`, "bootstrap props carry the back-link URL")
}

func TestShell_OmitsBackLinkWhenEmpty(t *testing.T) {
	av := fakeAV() // fakeAV's verifyLink returns "" back-link
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"backLink":""`, "bootstrap props carry an empty back-link when none is set")
}

func TestShell_FallsBackToArtifactIDWhenNoName(t *testing.T) {
	av := fakeAV() // artifactMeta unset → ("","",nil)
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"artifactName":"artifact-x"`, "bootstrap props fall back to the artifact id when the head has no name")
}

// TestShell_SessionViewsAbsent_InteractionsEmpty covers the default shape:
// fakeAV's sessionViews is unset (nil), mirroring an AgentClass with no
// session_views grant. The shell must still mount, with an empty
// interactions list — the client renders a read-only viewer, unchanged from
// today.
func TestShell_SessionViewsAbsent_InteractionsEmpty(t *testing.T) {
	av := fakeAV()
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"interactions":[]`,
		"bootstrap props carry an empty interactions list when session_views is absent")
}

// TestShell_SessionViewsGranted_InteractionsListed covers a class that
// grants session_views with the user_message interaction: the shell's
// bootstrap props must carry it verbatim so the client shows the chat.
func TestShell_SessionViewsGranted_InteractionsListed(t *testing.T) {
	av := fakeAV()
	var gotNs, gotSess string
	av.sessionViews = func(ctx context.Context, ns, sess string) []string {
		gotNs, gotSess = ns, sess
		return []string{"user_message"}
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"interactions":["user_message"]`,
		"bootstrap props carry the granted interaction kinds")
	assert.Equal(t, "default", gotNs, "SessionViews must be called with the session's namespace")
	assert.Equal(t, "s1", gotSess, "SessionViews must be called with the session's name")
}

func TestShell_HappyPath_RendersReactAppWithNonceCSPFramingSandbox(t *testing.T) {
	av := fakeAV()
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// The framework emits CSP as an HTTP response header (so frame-ancestors /
	// form-action are honored — they're ignored in a <meta>). The strict policy
	// uses a per-request script nonce and frames the sandbox host for the iframe.
	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "script-src 'nonce-", "CSP must use a per-request script nonce")
	assert.NotContains(t, csp, "script-src 'unsafe-inline'", "script-src must not fall back to unsafe-inline")
	assert.Contains(t, csp, "style-src 'self' 'unsafe-inline'", "style-src must allow the external /assets stylesheet")
	assert.Contains(t, csp, "connect-src 'self'", "CSP must allow the same-origin websocket")
	assert.Contains(t, csp, "frame-src "+sandboxHost, "CSP must frame the sandbox content origin")
	assert.Contains(t, csp, "frame-ancestors 'none'", "CSP header form makes frame-ancestors effective")
	assert.NotContains(t, body, `http-equiv="Content-Security-Policy"`, "CSP must not be duplicated in a <meta>")
	assert.Contains(t, body, `<script type="module" src="/assets/`, "the document must load the React app bundle")
	assert.Contains(t, body, `data-app="artifact-view"`, "the document mounts the artifact-view app")
}

func TestShell_Forbidden_DoesNotLeakContentURL(t *testing.T) {
	av := fakeAV()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) { return false, nil }
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="system"`, "a denial renders the styled system page")
	assert.NotContains(t, body, "/content?ct=", "a denied request must not leak the content URL/token")
	assert.NotContains(t, body, "/artifact-host?ct=", "a denied request must not leak the host URL/token")
}

func TestShell_BadLink_Forbidden(t *testing.T) {
	av := fakeAV()
	av.verifyLink = func(raw string) (string, string, string, error) {
		return "", "", "", assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestShell_UnsignedParams_AdminPath is the platform-admin viewer path: no signed
// d/sig link, just artifactId+sessionRef query params. The shell must read them
// directly (NOT call VerifyLink) and reach CheckView with the authenticated
// cookie subject + the query artifact id — Part A's SpiceDB platform->view_audit
// tie makes CheckView pass for an admin, so no signature is needed.
func TestShell_UnsignedParams_AdminPath(t *testing.T) {
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
	s := newServer(t, "user:admin", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?artifactId=artifact-x&sessionRef=default%2Fs1"))

	require.Equal(t, http.StatusOK, rec.Code, "unsigned admin params must reach the happy path")
	assert.False(t, verifyLinkCalled, "the unsigned path must not call VerifyLink")
	assert.Equal(t, "user:admin", gotSubject, "CheckView runs with the authenticated cookie subject")
	assert.Equal(t, "artifact-x", gotArtifactID, "CheckView runs with the query artifact id")
	assert.Contains(t, rec.Body.String(), `data-app="artifact-view"`, "shell mounts the React app")
	assert.Contains(t, rec.Body.String(), `"backLink":""`, "the unsigned admin path carries no back link")
}

// TestShell_UnsignedParams_ForbiddenStillGated confirms the unsigned path is NOT
// a bypass: CheckView still gates it. A non-admin whose CheckView returns false
// is denied even with valid-looking params.
func TestShell_UnsignedParams_ForbiddenStillGated(t *testing.T) {
	av := fakeAV()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) { return false, nil }
	s := newServer(t, "user:nobody", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?artifactId=artifact-x&sessionRef=default%2Fs1"))

	assert.Equal(t, http.StatusForbidden, rec.Code, "the unsigned path is still gated by CheckView")
	assert.NotContains(t, rec.Body.String(), "/content?ct=", "a denied unsigned request must not leak the content URL")
}

// TestShell_NoLinkNoParams_Forbidden verifies that a request with NEITHER a
// signed link NOR both unsigned params is a 403 — the unsigned path requires
// both artifactId and sessionRef, and a partial one must not fall through.
func TestShell_NoLinkNoParams_Forbidden(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{name: "no params at all: 403", query: "/artifact-view"},
		{name: "artifactId only (no sessionRef): 403", query: "/artifact-view?artifactId=artifact-x"},
		{name: "sessionRef only (no artifactId): 403", query: "/artifact-view?sessionRef=default%2Fs1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			av := fakeAV()
			var checkViewCalled bool
			av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
				checkViewCalled = true
				return true, nil
			}
			s := newServer(t, "user:admin", av)

			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req(trustedHost, tc.query))

			assert.Equal(t, http.StatusForbidden, rec.Code)
			assert.False(t, checkViewCalled, "an incomplete request must be rejected before CheckView")
		})
	}
}

// TestShell_SignedPath_TakesPrecedence confirms the signed path is unchanged: a
// request carrying d/sig verifies via VerifyLink and does NOT read the unsigned
// artifactId param even when one is also present.
func TestShell_SignedPath_TakesPrecedence(t *testing.T) {
	av := fakeAV()
	var verifiedRaw string
	av.verifyLink = func(raw string) (string, string, string, error) {
		verifiedRaw = raw
		return "signed-artifact", "default/s1", "", nil
	}
	var gotArtifactID string
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		gotArtifactID = artifactID
		return true, nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB&artifactId=unsigned-artifact"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "AA.BB", verifiedRaw, "the signed d.sig link is verified verbatim")
	assert.Equal(t, "signed-artifact", gotArtifactID, "the signed link's artifact id wins over the unsigned param")
}

// TestShell_NotFound_ArtifactExpired verifies that a ResolveRender error
// wrapping artifacts.ErrNotFound (artifact missing from memory, e.g. after
// an in-memory operator restart) returns 404 rather than 500.
func TestShell_NotFound_ArtifactExpired(t *testing.T) {
	av := fakeAV()
	av.resolveRender = func(ctx context.Context, ns, sess, artifactID string) (string, error) {
		return "", fmt.Errorf("artifacts: artifact %q not found in this session: %w", artifactID, artifacts.ErrNotFound)
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Artifact unavailable", "404 page must carry the not-found title")
}

// TestShell_EmptyRender_MountsWithEmptyContentURL covers a bundled-only artifact
// (svg/css) whose internal preview child is still generating: ResolveRender
// returns ("", nil). The shell must mount its React app with an empty contentUrl
// (its loading state) — NOT mint a content token for raw bytes and NOT error.
// The live-view ws poll fills in the content URL when the preview child lands.
func TestShell_EmptyRender_MountsWithEmptyContentURL(t *testing.T) {
	av := fakeAV()
	av.resolveRender = func(ctx context.Context, ns, sess, artifactID string) (string, error) {
		return "", nil // nothing to frame yet (preview generating)
	}
	var signCalled bool
	av.signContentToken = func(ns, sess, renderName, artifactID string) (string, error) {
		signCalled = true
		return "tok", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	require.Equal(t, http.StatusOK, rec.Code, "an empty render is the loading state, not an error")
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="artifact-view"`, "shell still mounts the React app")
	assert.Contains(t, body, `"hostUrl":""`, "no host URL is minted while content is unavailable")
	assert.Contains(t, body, `"contentUrl":""`, "no content URL is minted while content is unavailable")
	assert.NotContains(t, body, "/content?ct=", "no content token must leak while generating")
	assert.NotContains(t, body, "/artifact-host?ct=", "no host token must leak while generating")
	assert.False(t, signCalled, "no content token is minted for an empty render")
}

func TestContent_HappyPath_DelegatesToServeTransform(t *testing.T) {
	av := fakeAV()
	var gotNs, gotSess, gotRender string
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		gotNs, gotSess, gotRender = ns, sess, renderName
		return []byte(`<html><head></head><body>hi</body></html>`), "text/html", nil
	}
	// The kind owns the transform; the handler just dispatches. Assert it
	// passes the fetched bytes through ServeTransform and serves the result
	// (the transform itself is unit-tested in the html kind).
	av.serveTransform = func(ctx context.Context, ns, sess, renderName string, content []byte) []byte {
		return append(content, []byte("<!--transformed-->")...)
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/content?ct=tok"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<!--transformed-->", "handler must serve the kind's ServeTransform output")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "default", gotNs)
	assert.Equal(t, "s1", gotSess)
	assert.Equal(t, "ar-1", gotRender)
}

// TestContent_RewritesArtifactRefs_ToAssetURL is the end-to-end integration
// test for the live-view asset-ref rewrite: a primary render containing an
// `artifact:HANDLE` reference on img[src] gets that reference rewritten to
// the resolver's minted /artifacts/a/ URL before being served — proving
// contentHandler actually wires rewriteArtifactRefs + av.ResolveAssetURL, not
// just that rewriteArtifactRefs works in isolation (see rewrite_test.go).
func TestContent_RewritesArtifactRefs_ToAssetURL(t *testing.T) {
	av := fakeAV()
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		return []byte(`<html><head></head><body><img src="artifact:ar-secondary"></body></html>`), "text/html", nil
	}
	var gotNs, gotSess, gotHandle string
	av.resolveAssetURL = func(ctx context.Context, ns, sess, handle string) (string, bool, error) {
		gotNs, gotSess, gotHandle = ns, sess, handle
		return "/artifacts/a/?ct=MINTED", true, nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/content?ct=tok"))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `src="/artifacts/a/?ct=MINTED"`, "the artifact: ref must be rewritten to the resolved asset URL")
	assert.NotContains(t, body, "artifact:", "no artifact: scheme must reach the browser")
	assert.Equal(t, "default", gotNs, "resolution must be scoped to the primary's own ns")
	assert.Equal(t, "s1", gotSess, "resolution must be scoped to the primary's own session")
	assert.Equal(t, "ar-secondary", gotHandle)
}

// TestContent_UnresolvedArtifactRef_DropsAttrStillServes200 proves the
// same-session boundary at the handler level: a handle ResolveAssetURL can't
// resolve (e.g. it belongs to a different session) must not fail the
// request — the primary is still served 200, just without that one
// attribute.
func TestContent_UnresolvedArtifactRef_DropsAttrStillServes200(t *testing.T) {
	av := fakeAV()
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		return []byte(`<html><head></head><body><p>still here</p><img src="artifact:ar-other-session"></body></html>`), "text/html", nil
	}
	av.resolveAssetURL = func(ctx context.Context, ns, sess, handle string) (string, bool, error) {
		return "", false, nil // outside (ns,sess)
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/content?ct=tok"))

	require.Equal(t, http.StatusOK, rec.Code, "an unresolvable ref must not fail the whole primary")
	body := rec.Body.String()
	assert.NotContains(t, body, "artifact:")
	assert.Contains(t, body, "still here")
}

func TestContent_BadToken_Forbidden(t *testing.T) {
	av := fakeAV()
	av.verifyContentToken = func(token string) (string, string, string, error) {
		return "", "", "", assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/content?ct=bad"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestShell_ResolveRenderError_InternalServerError(t *testing.T) {
	av := fakeAV()
	av.resolveRender = func(ctx context.Context, ns, sess, artifactID string) (string, error) {
		return "", assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestShell_CheckViewError_InternalServerError(t *testing.T) {
	av := fakeAV()
	av.checkView = func(ctx context.Context, artifactID, subject string) (bool, error) {
		return false, assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestShell_SignContentTokenError_InternalServerError(t *testing.T) {
	av := fakeAV()
	av.signContentToken = func(ns, sess, renderName, artifactID string) (string, error) {
		return "", assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?d=AA&sig=BB"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestContent_FetchRenderError_BadGateway(t *testing.T) {
	av := fakeAV()
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		return nil, "", assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/content?ct=tok"))

	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// TestContent_TextHTML_ForcesUTF8Charset is the regression guard for a mojibake
// bug in the live view: the renderer strips the <meta charset> tag (head/meta
// sanitization) and the operator returns "text/html" with NO charset, while the
// handler sends X-Content-Type-Options: nosniff. With no explicit HTTP charset
// and sniffing disabled, the browser falls back to Latin-1 and renders UTF-8
// (e.g. "·" = bytes C2 B7) as "Â·". The handler MUST add charset=utf-8.
func TestContent_TextHTML_ForcesUTF8Charset(t *testing.T) {
	av := fakeAV()
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		return []byte("check · list · revoke"), "text/html", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/content?ct=tok"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"),
		"a text/html render must be served with an explicit utf-8 charset")
}

// TestContent_NonTextMIME_CharsetUntouched verifies a non-text render (e.g. an
// image) is served verbatim — no charset is appended.
func TestContent_NonTextMIME_CharsetUntouched(t *testing.T) {
	av := fakeAV()
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		return []byte{0x89, 'P', 'N', 'G'}, "image/png", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/content?ct=tok"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"),
		"non-text MIME must not get a charset appended")
}

// TestUnboundSessionRef_RefusedOnEveryRoute pins the artifact→session binding
// on every route that takes its target from the request. artifact-x lives in
// default/s1 and the subject may view it, but each request names a DIFFERENT
// session in the unsigned sessionRef param. Nothing about artifact-x authorizes
// default/victim, so every route must refuse before it runs a single
// session-scoped lookup — otherwise the shell hands the browser the victim's
// (ns, name) to post interactions against, and the revision/download routes
// read bytes out of a session the caller was never authorized for.
func TestUnboundSessionRef_RefusedOnEveryRoute(t *testing.T) {
	const victim = "artifactId=artifact-x&sessionRef=default%2Fvictim"
	cases := []struct {
		name string
		path string
	}{
		{name: "shell: 403 system page, no session-scoped lookup", path: "/artifact-view?" + victim},
		{name: "revision: 403, never resolves a revision", path: "/artifact-view/revision?" + victim + "&rev=rev-1"},
		{name: "download: 403, never fetches bytes", path: "/artifact-download?" + victim},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			av := fakeAV()
			// The real store resolves the head only inside
			// memory.Scope{ID: ns+"/"+sess} and hard-errors elsewhere.
			av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
				if ns != "default" || sess != "s1" {
					return nil, errors.New(`artifacts: artifact "artifact-x" not found`)
				}
				return []artifactview.RevisionMeta{{Seq: 1, RevisionID: "rev-1", RenderName: "ar-1"}}, nil
			}
			var touched []string
			av.channelKind = func(ctx context.Context, ns, sess string) (string, error) {
				touched = append(touched, "ChannelKind "+ns+"/"+sess)
				return "slack", nil
			}
			av.artifactMeta = func(ctx context.Context, ns, sess, artifactID string) (string, string, error) {
				touched = append(touched, "ArtifactMeta "+ns+"/"+sess)
				return "", "", nil
			}
			av.sessionViews = func(ctx context.Context, ns, sess string) []string {
				touched = append(touched, "SessionViews "+ns+"/"+sess)
				return nil
			}
			av.resolveRender = func(ctx context.Context, ns, sess, artifactID string) (string, error) {
				touched = append(touched, "ResolveRender "+ns+"/"+sess)
				return "ar-1", nil
			}
			av.fetchRenderBundle = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
				touched = append(touched, "FetchRenderBundle "+ns+"/"+sess)
				return []byte("victim bytes"), "text/html", nil
			}
			s := newServer(t, "user:abc", av)

			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req(trustedHost, tc.path))

			assert.Equal(t, http.StatusForbidden, rec.Code,
				"an artifact that does not live in the requested session must be refused")
			assert.Empty(t, touched, "no session-scoped lookup may run on an unbound pair")
			assert.NotContains(t, rec.Body.String(), "victim bytes")
		})
	}
}

// TestShell_BoundSessionRef_PropsCarryTheResolvedSession is the positive half of
// the binding: the admin path (unsigned params, no signature) still works, and
// the Ns/Name the shell hands the browser — the pair its /interact posts are
// stamped with — are the ones the server resolved, not a client-supplied echo.
func TestShell_BoundSessionRef_PropsCarryTheResolvedSession(t *testing.T) {
	av := fakeAV()
	av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
		if ns != "default" || sess != "s1" {
			return nil, errors.New(`artifacts: artifact "artifact-x" not found`)
		}
		return []artifactview.RevisionMeta{{Seq: 1, RevisionID: "rev-1", RenderName: "ar-1"}}, nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/artifact-view?artifactId=artifact-x&sessionRef=default%2Fs1"))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"ns":"default"`)
	assert.Contains(t, body, `"name":"s1"`)
}

// TestShell_MalformedSessionRef_Forbidden covers the ref shapes that are not a
// "<ns>/<name>" pair at all. The memory scope id is rebuilt by re-joining the
// two halves, so a ref with a missing or extra separator would address a scope
// that is not the session it appears to name; the binding refuses it outright
// rather than probing with it.
func TestShell_MalformedSessionRef_Forbidden(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{name: "no separator: 403, never probed", ref: "s1"},
		{name: "empty namespace: 403, never probed", ref: "%2Fs1"},
		{name: "empty name: 403, never probed", ref: "default%2F"},
		{name: "extra segment: 403, never probed", ref: "default%2Fs1%2Fextra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			av := fakeAV()
			var probed bool
			av.listRevisions = func(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error) {
				probed = true
				return nil, nil
			}
			s := newServer(t, "user:abc", av)

			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req(trustedHost, "/artifact-view?artifactId=artifact-x&sessionRef="+tc.ref))

			assert.Equal(t, http.StatusForbidden, rec.Code)
			assert.False(t, probed, "a malformed ref must be refused before it reaches the store")
		})
	}
}
