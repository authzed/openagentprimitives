// pkg/platform/identityd/handlers_oauthas_authorize.go — GET /oauth/authorize
// and POST /oauth/consent, the browser-facing half of identityd's OWN OAuth
// AUTHORIZATION-SERVER role (see handlers_oauthas.go's package doc). Task 8
// adds the /oauth/token exchange that redeems the code these two mint.
//
// The security-load-bearing property both handlers share: redirect_uri is
// validated ONCE, against the client's DCR-registered set, before anything is
// ever redirected to it. Every validation failure after that point renders an
// HTML error page in place — never a redirect — because an unvalidated
// redirect_uri is attacker-controlled (the query string of a GET a user was
// sent to click). Once validated, redirect_uri is carried only through the
// pendingAuthEntry the server itself minted, never re-read from client input.
package identityd

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// ConsentDeps is the OPTIONAL half of identityd's deps answering the one
// question the authorization-server's consent screen needs: which agent
// classes may this subject grant a tool access to? Separate from WebDeps
// because webd derives it from SpiceDB-backed lookups (LookupStartableClasses
// + the classes behind LookupInteractableSessions, unioned and deduped) that
// are unavailable without SpiceDB wired. A deps value not implementing this
// leaves Deps.Consent nil, and server.go's routes() does not register ANY of
// the authorization-server surface (metadata, register, authorize, consent) —
// the feature is OFF without SpiceDB, not degraded, since none of it can
// complete without a consent-class lookup.
type ConsentDeps interface {
	ConsentClasses(ctx context.Context, owner identity.CanonicalUserID) ([]ConsentClass, error)
}

// ConsentClass is one agent class the subject may grant an OAuth client
// access to, shown as a scope checkbox on the consent screen.
type ConsentClass struct {
	// ID is the "ns/name" AgentClass reference — what actually lands in an
	// authCodeEntry.ScopeClasses value, never a free-text label.
	ID string
	// DisplayName is the human-readable label shown next to the checkbox.
	DisplayName string
}

// handleOAuthAuthorize handles GET /oauth/authorize: validates the request
// per RFC 6749 §4.1.1 + PKCE (RFC 7636, S256 only), then renders the consent
// screen. See the package doc above for the redirect_uri ordering invariant.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())
	q := r.URL.Query()

	if q.Get("response_type") != "code" {
		s.writeError(w, r, http.StatusBadRequest, "Unsupported response_type",
			"Only response_type=code is supported.")
		return
	}

	clientID := q.Get("client_id")
	reg, err := s.verifyClientID(clientID)
	if err != nil {
		logger.Info("oauth authorize: invalid client_id", "err", err.Error())
		s.writeError(w, r, http.StatusBadRequest, "Unknown client",
			"This tool is not registered with this server.")
		return
	}

	redirectURI := q.Get("redirect_uri")
	if !slices.Contains(reg.RedirectURIs, redirectURI) {
		logger.Info("oauth authorize: redirect_uri not registered", "client", reg.Name)
		s.writeError(w, r, http.StatusBadRequest, "Invalid redirect_uri",
			"This redirect address was not registered for this tool.")
		return
	}

	// From here on redirectURI is a value the client registered via DCR, not
	// unvalidated client input — but every failure below STILL renders an
	// error page rather than redirecting, because none of these remaining
	// checks is specified to carry an error back through redirect_uri before
	// PKCE itself is confirmed sound.
	challenge := q.Get("code_challenge")
	if challenge == "" {
		s.writeError(w, r, http.StatusBadRequest, "Missing code_challenge",
			"This tool did not send a PKCE code_challenge.")
		return
	}
	if q.Get("code_challenge_method") != "S256" {
		s.writeError(w, r, http.StatusBadRequest, "Unsupported code_challenge_method",
			"Only the S256 PKCE method is supported.")
		return
	}

	if s.deps.Consent == nil {
		// routes() only mounts this handler when Consent is wired; reachable
		// only if something calls it directly outside that gate.
		logger.Info("oauth authorize: no consent deps wired")
		s.writeError(w, r, http.StatusInternalServerError, "Not available",
			"This server is not configured to grant tool access.")
		return
	}

	subj := webui.SubjectFromContext(r.Context())
	canonical, err := identity.Subject(subj).CanonicalUserID()
	if err != nil {
		logger.Info("oauth authorize: non-user subject", "err", err.Error())
		s.writeError(w, r, http.StatusForbidden, "Not supported",
			"Only a signed-in person may authorize a tool.")
		return
	}

	classes, err := s.deps.Consent.ConsentClasses(r.Context(), canonical)
	if err != nil {
		logger.Info("oauth authorize: ConsentClasses failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Could not load your agents",
			"Try again shortly.")
		return
	}

	pendingID, err := s.pendingAuth.NewPending(pendingAuthEntry{
		ClientID:    clientID,
		ClientName:  reg.Name,
		RedirectURI: redirectURI,
		Challenge:   challenge,
		State:       q.Get("state"),
		Subject:     canonical,
	})
	if err != nil {
		logger.Info("oauth authorize: mint pending failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Could not start authorization",
			"Try again.")
		return
	}

	writeConsentForm(w, reg.Name, identity.DecodeForDisplay(canonical.String()), pendingID, classes)
}

