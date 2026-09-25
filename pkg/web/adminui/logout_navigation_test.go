package adminui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// /admin/logout is a GET with a SIDE EFFECT, and markdown image syntax fires a
// same-origin GET. An image reference to it in any message a browser surface
// renders — a transcript, an approval card's excerpt, an agent's own reply —
// logs out every viewer who renders that message. Nothing has to be clicked.
//
// The framework's Origin pin is the wrong tool here: a top-level navigation
// sends no Origin header, so pinning on it would refuse the real Sign Out
// button. Sec-Fetch-Dest answers the question that actually distinguishes them
// — an <img> is "image", a fetch is "empty", and only a navigation is
// "document". The Sign Out button is window.location.assign, which is a
// navigation.
//
// Of the ten unauthenticated admin routes this is the only one with a side
// effect, so this is a one-route change rather than a class migration.

func doLogout(t *testing.T, dest string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/logout", nil)
	if dest != "" {
		req.Header.Set("Sec-Fetch-Dest", dest)
	}
	rec := httptest.NewRecorder()
	logoutHandler(rec, req)
	return rec
}

func clearsSessionCookie(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminSessionCookie && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

func TestLogout_ImageRequestDoesNotLogAnyoneOut(t *testing.T) {
	rec := doLogout(t, "image")

	assert.False(t, clearsSessionCookie(rec),
		"a rendered <img> must not be able to end a viewer's session")
	assert.NotEqual(t, http.StatusFound, rec.Code, "and it must not redirect either")
}

// A subresource fetch is the other shape a page can produce without a click.
func TestLogout_SubresourceFetchDoesNotLogAnyoneOut(t *testing.T) {
	for _, dest := range []string{"empty", "iframe", "script", "style", "font"} {
		t.Run(dest, func(t *testing.T) {
			assert.False(t, clearsSessionCookie(doLogout(t, dest)))
		})
	}
}

// The real Sign Out button: window.location.assign, a top-level navigation.
func TestLogout_NavigationStillSignsOut(t *testing.T) {
	rec := doLogout(t, "document")

	require.True(t, clearsSessionCookie(rec), "Sign Out must still work")
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/admin", rec.Header().Get("Location"))
}

// A browser that sends no Sec-Fetch-Dest at all predates the header (and the
// threat model). It fails CLOSED: the cost is that such a browser cannot sign
// out from the button, which is an inconvenience, where the other direction is
// the hole this closes.
func TestLogout_AbsentHeaderFailsClosed(t *testing.T) {
	assert.False(t, clearsSessionCookie(doLogout(t, "")),
		"no header is not proof of a navigation")
}
