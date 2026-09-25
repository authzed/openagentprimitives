// pkg/platform/identityd/handlers_oauth.go — GET /link/oauth/<credname>, the
// cookie-gated entry to the OAuth dance, and its /oauth/callback/<credname>
// completion.
//
// Client resolution is DCR-only: identityd cannot prompt for a pre-registered
// client the way the TTY oauth_mcp flow does, so a provider with no
// registration_endpoint is a hard error rather than a fallback.
//
// Test seam: newOAuthHTTPClient is the package-level *http.Client factory.
// Production is safehttp.Client() (SSRF-guarded, refuses loopback); tests swap
// in httptest's client so discovery against 127.0.0.1 connects.
package identityd

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	mcpoauth "github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// newOAuthHTTPClient is the *http.Client factory for OAuth discovery + DCR,
// defaulting to the SSRF-guarded safehttp.Client(); tests override it via
// installOAuthHTTPClient. A factory rather than a stored client so each call
// gets a fresh one and test cases share no global state.
var newOAuthHTTPClient = safehttp.Client

// handleLinkOAuthGet handles GET /link/oauth/<credname>.
func (s *Server) handleLinkOAuthGet(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	// 1. Cookie gate. /link/oauth is NOT a portal-bootstrap path: it will not
	//    trade a signed link for a session, so the visitor must already hold
	//    an idd_session established via /my/accounts.
	subject, ok := s.checkOIDCCookie(r)
	if !ok {
		logger.Info("link/oauth: no cookie", "path", r.URL.Path)
		s.writeError(w, r, http.StatusUnauthorized, "Sign-in required",
			"Open /my/accounts first to sign in.")
		return
	}

	// 2. Extract credname. The route is registered with a trailing slash, so a
	//    multi-segment path matches too; reject any slash so a path-traversal
	//    segment cannot ride in through the credname.
	credName := strings.TrimPrefix(r.URL.Path, "/link/oauth/")
	if credName == "" || strings.Contains(credName, "/") {
		logger.Info("link/oauth: invalid credname", "path", r.URL.Path)
		s.writeError(w, r, http.StatusBadRequest, "Invalid path", "Credential name missing or malformed.")
		return
	}

	// 3. Locate the MCPServer that owns this credential.
	server, err := findMCPServerForCredential(r.Context(), s.deps.K8s, credName)
	if err != nil {
		logger.Info("link/oauth: MCPServer lookup failed", "cred", credName, "err", err.Error())
		s.writeError(w, r, http.StatusBadRequest, "No matching service",
			"No service is configured for this credential.")
		return
	}

	// 4. OAuth discovery against the MCP server's URL.
	hc := newOAuthHTTPClient()
	meta, err := discoverOAuthMetadata(r.Context(), hc, server.Spec.Server.URL)
	if err != nil {
		logger.Info("link/oauth: discovery failed",
			"cred", credName, "url", server.Spec.Server.URL, "err", err.Error())
		s.writeError(w, r, http.StatusBadGateway, "Service unreachable",
			"Couldn't discover the OAuth configuration of the upstream service.")
		return
	}

	// 5. Resolve the OAuth client (DCR-only; see the file header). credName is
	//    plumbed through so DCR registers the exact per-credential
	//    redirect_uri the authorize step sends — strict-match providers reject
	//    anything else. The resulting client is stashed in the state entry so
	//    the callback redeems the code as the SAME client.
	clientID, clientSecret, err := s.resolveOAuthClient(r.Context(), hc, server, meta, credName)
	if err != nil {
		logger.Info("link/oauth: client resolution failed",
			"cred", credName, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "OAuth client unavailable",
			"Couldn't get an OAuth client ID for this service. The operator may need to configure DCR or a pre-registered client.")
		return
	}

	// 6. PKCE.
	pkce, err := mcpoauth.NewPKCE()
	if err != nil {
		logger.Info("link/oauth: PKCE generation failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Internal error",
			"Couldn't prepare the OAuth flow.")
		return
	}

	// 7. Stash state. The provider echoes this token back to
	//    /oauth/callback/<credname>, which consumes it single-use.
	stateTok, err := s.oauthState.NewState(oauthStateEntry{
		CredentialName:     credName,
		Subject:            subject,
		PKCEVerifier:       pkce.Verifier,
		MCPServerNamespace: server.Namespace,
		MCPServerName:      server.Name,
		ClientID:           clientID,
		ClientSecret:       clientSecret,
	})
	if err != nil {
		logger.Info("link/oauth: state store NewState failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Internal error",
			"Couldn't track the OAuth flow.")
		return
	}

	// 8. Build authorize URL + 302.
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {oauthRedirectURI(s.deps.ExternalBaseURL(), credName)},
		"state":                 {stateTok},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {"S256"},
	}
	if scope := defaultScopeFromMetadata(meta); scope != "" {
		q.Set("scope", scope)
	}
	sep := "?"
	if strings.Contains(meta.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	dest := meta.AuthorizationEndpoint + sep + q.Encode()
	http.Redirect(w, r, dest, http.StatusFound)
}