// handleOAuthConsent handles POST /oauth/consent — the consent screen's
// Approve/Deny submit. redirect_uri, client_id, and the PKCE challenge all
// came from the pendingAuthEntry minted at /oauth/authorize, never from this
// POST body: the form carries only `pending`, `role`, `scope`/`scope_all`,
// and the `deny` marker, so there is nothing here for a forged submission to
// override except the grant's role and scope.
func (s *Server) handleOAuthConsent(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	// 1. CSRF origin pin — this POST creates a durable grant, so it must come
	// from the consent page itself, not a cross-site form.
	if !webui.TrustedOriginMatch(r, s.deps.ExternalBaseURL()) {
		logger.Info("oauth consent: Origin mismatch", "origin", r.Header.Get("Origin"))
		s.writeError(w, r, http.StatusForbidden, "Request blocked",
			"This request did not come from a trusted page.")
		return
	}

	if err := r.ParseForm(); err != nil {
		logger.Info("oauth consent: ParseForm failed", "err", err.Error())
		s.writeError(w, r, http.StatusBadRequest, "Invalid form", "Could not read the form submission.")
		return
	}

	// 2. Consume the pending entry — single-use, so a replayed submit (or a
	// second tab) cannot mint a second code off the same authorize visit.
	pending, ok := s.pendingAuth.Consume(r.FormValue("pending"))
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "Authorization expired",
			"Start over from the tool you were connecting.")
		return
	}

	// 3. The posting cookie subject MUST equal the pending entry's subject —
	// never trust the form alone for who is granting this.
	subj := webui.SubjectFromContext(r.Context())
	canonical, err := identity.Subject(subj).CanonicalUserID()
	if err != nil || canonical != pending.Subject {
		logger.Info("oauth consent: subject mismatch")
		s.writeError(w, r, http.StatusForbidden, "Not allowed",
			"This authorization does not belong to your sign-in session.")
		return
	}

	redirectURL, err := url.Parse(pending.RedirectURI)
	if err != nil {
		// pending.RedirectURI was exact-matched against the client's registered
		// set at /oauth/authorize; this should be unreachable.
		logger.Info("oauth consent: pending redirect_uri unparseable", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	// Deny never mints a code — it is handled before any role/scope
	// validation, and returns straight to the client with access_denied.
	if r.FormValue("deny") != "" {
		redirectWithParams(w, r, redirectURL, map[string]string{
			"error": "access_denied",
			"state": pending.State,
		})
		return
	}

	role := r.FormValue("role")
	switch role {
	case accesstoken.RoleRead, accesstoken.RoleInteract, accesstoken.RoleFull:
	default:
		s.writeError(w, r, http.StatusBadRequest, "Invalid access level", "Choose a valid access level.")
		return
	}

	if s.deps.Consent == nil {
		logger.Info("oauth consent: no consent deps wired")
		s.writeError(w, r, http.StatusInternalServerError, "Not available",
			"This server is not configured to grant tool access.")
		return
	}

	// 4. Scope is validated against a RE-FETCHED ConsentClasses list, not the
	// set rendered on the form minutes earlier and not any free-text value a
	// forged submission could supply — what the subject can grant may have
	// changed since the GET, and the class IDs themselves must be real.
	classes, err := s.deps.Consent.ConsentClasses(r.Context(), canonical)
	if err != nil {
		logger.Info("oauth consent: ConsentClasses failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Could not verify access", "Try again shortly.")
		return
	}
	allowed := make(map[string]bool, len(classes))
	for _, c := range classes {
		allowed[c.ID] = true
	}

	unfiltered := r.FormValue("scope_all") == "1"
	var scopeClasses []string
	if !unfiltered {
		for _, v := range r.Form["scope"] {
			if !allowed[v] {
				logger.Info("oauth consent: scope not in re-fetched ConsentClasses", "scope", v)
				s.writeError(w, r, http.StatusBadRequest, "Invalid scope",
					"One of the selected items is not available to you.")
				return
			}
			scopeClasses = append(scopeClasses, v)
		}
	}

	code, err := s.authCodes.NewCode(authCodeEntry{
		ClientID:     pending.ClientID,
		RedirectURI:  pending.RedirectURI,
		Challenge:    pending.Challenge,
		Subject:      canonical,
		Role:         role,
		ScopeClasses: scopeClasses,
		Unfiltered:   unfiltered,
	})
	if err != nil {
		logger.Info("oauth consent: mint code failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Could not finish authorization", "Try again.")
		return
	}

	redirectWithParams(w, r, redirectURL, map[string]string{
		"code":  code,
		"state": pending.State,
	})
}

// redirectWithParams 302s to base with params merged into its query string,
// skipping any empty value (state is often absent) — shared by the
// deny/approve tails of handleOAuthConsent so neither path hand-rolls query
// construction differently from the other.
func redirectWithParams(w http.ResponseWriter, r *http.Request, base *url.URL, params map[string]string) {
	q := base.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	base.RawQuery = q.Encode()
	http.Redirect(w, r, base.String(), http.StatusFound)
}

// writeConsentForm renders the consent screen: a self-contained HTML page
// (no third-party assets, no JS) with html.EscapeString on every
// interpolation — clientName and ownerDisplay are both user-influenced
// (DCR client_name, and whatever string decoded the subject's canonical),
// and classes come from SpiceDB-backed lookups this handler does not control.
func writeConsentForm(w http.ResponseWriter, clientName, ownerDisplay, pendingID string, classes []ConsentClass) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never cached: a per-visit page carrying a single-use pending id.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	var rows strings.Builder
	for _, c := range classes {
		fmt.Fprintf(&rows, consentScopeRowHTML, html.EscapeString(c.ID), html.EscapeString(c.DisplayName))
	}

	fmt.Fprintf(w, consentFormHTML,
		html.EscapeString(webui.FaviconHref),
		html.EscapeString(clientName),
		html.EscapeString(ownerDisplay),
		html.EscapeString(pendingID),
		rows.String(),
	)
}

