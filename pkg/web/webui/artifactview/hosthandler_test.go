package artifactview

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostHandler_ValidToken_ServesHostPageWithCSP(t *testing.T) {
	av := &fakeDeps{trusted: "https://shell.example", verifyOK: true, sessionViews: []string{"user_message", "annotation_batch"}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/artifact-host?ct=TOK123", nil)
	hostHandler(av).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Type"), "text/html")
	assert.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"))
	csp := rr.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "frame-ancestors https://shell.example")
	assert.Contains(t, rr.Body.String(), `src="/content?ct=TOK123"`)
	assert.Contains(t, rr.Body.String(), `src="/artifact-host.js"`, "session_views grants annotation_batch — the bundle must be referenced")
}

func TestHostHandler_InvalidToken_403(t *testing.T) {
	av := &fakeDeps{trusted: "https://shell.example", verifyOK: false}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/artifact-host?ct=bad", nil)
	hostHandler(av).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

// TestHostHandler_NoAnnotationGrant_OmitsAnnotatorBundle is the review-finding
// regression: a session whose class does NOT grant annotation_batch must not
// get a working-looking annotator whose Send then fails closed at /interact.
// hostHandler must resolve SessionViews and omit the config global + bundle
// script when annotation_batch isn't among them, while the read-only swap
// bridge and content frame still work.
func TestHostHandler_NoAnnotationGrant_OmitsAnnotatorBundle(t *testing.T) {
	av := &fakeDeps{trusted: "https://shell.example", verifyOK: true, sessionViews: []string{"user_message"}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/artifact-host?ct=TOK123", nil)
	hostHandler(av).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.NotContains(t, body, "__AP_ANNOT", "annotation_batch not granted — config global must be omitted")
	assert.NotContains(t, body, `src="/artifact-host.js"`, "annotation_batch not granted — bundle script must be omitted")
	assert.Contains(t, body, `src="/content?ct=TOK123"`, "read-only content frame must still be served")
	assert.Contains(t, body, `sandbox="allow-same-origin"`, "inner artifact frame must still be present")
}