// resolveOAuthClient resolves the OAuth client_id (+ optional client_secret) for
// an MCPServer + credential via RFC 7591 Dynamic Client Registration. No
// registration_endpoint is an error, not a fallback: identityd cannot prompt for
// a pre-registered client the way the TTY oauth_mcp flow can.
//
// credName is plumbed in so the registered redirect_uri byte-matches the
// authorize-time one — some providers strict-match and reject anything else.
//
// TODO: when MCPServer gains a pre-registered OAuth client field, check it first
// and fall through to DCR.
func (s *Server) resolveOAuthClient(ctx context.Context, hc *http.Client, srv *spiceboxv1alpha1.MCPServer, meta *mcpoauth.Metadata, credName string) (clientID, clientSecret string, err error) {
	if meta.RegistrationEndpoint == "" {
		return "", "", &mcpoauth.ErrNoDynamicRegistration{ServerURL: srv.Spec.Server.URL}
	}
	id, sec, err := mcpoauth.Register(ctx, hc, meta.RegistrationEndpoint,
		s.oauthRegistrationRedirects(credName))
	if err != nil {
		return "", "", err
	}
	return id, sec, nil
}

// oauthRegistrationRedirects returns the redirect_uri list to register at DCR
// time. RFC 7591 servers byte-match the authorize-time `redirect_uri` against
// this list, so it MUST contain exactly what oauthRedirectURI produces for the
// same credName — hence per-credential rather than one shared root.
//
// The trailing-slash variant is included so providers that canonicalize URIs by
// adding or stripping a slash still match.
func (s *Server) oauthRegistrationRedirects(credName string) []string {
	base := strings.TrimRight(s.deps.ExternalBaseURL(), "/")
	perCred := base + "/oauth/callback/" + credName
	return []string{perCred, perCred + "/"}
}

// defaultScopeFromMetadata joins ScopesSupported for the authorize endpoint.
// "" when the provider advertised none — omitting `scope` is RFC-compliant.
func defaultScopeFromMetadata(meta *mcpoauth.Metadata) string {
	if meta == nil || len(meta.ScopesSupported) == 0 {
		return ""
	}
	return strings.Join(meta.ScopesSupported, " ")
}