const consentScopeRowHTML = `      <label><input type="checkbox" name="scope" value="%s"> %s</label>
`

const consentFormHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize access</title>
<link rel="icon" type="image/svg+xml" href="%s">
<style>
  :root { color-scheme: light dark; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; background: #f5f5f7; }
  .card { background: #fff; border-radius: 12px; padding: 2rem; box-shadow: 0 1px 4px rgba(0,0,0,0.12); width: 100%%; max-width: 420px; }
  h1 { font-size: 1.1rem; margin: 0 0 0.25rem; }
  .owner { color: #6e6e73; font-size: 0.85rem; margin: 0 0 1rem; }
  fieldset { border: 1px solid #d2d2d7; border-radius: 8px; margin: 0 0 1rem; padding: 0.75rem; }
  legend { font-size: 0.85rem; color: #6e6e73; padding: 0 0.3rem; }
  label { display: block; font-size: 0.95rem; margin: 0.4rem 0; }
  .desc { display: block; color: #6e6e73; font-size: 0.8rem; margin-left: 1.4rem; }
  .actions { display: flex; gap: 0.6rem; }
  button { flex: 1; padding: 0.6rem; font-size: 1rem; border: 0; border-radius: 6px; cursor: pointer; }
  button[name=approve] { background: #0071e3; color: #fff; }
  button[name=deny] { background: #e8e8ed; color: #1d1d1f; }
  @media (prefers-color-scheme: dark) {
    body { background: #1d1d1f; }
    .card { background: #2c2c2e; box-shadow: 0 1px 4px rgba(0,0,0,0.4); }
    h1 { color: #f5f5f7; }
    .owner, .desc, legend { color: #98989d; }
    fieldset { border-color: #48484a; }
    label { color: #f5f5f7; }
    button[name=deny] { background: #3a3a3c; color: #f5f5f7; }
  }
</style>
</head>
<body>
<div class="card">
  <h1>%s wants to access your account</h1>
  <p class="owner">Signed in as %s</p>
  <form method="POST" action="/oauth/consent">
    <input type="hidden" name="pending" value="%s">
    <fieldset>
      <legend>Access level</legend>
      <label><input type="radio" name="role" value="read" checked> Read<span class="desc">View sessions and their history.</span></label>
      <label><input type="radio" name="role" value="interact"> Interact<span class="desc">Send messages and respond to approvals.</span></label>
      <label><input type="radio" name="role" value="full"> Full<span class="desc">Everything you can do, including starting new sessions.</span></label>
    </fieldset>
    <fieldset>
      <legend>Which agents?</legend>
%s      <label><input type="checkbox" name="scope_all" value="1"> Everything I can access</label>
    </fieldset>
    <div class="actions">
      <button type="submit" name="deny" value="1">Deny</button>
      <button type="submit" name="approve" value="1">Approve</button>
    </div>
  </form>
</div>
</body>
</html>
`
