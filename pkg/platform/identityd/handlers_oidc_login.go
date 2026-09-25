// pkg/platform/identityd/handlers_oidc_login.go — GET /oidc/login, the generic
// "authenticate me, then go to next" entry. webd redirects a browser here when a
// GET hits an AuthLoginIfNecessary route with no session cookie.
//
// Unlike /link, this entry does NOT enforce link-Subject == OIDC subject: the
// signed link only resolves WHICH session, and therefore which channel kind's
// ceremony, to start. Whatever `next` points at is gated by downstream authz,
// not by a subject-equality check here.
package identityd

import (
	"errors"
	"net/http"
	"net/url"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// handleOIDCLogin handles GET /oidc/login?d=&sig=&next=. It verifies the signed
// link for integrity only (any iss/aud — the link is just a session/kind locator
// here), then walks a four-step precedence chain:
//
//  1. Session-bootstrap link — one linkMayMintSession clears. Skips OIDC and
//     sets the SHORT cookieTTL cookie; the long sessionTTL is reserved for
//     IdP-verified logins. Every other link shape falls through.
//  2. Cluster IdP — start the flow through a valid ClusterIdentityProvider. A
//     MISconfigured IdP fails closed (500); it never silently downgrades.
//  3. Channel-kind authenticator — the session kind's WebAuthenticator, used
//     only when no IdP is configured.
//  4. InsecureTrustLinks — dev-only opt-in. Without it, 403.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())
	q := r.URL.Query()
	d := q.Get("d")
	sig := q.Get("sig")
	if d == "" || sig == "" {
		logger.Info("oidc login: missing d/sig query params", "path", r.URL.Path)
		s.writeError(w, r, http.StatusForbidden, "Invalid link", "This link is invalid or has expired.")
		return
	}
	raw := d + "." + sig
	next := q.Get("next")

	// Integrity only, any iss/aud: the link locates the session and its channel
	// kind, it is not an authorization assertion. Bad or expired fails closed.
	payload, err := s.deps.LinkSigner.Verify(raw)
	if err != nil {
		logger.Info("oidc login: link verify failed", "err", err.Error())
		s.writeError(w, r, http.StatusForbidden, "Invalid link", "This link is invalid or has expired.")
		return
	}

	// Admin-UI login is session-less: there is no AgentSession to resolve a
	// channel kind from, so it dispatches straight to an authenticator.
	if payload.Purpose == passthroughlink.PurposeAdminLogin {
		s.handleAdminLogin(w, r, raw, next)
		return
	}

	// Step 1: session-bootstrap link. A link buys an idd_session ONLY when
	// linkMayMintSession clears it — the cookie is a general authentication
	// credential (see setTrustLinkCookie), so a narrower link must not buy one.
	ok, refusal := linkMayMintSession(payload)
	if !ok {
		// Purpose is checked first, so refusalPurpose is the routine case: an
		// artifact_view/session_view link correctly taking the long way round
		// to a real sign-in. V(1) keeps "why did this ask me to sign in?"
		// answerable without narrating every visit.
		//
		// Any other refusal means the link ALREADY had a bootstrap purpose and
		// still failed — a portal link channelsd did not issue, or one with no
		// subject. A visitor who then signs in leaves no other trace of it, so
		// it goes to INFO where an operator will see it.
		if refusal == refusalPurpose {
			logger.V(1).Info("oidc login: link is not a session-bootstrap link; routing to full sign-in",
				"purpose", payload.Purpose, "refusal", string(refusal))
		} else {
			logger.Info("oidc login: session-bootstrap link refused; routing to full sign-in",
				"purpose", payload.Purpose, "refusal", string(refusal),
				"issuer", payload.Issuer, "audience", payload.Audience)
		}
	}
	if ok {
		// Single-use, the same control /my/accounts applies to the same link:
		// aiming it at this entry instead must not be a way around it.
		first, cerr := s.consumedLinks.markConsumed(r.Context(), raw)
		if cerr != nil {
			// Fail closed, but say what actually happened: reporting a storage
			// outage as "already used" is a lie the user cannot act on.
			logger.Info("oidc login: could not verify single-use for session-bootstrap link; refusing",
				"subject", payload.Subject, "purpose", payload.Purpose, "err", cerr.Error())
			s.writeError(w, r, http.StatusServiceUnavailable, "Couldn't verify this link",
				"We couldn't check whether this link has already been used. Please try again in a moment.")
			return
		}
		if !first {
			logger.Info("oidc login: session-bootstrap link already consumed",
				"subject", payload.Subject, "purpose", payload.Purpose)
			s.writeError(w, r, http.StatusBadRequest, "Link already used",
				"This sign-in link has already been used. Ask your agent for a fresh one (e.g. 'manage my accounts').")
			return
		}
		s.setTrustLinkCookie(w, payload.Subject, logger)
		s.redirectNextOrLink(w, r, next, raw)
		return
	}

	// Step 2: cluster IdP. beginIdPLogin writes the response and returns true
	// when it handled the request; false ONLY when no IdP is configured.
	if s.beginIdPLogin(w, r, raw, next) {
		return
	}

	// Step 3: channel-kind authenticator, resolved from the session's kind.
	kindName, err := s.resolveKindForSessionRef(r, payload.SessionRef)
	if err != nil {
		logger.Info("oidc login: kind resolution failed", "ref", payload.SessionRef, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"Could not determine how to sign you in.")
		return
	}
	if kindName == "" {
		logger.Info("oidc login: kind resolution returned empty kind", "ref", payload.SessionRef)
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"Could not determine how to sign you in.")
		return
	}

	auth, ok := s.deps.Authenticators[kindName]
	if ok && auth != nil {
		stateTok, err := s.beginLoginState(w, raw, next, kindName)
		if err != nil {
			logger.Info("oidc login: could not start a browser-bound login state", "err", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Could not start sign-in.")
			return
		}
		redirect, err := auth.Begin(r.Context(), stateTok)
		if err != nil {
			logger.Info("oidc login: authenticator Begin failed", "kind", kindName, "err", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Could not start sign-in.")
			return
		}
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	}

	// Step 4: No authenticator found.
	if s.deps.InsecureTrustLinks {
		logger.Info("INSECURE: trust-link login used (--insecure-trust-links)", "subject", payload.Subject)
		s.setTrustLinkCookie(w, payload.Subject, logger)
		s.redirectNextOrLink(w, r, next, raw)
		return
	}
	logger.Info("oidc login: no cluster IdP and no authenticator for kind; returning 403",
		"kind", kindName, "ref", payload.SessionRef)
	s.writeError(w, r, http.StatusForbidden, "Sign-in unavailable",
		"This cluster has no browser sign-in configured. Ask your administrator to run `oap idp setup` (cluster IdP) or configure the channel's OAuth sign-in.")
}

// beginIdPLogin starts the cluster-IdP OIDC flow for a signed link + next. It
// backs both /oidc/login's IdP step and admin login, so "cluster IdP wins,
// misconfig fails closed, not-configured falls through" is identical for both.
//
// handled == true means a response is already written — a 302 to the IdP, or a
// 500 for a mint/Begin failure or a MISconfigured IdP — and the caller must
// return. handled == false means only ErrIdPNotConfigured, nothing written, and
// the caller should try its channel-kind path.
func (s *Server) beginIdPLogin(w http.ResponseWriter, r *http.Request, raw, next string) (handled bool) {
	logger := log.FromContext(r.Context())
	resolved, idpErr := s.idp.Current(r.Context())
	if idpErr == nil {
		// IdP configured + valid — start the OIDC flow.
		stateTok, err := s.beginLoginState(w, raw, next, idpAuthenticator)
		if err != nil {
			logger.Info("oidc login: could not start a browser-bound login state (idp path)", "err", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Could not start sign-in.")
			return true
		}
		redirect, err := resolved.provider.Begin(r.Context(), stateTok)
		if err != nil {
			logger.Info("oidc login: idp provider Begin failed", "err", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Could not start sign-in.")
			return true
		}
		http.Redirect(w, r, redirect, http.StatusFound)
		return true
	}
	if !errors.Is(idpErr, ErrIdPNotConfigured) {
		// Misconfiguration — fail closed, never silently downgrade.
		logger.Info("oidc login: cluster IdP misconfigured", "err", idpErr.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"The cluster identity provider is misconfigured. Ask your administrator to run `oap idp status`.")
		return true
	}
	// idpErr == ErrIdPNotConfigured → caller falls back to channel-kind.
	return false
}

// resolveKindForSessionRef loads the AgentSession named by sessionRef
// ("<ns>/<name>") and reads its channel kind through authKindForSession, so the
// ?auth=<kind> test override applies here too.
func (s *Server) resolveKindForSessionRef(r *http.Request, sessionRef string) (string, error) {
	ns, name, ok := splitSessionRef(sessionRef)
	if !ok {
		return "", errMalformedSessionRef
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := s.deps.K8s.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return "", err
	}
	return authKindForSession(r, &sess), nil
}

// safeNext reports whether next is a same-origin relative path, the only shape
// redirectNextOrLink honors. Absolute and protocol-relative ("//host") values
// are rejected: they would be open redirects off a freshly-authenticated tab.
func safeNext(next string) bool {
	if next == "" {
		return false
	}
	// Must start with "/" but NOT "//" (protocol-relative) or "/\" (backslash escape).
	if len(next) < 1 || next[0] != '/' {
		return false
	}
	if len(next) >= 2 && (next[1] == '/' || next[1] == '\\') {
		return false
	}
	// A non-empty Scheme or Host means an absolute URL masquerading as a path.
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return false
	}
	return true
}

// redirectNextOrLink sends the visitor to next when safeNext clears it, else
// rebuilds the /link?d=&sig= destination from raw. An unsafe next is DROPPED,
// not honored — falling back to /link is what keeps this from being an open
// redirect.
func (s *Server) redirectNextOrLink(w http.ResponseWriter, r *http.Request, next, raw string) {
	if next != "" {
		if !safeNext(next) {
			log.FromContext(r.Context()).V(1).Info("redirectNextOrLink: dropping invalid next (not a safe relative path)", "next", next)
			// Fall through to the /link reconstruction below.
		} else {
			http.Redirect(w, r, next, http.StatusFound)
			return
		}
	}
	d, sig, ok := splitSignedLink(raw)
	if !ok {
		// raw came off a Verify-validated link, so this is a logic bug, not
		// user error. Fail closed with a readable page.
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"An internal error occurred. Try the link again.")
		return
	}
	dest := "/link?d=" + url.QueryEscape(d) + "&sig=" + url.QueryEscape(sig)
	http.Redirect(w, r, dest, http.StatusFound)
}

// errMalformedSessionRef is returned by resolveKindForSessionRef when the
// link's SessionRef isn't a well-formed "<ns>/<name>".
var errMalformedSessionRef = errors.New("identityd: malformed SessionRef")
