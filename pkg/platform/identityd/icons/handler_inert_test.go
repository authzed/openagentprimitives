package icons

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// /icon/ writes UPSTREAM bytes with the UPSTREAM's own Content-Type, on the
// origin that holds idd_session. SVG was on the accepted-type list and
// formatTier ranked it BEST, so an attacker who controls a favicon — anyone who
// can create an MCPServer with a spec.siteURL they own, an ordinary tenant
// action — got a script executing same-origin with a signed-in admin's session
// as soon as that admin loaded the icon.
//
// This is not a disclosure question, which is what the handler's own comment
// reasoned about. It is whether the response is EXECUTABLE.
//
// Two independent changes, because either alone leaves the other half:
// SVG is no longer an accepted icon type at discovery, and the response is
// served with headers that make any byte sequence inert regardless of what a
// future type list admits.

type stubResolver struct{ e Entry }

func (s stubResolver) Lookup(context.Context, string) Entry { return s.e }

func serveIcon(t *testing.T, e Entry) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/icon/acme-creds", nil)
	rec := httptest.NewRecorder()
	NewHandler(stubResolver{e: e}).ServeHTTP(rec, req)
	return rec
}

func TestIconHandler_ResponseIsInertWhateverTheUpstreamSent(t *testing.T) {
	rec := serveIcon(t, Entry{
		ContentType: "image/svg+xml",
		Bytes:       []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
	})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"),
		"without nosniff a mislabelled body is sniffed back into something executable")
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "sandbox",
		"a sandboxed document has an opaque origin, so it cannot reach idd_session")
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "default-src 'none'",
		"and it loads nothing of its own")
	assert.Equal(t, "attachment", rec.Header().Get("Content-Disposition"),
		"navigating straight to the URL must not render it as a document")
}

// The type list is the other half. SVG is the only image format that is also a
// script host, and nothing in this product needs it: every other accepted type
// is raster.
func TestIsAllowedImageType_RefusesSVG(t *testing.T) {
	assert.False(t, isAllowedImageType("image/svg+xml"),
		"SVG is a script host, and an icon has no need to be one")

	for _, ok := range []string{"image/png", "image/x-icon", "image/vnd.microsoft.icon",
		"image/webp", "image/jpeg", "image/gif"} {
		assert.True(t, isAllowedImageType(ok), "%s is raster and stays accepted", ok)
	}
}

// formatTier ranked SVG FIRST, so an attacker offering both an SVG and a PNG
// won deterministically. With SVG refused upstream the ranking must not still
// prefer it, or a candidate that slips past the type check by another route
// (a `mask-icon` rel with no type) is chosen over an honest PNG.
func TestFormatTier_DoesNotPreferSVG(t *testing.T) {
	svg := iconCandidate{Type: "image/svg+xml"}
	png := iconCandidate{Type: "image/png"}

	assert.Greater(t, formatTier(svg), formatTier(png),
		"a PNG must outrank an SVG now that SVG is not served")
}
