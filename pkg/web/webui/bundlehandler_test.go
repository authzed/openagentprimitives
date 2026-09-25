package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveBundle mounts BundleHandler(appKey) and serves one GET, optionally with
// an If-None-Match header. Fails the test if the entry does not resolve.
func serveBundle(t *testing.T, appKey, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := BundleHandler(appKey)
	require.NoError(t, err, "manifest must resolve the %q entry (run `mage web:build` first)", appKey)
	require.NotNil(t, h)
	req := httptest.NewRequest(http.MethodGet, "/"+appKey+".js", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// Every sandbox-origin host bundle is served by this one handler — artifactview
// mounts /artifact-host.js and sessionview mounts /mcpui-host.js over it — so
// the served shape is asserted once, per real appKey, rather than once per
// package. Both entries must exist in the committed dist: a renamed or unbuilt
// Vite entry silently drops the route at server-build time, and the only visible
// symptom is a 404 on first page load.
func TestBundleHandlerServesEachHostBundle(t *testing.T) {
	for _, appKey := range []string{"artifact-host", "mcpui-host"} {
		t.Run(appKey+": 200 with JS content-type, nosniff, an ETag and a non-empty body", func(t *testing.T) {
			rr := serveBundle(t, appKey, "")

			require.Equal(t, http.StatusOK, rr.Code)
			assert.Contains(t, rr.Header().Get("Content-Type"), "javascript")
			assert.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"),
				"the bundle is executed by the sandbox origin; never let a browser re-sniff its type")
			assert.NotEmpty(t, rr.Body.Bytes(), "the built bundle must not be empty")
			assert.NotEmpty(t, rr.Header().Get("ETag"),
				"serve an ETag so browsers revalidate across builds — the URL is stable, so the ETag is the only version signal")
			assert.Equal(t, "public, max-age=0, must-revalidate", rr.Header().Get("Cache-Control"),
				"a stable URL must never be cached immutably the way /assets is")
		})

		t.Run(appKey+": matching If-None-Match ⇒ 304 with no body", func(t *testing.T) {
			etag := serveBundle(t, appKey, "").Header().Get("ETag")
			require.NotEmpty(t, etag)

			rr := serveBundle(t, appKey, etag)
			assert.Equal(t, http.StatusNotModified, rr.Code)
			assert.Empty(t, rr.Body.Bytes(), "a 304 must not resend the bundle")
		})

		t.Run(appKey+": stale If-None-Match ⇒ 200 with the new body", func(t *testing.T) {
			rr := serveBundle(t, appKey, `"artifact-host-stalehash.js"`)
			assert.Equal(t, http.StatusOK, rr.Code, "a stale ETag must re-download, not 304")
			assert.NotEmpty(t, rr.Body.Bytes())
		})
	}
}

// An entry absent from the manifest must come back as an ERROR, never a nil
// handler. Callers (artifactview.Routes, sessionview.Routes) log it and omit the
// route so the host page's <script src> 404s; a nil handler mounted instead
// would panic the server on first request.
func TestBundleHandlerUnbuiltEntryIsAnError(t *testing.T) {
	h, err := BundleHandler("no-such-vite-entry")
	require.Error(t, err, "an unbuilt/renamed entry must be reported, not tolerated")
	assert.Nil(t, h, "never hand back a handler the caller could mount")
	assert.Contains(t, err.Error(), "no-such-vite-entry",
		"the error must name the missing entry so the fix is obvious from the log line")
}
