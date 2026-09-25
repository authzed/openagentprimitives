package identityd

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// loginBindingCookie holds the per-flow nonce that ties an in-flight login
// state to the browser that STARTED it.
//
// Distinct from cookieName (the session cookie): this one is minted before any
// identity is known, proves nothing about who the visitor is, and is spent the
// moment the flow completes. It exists solely so a state token handed to a
// different browser is useless — see linkStateEntry.binding for the login-CSRF
// path that closes.
const loginBindingCookie = "idd_login_binding"

// loginBindingTTL matches the state store's own 10-minute window: a cookie
// outliving the state it binds protects nothing, and one expiring first turns a
// legitimate slow login into a refusal.
const loginBindingTTL = 10 * time.Minute

// beginLoginState mints a fresh browser binding, writes it to the response as
// loginBindingCookie, and returns a state token bound to BOTH the signed link
// and that binding.
//
// Every login entry point mints through here, and that is the point: the state
// store refuses an empty binding, so an entry that forgot the cookie cannot
// produce a usable state at all. The failure is at the first request rather
// than at a callback nobody tests.
func (s *Server) beginLoginState(w http.ResponseWriter, linkRaw, next, authenticator string) (string, error) {
	binding, err := newLoginBinding()
	if err != nil {
		return "", err
	}
	tok, err := s.stateStore.NewStateWithNext(linkRaw, next, binding, authenticator)
	if err != nil {
		return "", err
	}
	s.setLoginBindingCookie(w, binding)
	return tok, nil
}

// consumeLoginState resolves a callback's state token, requiring the binding
// cookie the begin handler set. A browser with no cookie, or the wrong one,
// gets stateWrongBrowser — and the state is spent either way (see Consume).
func (s *Server) consumeLoginState(r *http.Request, stateTok, authenticator string) (linkRaw, next string, refusal stateRefusal) {
	binding := ""
	if c, err := r.Cookie(loginBindingCookie); err == nil {
		binding = c.Value
	}
	return s.stateStore.Consume(stateTok, binding, authenticator)
}

// writeStateRefusal renders the user-facing page for a refused login state, and
// logs which of the two happened.
//
// The copy differs because the situations do. stateMissing is routine — a slow
// user, a back button, a re-clicked link — and "sign in again" is the whole
// answer. stateWrongBrowser means this browser did not start the login it is
// being asked to finish, which is what a login-CSRF lure looks like from the
// victim's side, so it says so rather than telling them to retry a thing they
// never started.
func (s *Server) writeStateRefusal(w http.ResponseWriter, r *http.Request, where string, refusal stateRefusal) {
	logger := log.FromContext(r.Context())
	if refusal == stateWrongAuthenticator {
		logger.Info(where+": login state was minted by a different sign-in path; refusing",
			"refusal", string(refusal))
		s.writeError(w, r, http.StatusForbidden, "This sign-in didn't start here",
			"This sign-in was started through a different sign-in method. Start it again from the beginning.")
		return
	}
	if refusal == stateWrongBrowser {
		logger.Info(where+": login state presented by a browser that did not start it; refusing",
			"refusal", string(refusal))
		s.clearLoginBindingCookie(w)
		s.writeError(w, r, http.StatusForbidden, "This sign-in didn't start here",
			"This sign-in was started in a different browser. Start it again from this browser, and if you did not start one, ignore the link you followed.")
		return
	}
	logger.Info(where+": state token missing/expired/used", "refusal", string(refusal))
	s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
		"Sign in again by clicking the link the agent gave you.")
}

// setLoginBindingCookie writes the binding for the life of one login flow.
//
// SameSite=Lax is REQUIRED rather than incidental: the callback arrives as a
// top-level GET navigation the IdP initiated, and Strict would withhold the
// cookie on exactly that request, refusing every legitimate login. Lax still
// withholds it from cross-site POSTs and subresource loads, which is the part
// that matters here.
func (s *Server) setLoginBindingCookie(w http.ResponseWriter, binding string) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginBindingCookie,
		Value:    binding,
		Path:     "/",
		MaxAge:   int(loginBindingTTL / time.Second),
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.deps.ExternalBaseURL(), "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearLoginBindingCookie expires the binding.
//
// Called on the wrong-browser refusal, and deliberately NOT on the success
// paths. A spent binding is inert — its state is gone, so it can never resolve
// again, and it expires on its own inside loginBindingTTL — whereas clearing it
// on success would have to interleave correctly with the password retry path,
// which consumes a state and immediately mints a fresh one on the same
// response. Two Set-Cookie headers for one name in one response is a browser
// tiebreak nobody should be relying on. The refusal path has no such conflict
// and is the one where a lingering nonce on a lured victim's browser is worth
// removing.
func (s *Server) clearLoginBindingCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginBindingCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.deps.ExternalBaseURL(), "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// newLoginBinding returns 32 random hex chars. crypto/rand only — a guessable
// binding is no binding, and the whole control rests on an attacker being
// unable to predict or write this value.
func newLoginBinding() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
