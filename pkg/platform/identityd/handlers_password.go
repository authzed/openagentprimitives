// pkg/platform/identityd/handlers_password.go — GET /password/login and POST
// /password/verify, the password-only local IdP's form and credential check.
// They are reachable only when the singleton ClusterIdentityProvider is
// kind=password; every other kind authenticates through the OIDC
// begin/callback pair.
//
// The password kind's Provider.Begin returns "/password/login?state=..." rather
// than a remote authorize URL, so beginIdPLogin's state-store + redirect
// plumbing drives this flow unchanged. Only the "exchange credential for
// Principal" step differs: VerifyPassword instead of an OIDC code exchange.
package identityd

import (
	"fmt"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"html"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// passwordWrongAttemptDelay is a fixed per-failure delay that blunts online
// brute-force guessing. Deliberately small — this guards one local admin account
// behind a desktop loopback/LAN listener, not a public login — but non-zero so a
// scripted guesser pays a fixed cost per attempt. A var only so tests can shrink
// it.
var passwordWrongAttemptDelay = 500 * time.Millisecond

// handlePasswordLogin handles GET /password/login?state=<token>: it validates
// beginIdPLogin's state token but deliberately does NOT consume it (see
// linkStateStore.Valid), then renders the form. /password/verify consumes it.
func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())
	state := r.URL.Query().Get("state")
	if state == "" {
		logger.Info("password login: missing state param")
		s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
			"Sign in again by clicking the link the agent gave you.")
		return
	}
	if !s.stateStore.Valid(state) {
		logger.Info("password login: state token missing/expired")
		s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
			"Sign in again by clicking the link the agent gave you.")
		return
	}
	writePasswordForm(w, http.StatusOK, state, "")
}

// handlePasswordVerify handles POST /password/verify: consumes the state token,
// requires the current ClusterIdentityProvider to be an idp.PasswordVerifier,
// and checks the password. Success mints the same idd_session cookie shape
// handleIdPCallback does and redirects to the state's `next` (default "/admin").
//
// A wrong password sets no cookie, pays passwordWrongAttemptDelay, and
// re-renders with a generic error under a FRESH state token — single-use covers
// each attempt, not merely each page load.
func (s *Server) handlePasswordVerify(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		logger.Info("password verify: ParseForm failed", "err", err.Error())
		s.writeError(w, r, http.StatusBadRequest, "Invalid form", "Could not read the form submission.")
		return
	}
	stateTok := r.FormValue("state")
	password := r.FormValue("password")
	if stateTok == "" {
		logger.Info("password verify: missing state form field")
		s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
			"Sign in again by clicking the link the agent gave you.")
		return
	}

	// Consumed regardless of whether the password matches. The retry path
	// re-mints a state bound to the same (linkRaw, next).
	linkRaw, next, refusal := s.consumeLoginState(r, stateTok, idpAuthenticator)
	if refusal != stateAccepted {
		s.writeStateRefusal(w, r, "password verify", refusal)
		return
	}
	if next == "" {
		next = "/admin"
	}

	resolved, err := s.idp.Current(ctx)
	if err != nil {
		logger.Info("password verify: cluster IdP unavailable", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"The cluster identity provider is not available. Ask your administrator to run `oap idp status`.")
		return
	}
	verifier, ok := resolved.provider.(idp.PasswordVerifier)
	if !ok {
		// The IdP was a password kind when Begin() minted this state, but it
		// could have been reconfigured since. Refuse rather than pretend some
		// other verification happened.
		logger.Info("password verify: current cluster IdP is not a password-kind provider")
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"This cluster's identity provider is not configured for password sign-in.")
		return
	}

	principal, verified := verifier.VerifyPassword(password)
	if !verified {
		time.Sleep(passwordWrongAttemptDelay)
		// A retry is a fresh flow, so it gets a fresh binding and a fresh cookie:
		// the state just consumed above spent the previous one.
		retryState, mintErr := s.beginLoginState(w, linkRaw, next, idpAuthenticator)
		if mintErr != nil {
			logger.Info("password verify: retry state mint failed", "err", mintErr.Error())
			s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Try again.")
			return
		}
		writePasswordForm(w, http.StatusUnauthorized, retryState, "Incorrect password.")
		return
	}

	// The password kind's principal always carries a proven email, so this
	// never hits the synthetic-subject guard — but fail closed on the
	// impossible error rather than proceed with an unresolved subject.
	pCanon, err := principal.Canonical()
	if err != nil {
		logger.Info("password verify: principal canonicalization failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Try again.")
		return
	}
	pSubject := pCanon.Subject()

	// Defense in depth: the password kind's wizard always sets
	// AllowAnyEmail=true, so this should never deny — but a misconfigured CR
	// must fail closed here, not only wherever else it might be noticed.
	if msg := enforceIdPPolicy(principal, resolved); msg != "" {
		logger.Info("password verify: policy denied", "canonical", pCanon)
		s.writeError(w, r, http.StatusForbidden, "Sign-in not permitted", msg)
		return
	}

	// Mint EXACTLY the cookie handleIdPCallback mints for every other IdP kind
	// — downstream gates cannot tell which path established it.
	cookieRaw, err := s.deps.LinkSigner.Mint(passthroughlink.Payload{
		Subject:   pSubject,
		ExpiresAt: time.Now().Add(resolved.sessionTTL).Unix(),
	})
	if err != nil {
		logger.Info("password verify: cookie mint failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in failed",
			"We couldn't issue a sign-in cookie. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    cookieRaw,
		Path:     "/",
		MaxAge:   int(resolved.sessionTTL / time.Second),
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.deps.ExternalBaseURL(), "https://"),
		SameSite: http.SameSiteLaxMode,
	})

	s.redirectNextOrLink(w, r, next, linkRaw)
}