// handleOAuthCallbackGet handles GET /oauth/callback/<credname>: validates the
// state token, gates on the cookie subject, exchanges code + PKCE verifier for
// tokens, persists via useridentity.PutOAuthToken, then 302s to /my/accounts.
//
// Ordering rationale:
//
//  1. ?error= short-circuits — the provider already failed, so no state
//     consume, no cookie check, no exchange.
//  2. State Consume runs BEFORE the cookie check, so a stolen state token
//     cannot be replayed after a legitimate user abandons the flow.
//  3. The credential comes from the state entry, never the URL; the URL is the
//     provider's choice, the state is ours.
//  4. The cookie subject must equal the state entry's Subject — a valid signed
//     cookie alone is not enough.
//
// Every error path logs and renders a page via writeError; never a silent 4xx.
func (s *Server) handleOAuthCallbackGet(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	// 1. Extract credname; reject any slash so a path-traversal segment
	//    cannot ride in through it.
	credName := strings.TrimPrefix(r.URL.Path, "/oauth/callback/")
	if credName == "" || strings.Contains(credName, "/") {
		logger.Info("oauth callback: invalid credname", "path", r.URL.Path)
		s.writeError(w, r, http.StatusBadRequest, "Invalid path",
			"Credential name missing or malformed.")
		return
	}

	q := r.URL.Query()

	// 2. Provider-side error (consent declined, provider failure): never
	//    proceed past this point.
	if denial := q.Get("error"); denial != "" {
		logger.Info("oauth callback: provider returned error",
			"cred", credName, "error", denial, "desc", q.Get("error_description"))
		s.writeError(w, r, http.StatusBadRequest, "Sign-in cancelled",
			"You declined to authorize this service, or the provider rejected the request. You can try again from /my/accounts.")
		return
	}

	// 3. State token. Single-use; consumed even if subsequent checks fail
	//    so a stolen state token can't be replayed.
	stateTok := q.Get("state")
	if stateTok == "" {
		logger.Info("oauth callback: missing state param", "cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Invalid callback",
			"The callback is missing the state parameter.")
		return
	}
	entry, ok := s.oauthState.Consume(stateTok)
	if !ok {
		logger.Info("oauth callback: state missing/expired/used", "cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
			"This sign-in attempt is no longer valid. Try linking again from /my/accounts.")
		return
	}

	// 3b. Refuse an AGENT-flow state entry (AgentIdentityRef set). Both
	//     callbacks Consume from the SAME oauthStateStore, and the agent flow
	//     authorizes against an MCPServer a workshop builder authored — whose
	//     spec.server.URL, and so the provider the browser is redirected to,
	//     is untrusted input. If that flow's redirect_uri ever pointed back
	//     here (by bug, not by the agent flow's own dedicated redirect base),
	//     this branch is what stops the code from being redeemed into the
	//     STARTER'S OWN PERSONAL UserIdentity via PutOAuthToken below — which
	//     would file a shared bot token under one person's private account,
	//     bypass the operator relay entirely, and bypass the workshop write
	//     authority that is supposed to govern this credential. See
	//     handlers_agentoauth.go's package doc for the full rationale.
	if entry.AgentIdentityRef != "" {
		logger.Info("oauth callback: refusing an agent-flow state entry (has AgentIdentityRef) on the per-user callback",
			"cred", credName, "agentIdentityRef", entry.AgentIdentityRef)
		s.writeError(w, r, http.StatusBadRequest, "Invalid callback",
			"This sign-in was started as a different kind of connection. Try linking again from /my/accounts.")
		return
	}

	// 4. Credential binding: the flow's credential is the state entry's, never
	//    the callback URL's. The provider picks where it redirects, so a
	//    hostile one (merely linked by the user) could call back on
	//    /oauth/callback/<other credential> with this state and code —
	//    downstream would then file ITS token in the other credential's slot,
	//    replacing the user's real credential for an unrelated service.
	if entry.CredentialName == "" || entry.CredentialName != credName {
		logger.Info("oauth callback: credential name does not match the flow's state entry; refusing",
			"callback_cred", credName, "state_cred", entry.CredentialName, "subject", entry.Subject)
		s.writeError(w, r, http.StatusBadRequest, "Invalid callback",
			"This sign-in was started for a different credential. Try linking again from /my/accounts.")
		return
	}
	credName = entry.CredentialName

	// 5. Cookie gate: the cookie subject must equal the state entry's Subject.
	//    The state is already consumed even when this fails (replay defense).
	cookieSubject, cookieOK := s.checkOIDCCookie(r)
	if !cookieOK || cookieSubject != entry.Subject {
		logger.Info("oauth callback: cookie subject mismatch",
			"cred", credName, "cookie_ok", cookieOK,
			"cookie_subject", cookieSubject, "state_subject", entry.Subject)
		s.writeError(w, r, http.StatusForbidden, "Not your session",
			"You're signed in as a different user than this sign-in flow was started for.")
		return
	}

	// 6. Authorization code.
	code := q.Get("code")
	if code == "" {
		logger.Info("oauth callback: missing code param",
			"cred", credName, "subject", entry.Subject)
		s.writeError(w, r, http.StatusBadRequest, "Invalid callback",
			"The callback is missing the authorization code.")
		return
	}

	// 7. Re-load the MCPServer by the state entry's namespace/name — the
	//    trusted mapping; credname is only a UX handle from the URL.
	var srv spiceboxv1alpha1.MCPServer
	if err := s.deps.K8s.Get(r.Context(), client.ObjectKey{
		Namespace: entry.MCPServerNamespace,
		Name:      entry.MCPServerName,
	}, &srv); err != nil {
		logger.Info("oauth callback: MCPServer Get failed",
			"ns", entry.MCPServerNamespace, "name", entry.MCPServerName,
			"err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Service unreachable",
			"Couldn't load the upstream service configuration.")
		return
	}

	// 8. Re-discover: the token endpoint may have rotated, so discovery
	//    results are never cached across the authorize-redirect boundary.
	hc := newOAuthHTTPClient()
	meta, err := discoverOAuthMetadata(r.Context(), hc, srv.Spec.Server.URL)
	if err != nil {
		logger.Info("oauth callback: discovery failed",
			"cred", credName, "url", srv.Spec.Server.URL, "err", err.Error())
		s.writeError(w, r, http.StatusBadGateway, "Service unreachable",
			"Couldn't discover the OAuth configuration of the upstream service.")
		return
	}

	// 9. Reuse the client from the state entry. DCR mints a fresh client_id on
	//    every call, so re-registering here would mismatch the auth code's
	//    issuing client and the provider would reject the exchange.
	if entry.ClientID == "" {
		logger.Info("oauth callback: state entry missing ClientID — pre-state-bump entry, refusing",
			"cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
			"This sign-in flow was started under an older identityd build. Try linking again.")
		return
	}

	// 10. Exchange code + PKCE verifier for tokens.
	tok, err := mcpoauth.ExchangeCode(r.Context(), hc, meta.TokenEndpoint, mcpoauth.ExchangeCodeParams{
		Code:         code,
		RedirectURI:  oauthRedirectURI(s.deps.ExternalBaseURL(), credName),
		ClientID:     entry.ClientID,
		ClientSecret: entry.ClientSecret,
		CodeVerifier: entry.PKCEVerifier,
	})
	if err != nil {
		logger.Info("oauth callback: token exchange failed",
			"cred", credName, "subject", entry.Subject, "err", err.Error())
		s.writeError(w, r, http.StatusBadGateway, "Token exchange failed",
			"The upstream service rejected the token exchange. Try again from /my/accounts.")
		return
	}

	// 11. Persist. mcpoauth.Token.ExpiresIn is RFC 6749 seconds-from-now;
	//     PutOAuthTokenRequest.ExpiresAt is absolute Unix seconds. ExpiresIn
	//     == 0 stays 0, meaning "unknown / never expires" — PutOAuthToken then
	//     omits the expires_at key entirely.
	var expiresAt int64
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + int64(tok.ExpiresIn)
	}
	if err := useridentity.PutOAuthToken(r.Context(), s.deps.K8s, useridentity.PutOAuthTokenRequest{
		Subject:        entry.Subject,
		CredentialName: credName,
		AccessToken:    tok.AccessToken,
		RefreshToken:   tok.RefreshToken,
		ExpiresAt:      expiresAt,
		TokenType:      tok.TokenType,
		Scope:          tok.Scope,
		// A refresh_token is useless without the endpoint to redeem it at and
		// the client identity to redeem it as. Persisting the token without
		// these three stores a credential that can never be refreshed.
		TokenEndpoint: meta.TokenEndpoint,
		ClientID:      entry.ClientID,
		ClientSecret:  entry.ClientSecret,
	}); err != nil {
		logger.Info("oauth callback: PutOAuthToken failed",
			"cred", credName, "subject", entry.Subject, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Couldn't store credential",
			"The tokens were obtained but couldn't be saved. Try again from /my/accounts.")
		return
	}

	// 12. Redirect back with a ?linked= notice the portal surfaces as a banner.
	http.Redirect(w, r, "/my/accounts?linked="+url.QueryEscape(credName), http.StatusFound)
}
