package identityd

import (
	"net/http"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// adminLoginKind picks the authenticator for a session-less admin login:
// explicit ?kind= override, then "slack" when registered, then first
// alphabetically — deterministic in every case. A chooser page for the
// multi-authenticator case is a follow-up; until then the ?kind= override
// is what keeps every registered kind reachable.
func adminLoginKind(auths map[string]channelkinds.WebAuthenticator, override string) (string, bool) {
	if override != "" {
		_, ok := auths[override]
		return override, ok
	}
	if len(auths) == 0 {
		return "", false
	}
	if _, ok := auths["slack"]; ok {
		return "slack", true
	}
	names := make([]string, 0, len(auths))
	for n := range auths {
		names = append(names, n)
	}
	sort.Strings(names)
	return names[0], true
}

// handleAdminLogin starts OIDC for an admin_login link. There is deliberately NO
// trust-link fallback here: an admin link carries no Subject, so "no
// authenticator" must be a hard error — minting an empty-subject cookie would be
// an authz bypass.
//
// Precedence: the cluster IdP is the canonical browser identity and wins
// whenever it is configured. ?kind= selects among the CHANNEL authenticators
// and is consulted only after that — it is the multi-auth escape hatch for a
// deployment with no cluster IdP, not a way past one.
//
// It used to skip a configured IdP entirely, and that was a policy bypass, not
// a preference. enforceIdPPolicy is the only enforcement of the cluster's
// declared emailVerified and allowedEmailDomains rules, it runs on the IdP
// callback, and the channel-kind callback (handlers_oidc.go) never calls it —
// so `?kind=slack` on an admin-login link took a real, registered authenticator
// and minted a full idd_session with the cluster's login policy unconsulted.
// The signed d/sig pair needed to reach it is handed out by the 302 that
// GET /admin returns to any cookie-less browser.
//
// Distinct from the deleted ?auth= override, which fails closed: this one
// authenticates for real and simply ignores the policy.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request, raw, next string) {
	logger := log.FromContext(r.Context())
	override := r.URL.Query().Get("kind")

	// beginIdPLogin returns false only when no IdP is configured. Consulted
	// FIRST and unconditionally: a configured cluster IdP is the deployment's
	// declared answer to "who may sign in", and no query parameter overrides it.
	if s.beginIdPLogin(w, r, raw, next) {
		if override != "" {
			logger.Info("admin login: ignoring ?kind= — a cluster IdP is configured and its policy governs admin sign-in",
				"override", override)
		}
		return
	}

	kind, ok := adminLoginKind(s.deps.Authenticators, override)
	if !ok {
		logger.Info("admin login: no usable authenticator",
			"override", r.URL.Query().Get("kind"), "registered", len(s.deps.Authenticators))
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"No sign-in provider is configured for admin login.")
		return
	}
	auth := s.deps.Authenticators[kind]
	// The state is bound to THIS authenticator: the callback route takes its
	// kind from the URL, so an unbound state minted here is redeemable at any
	// other registered callback.
	stateTok, err := s.beginLoginState(w, raw, next, kind)
	if err != nil {
		logger.Info("admin login: could not start a browser-bound login state", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Could not start sign-in.")
		return
	}
	redirect, err := auth.Begin(r.Context(), stateTok)
	if err != nil {
		logger.Info("admin login: authenticator Begin failed", "kind", kind, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Could not start sign-in.")
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}
