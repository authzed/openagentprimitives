package settingsui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
)

// sessionCookieName is the cookie that authenticates every request after the
// one-shot launch-token exchange (see handleAuth). Its value is the SAME secret
// as the launch token — there is only one secret — but the two differ in how
// they travel: the launch token appears exactly once, in the GET /auth query
// string, and from then on the secret rides only in this cookie (which
// requireAuth reads). So the secret never again appears in a URL, and therefore
// never lands in browser history, a proxy access log, or a Referer header.
const sessionCookieName = "oap_settings_session"

// newToken mints a 256-bit random token, hex-encoded, used both as the
// one-shot launch credential (see Server.URL) and, once exchanged, as the
// session cookie value.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("settingsui: mint token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// validToken reports whether candidate matches s.token, compared in
// constant time so a timing side-channel can't be used to guess the token
// byte-by-byte.
func (s *Server) validToken(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.token)) == 1
}

// handleAuth is the launch-token exchange: GET /auth?token=<hex>. A valid
// token sets the session cookie and redirects to "/"; an invalid or missing
// one 401s without touching the response's cookie jar. This is the only
// route reachable without the session cookie already set (see Start's mux
// wiring) — everything else requires the cookie requireAuth checks.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	candidate := r.URL.Query().Get("token")
	if candidate == "" || !s.validToken(candidate) {
		http.Error(w, "settings: invalid token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// requireAuth gates next behind a valid session cookie (see
// sessionCookieName), comparing it in constant time. Every route the server
// registers except GET /auth is wrapped in this — see Start.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie(sessionCookieName)
		if err != nil || !s.validToken(ck.Value) {
			http.Error(w, "settings: authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