// writePasswordForm renders the login form: a self-contained HTML page with no
// third-party assets, posting the password and state to /password/verify. A
// non-empty errMsg is shown as an inline banner for the wrong-password retry.
func writePasswordForm(w http.ResponseWriter, status int, state, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never cached: a per-attempt page carrying a single-use state token.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	var banner string
	if errMsg != "" {
		banner = `<p class="error">` + html.EscapeString(errMsg) + `</p>`
	}
	// The favicon is shared with every framework document (webui.FaviconHref)
	// so the first page a user ever sees carries the same mark as the rest.
	fmt.Fprintf(w, passwordFormHTML, html.EscapeString(webui.FaviconHref), banner, html.EscapeString(state))
}

const passwordFormHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in</title>
<link rel="icon" type="image/svg+xml" href="%s">
<style>
  :root { color-scheme: light dark; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; background: #f5f5f7; }
  .card { background: #fff; border-radius: 12px; padding: 2rem; box-shadow: 0 1px 4px rgba(0,0,0,0.12); width: 100%%; max-width: 320px; }
  h1 { font-size: 1.1rem; margin: 0 0 1rem; }
  input[type=password] { width: 100%%; box-sizing: border-box; padding: 0.6rem; font-size: 1rem; border: 1px solid #ccc; border-radius: 6px; margin-bottom: 1rem; }
  button { width: 100%%; padding: 0.6rem; font-size: 1rem; border: 0; border-radius: 6px; background: #0071e3; color: #fff; cursor: pointer; }
  button:hover { background: #0066cc; }
  .mark { display: block; width: 28px; height: 25px; margin: 0 0 0.75rem; fill: #1D1423; }
  .error { color: #d70015; font-size: 0.9rem; margin: 0 0 1rem; }
  @media (prefers-color-scheme: dark) {
    body { background: #1d1d1f; }
    .card { background: #2c2c2e; box-shadow: 0 1px 4px rgba(0,0,0,0.4); }
    .mark { fill: #F1F0F2; }
    h1 { color: #f5f5f7; }
    input[type=password] { background: #3a3a3c; border-color: #565658; color: #f5f5f7; }
    input[type=password]::placeholder { color: #98989d; }
    .error { color: #ff6961; }
  }
</style>
</head>
<body>
<div class="card">
  <svg class="mark" viewBox="0 0 345 305" role="img" aria-label="Open Agent Primitives"><path d="M159.186 7.59677C164.993 -2.53226 179.604 -2.53226 185.411 7.59677L342.572 281.702C348.349 291.779 341.075 304.335 329.459 304.335H15.138C3.52262 304.335 -3.75273 291.779 2.02468 281.702L159.186 7.59677ZM178.855 133.226C175.951 128.161 168.646 128.161 165.742 133.226L117.254 217.794C114.366 222.832 118.003 229.11 123.811 229.11H220.786C226.594 229.11 230.232 222.832 227.343 217.794L178.855 133.226Z"/></svg>
  <h1>Sign in</h1>
  %s
  <form method="POST" action="/password/verify">
    <input type="hidden" name="state" value="%s">
    <input type="password" name="password" placeholder="Password" autofocus required>
    <button type="submit">Sign in</button>
  </form>
</div>
</body>
</html>
`
