package webui

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderInlineError(t *testing.T) {
	rec := httptest.NewRecorder()
	RenderInlineError(rec, 502, "Could not load", "The render is unavailable.")
	assert.Equal(t, 502, rec.Code)
	body := rec.Body.String()
	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	// Locked-down CSP, but NO frame-ancestors: this page is served inside the
	// artifact content iframe and must remain embeddable by the shell.
	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'none'")
	assert.NotContains(t, csp, "frame-ancestors", "must stay framable inside the artifact shell")
	assert.Empty(t, rec.Header().Get("X-Frame-Options"), "must not block framing by the shell")
	assert.Contains(t, body, "Could not load")
	assert.Contains(t, body, "The render is unavailable.")
	assert.Contains(t, body, "<style>")     // CSS is inlined
	assert.NotContains(t, body, "/assets/") // asset-free (sandbox has no /assets)
}

func TestRenderInlineErrorEscapes(t *testing.T) {
	rec := httptest.NewRecorder()
	RenderInlineError(rec, 403, "<x>", "<script>alert(1)</script>")
	body := rec.Body.String()
	assert.NotContains(t, body, "<script>alert(1)</script>")
	assert.NotContains(t, body, "<x>")
}
