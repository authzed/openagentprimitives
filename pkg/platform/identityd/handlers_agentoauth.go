// pkg/platform/identityd/handlers_agentoauth.go — the AGENT-owned
// (workshop-credential) half of the OAuth dance: GET /link/agent-oauth/<cred>
// (authorize) and GET /oauth/agent-callback/<cred> (callback).
//
// This is a SIBLING to handlers_oauth.go's per-user /link/oauth/ +
// /oauth/callback/, not a variant of it, and the two must never share a
// redirect_uri or a state-store entry shape indistinguishably. Two
// requirements make that non-negotiable:
//
//   - The credential's owning MCPServer lives in a WORKSHOP namespace W a
//     builder authored — untrusted input the same way the builder's prompt
//     is. authorizeAgentOAuth resolves it via
//     passthroughcatalog.LookupMCPServerByCredentialInNamespace(W), never the
//     cluster-wide lookup the per-user flow uses: a credential name that
//     happens to also match a PRODUCTION MCPServer elsewhere must never be
//     selected, or the starter's browser is redirected at the production
//     provider and that provider's token lands in W.
//   - The MCPServer's spec.server.URL — and so the provider the browser is
//     sent to, and where IT redirects back — is therefore also
//     builder-influenced. If this flow reused the per-user redirect_uri
//     (/oauth/callback/<cred>), a hostile provider's redirect would land on
//     the PER-USER callback, which redeems the code via
//     useridentity.PutOAuthToken(Subject=starter) — filing the bot's token
//     under the clicking starter's own PERSONAL UserIdentity, bypassing the
//     operator relay and the workshop write authority entirely. So this flow
//     mints its OWN redirect base (agentOAuthRedirectURI) at DCR
//     registration, at authorize, and at the exchange, and BOTH callbacks
//     additionally discriminate their state entries by
//     oauthStateEntry.AgentIdentityRef — a second, independent line of
//     defense in case the URLs are ever reused by mistake.
//
// The identity of who may run this at all is unchanged from the rest of the
// workshop-credential surface: mayUpdateWorkshopCredential (the click-time UX
// gate) and the operator's own re-check on the write (pkg/web/admind) both
// assert "started this workshop" — never agentidentity#update_credential,
// which is platform-admin-only and the wrong authority for a builder.
package identityd

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	mcpoauth "github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
)

// handleLinkAgentOAuthGet handles GET /link/agent-oauth/<credname>?d=&sig=.
//
// It re-verifies the SAME signed payload the Connect-page card carried (never
// trusting the path alone), re-runs the click-time starter gate, resolves the
// MCPServer scoped to the target's OWN workshop namespace (requirement A),
// performs DCR against the agent flow's OWN redirect base (requirement B),
// and stashes a state entry carrying AgentIdentityRef + SessionRef before
// redirecting to the provider.
func (s *Server) handleLinkAgentOAuthGet(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	// 1. Extract credname; reject any slash so a path-traversal segment
	//    cannot ride in through it — same discipline as every other credname
	//    path segment in this package.
	credName := strings.TrimPrefix(r.URL.Path, "/link/agent-oauth/")
	if credName == "" || strings.Contains(credName, "/") {
		logger.Info("link/agent-oauth: invalid credname", "path", r.URL.Path)
		s.writeError(w, r, http.StatusBadRequest, "Invalid path", "Credential name missing or malformed.")
		return
	}

	// 2. Verify the signed link. Same payload the Connect page's d/sig came
	//    from — this handler does its OWN verification rather than trusting
	//    that the click came from a page identityd itself rendered.
	d := r.URL.Query().Get("d")
	sig := r.URL.Query().Get("sig")
	if d == "" || sig == "" {
		logger.Info("link/agent-oauth: missing d/sig query params", "cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Invalid link", "This link is missing required parameters.")
		return
	}
	raw := d + "." + sig
	payload, err := s.deps.LinkSigner.Verify(raw,
		passthroughlink.WithExpectedIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithExpectedAudience(passthroughlink.AudienceIdentityd))
	if err != nil {
		s.writeLinkVerifyError(w, r, err, logger)
		return
	}
	if payload.Purpose != passthroughlink.PurposeWorkshopCredential {
		logger.Info("link/agent-oauth: link is not a workshop-credential link", "purpose", payload.Purpose, "cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Invalid link",
			"This link does not cover a workshop bot credential.")
		return
	}

	// 3. Resolve the target AgentIdentity in W straight from the payload —
	//    same derivation the card-link GET used, so the object shown on the
	//    card and the object this authorize step acts on cannot drift apart.
	t, err := resolveWorkshopCredentialTarget(payload)
	if err != nil {
		s.failAgentOwnedResolve(w, r, payload.Purpose, err, logger)
		return
	}
	if t.Credential != credName {
		logger.Info("link/agent-oauth: URL credential does not match the signed link's own credential",
			"url_cred", credName, "link_cred", t.Credential)
		s.writeError(w, r, http.StatusBadRequest, "Invalid link",
			"This link is for a different credential.")
		return
	}

	// 4. Click-time gate: cookie proves a subject, mayUpdateWorkshopCredential
	//    (via gateAgentOwned → mayUpdateAgentCredential's Purpose dispatch)
	//    asks whether that subject started the workshop owning W. The SAME
	//    gate the Connect-page card-link GET already ran.
	subject, noCookie, why := s.gateAgentOwned(r, payload, t, logger)
	if noCookie {
		http.Redirect(w, r, "/oidc/login?d="+url.QueryEscape(d)+"&sig="+url.QueryEscape(sig), http.StatusFound)
		return
	}
	if why != "" {
		s.refuseAgentOwned(w, r, payload.Purpose, why, logger)
		return
	}

	// 5. (A) MCPServer lookup SCOPED to W — never the cluster-wide lookup the
	//    per-user flow uses. Zero matches in W fails closed rather than
	//    falling back to a same-named MCPServer elsewhere in the cluster.
	server, err := passthroughcatalog.LookupMCPServerByCredentialInNamespace(r.Context(), s.deps.K8s, t.Namespace, credName)
	if err != nil {
		logger.Info("link/agent-oauth: MCPServer lookup failed (namespace-scoped)",
			"cred", credName, "namespace", t.Namespace, "err", err.Error())
		s.writeError(w, r, http.StatusBadRequest, "No matching service",
			"No service is configured for this credential in this workshop.")
		return
	}

	// 6. OAuth discovery against the MCP server's URL.
	hc := newOAuthHTTPClient()
	meta, err := discoverOAuthMetadata(r.Context(), hc, server.Spec.Server.URL)
	if err != nil {
		logger.Info("link/agent-oauth: discovery failed",
			"cred", credName, "url", server.Spec.Server.URL, "err", err.Error())
		s.writeError(w, r, http.StatusBadGateway, "Service unreachable",
			"Couldn't discover the OAuth configuration of the upstream service.")
		return
	}

	// 7. Resolve the OAuth client via DCR, registering the AGENT flow's OWN
	//    redirect_uri (requirement B) — never oauthRegistrationRedirects,
	//    which registers the per-user one.
	clientID, clientSecret, err := s.resolveAgentOAuthClient(r.Context(), hc, server, meta, credName)
	if err != nil {
		logger.Info("link/agent-oauth: client resolution failed", "cred", credName, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "OAuth client unavailable",
			"Couldn't get an OAuth client ID for this service. The operator may need to configure DCR or a pre-registered client.")
		return
	}

	// 8. PKCE.
	pkce, err := mcpoauth.NewPKCE()
	if err != nil {
		logger.Info("link/agent-oauth: PKCE generation failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Internal error", "Couldn't prepare the OAuth flow.")
		return
	}

	// 9. Stash state — AgentIdentityRef non-empty marks this an AGENT-flow
	//    entry (the discriminant both callbacks fail closed on), SessionRef
	//    carries the builder session the callback re-checks the starter
	//    authority against.
	stateTok, err := s.oauthState.NewState(oauthStateEntry{
		CredentialName:     credName,
		Subject:            subject,
		PKCEVerifier:       pkce.Verifier,
		MCPServerNamespace: server.Namespace,
		MCPServerName:      server.Name,
		ClientID:           clientID,
		ClientSecret:       clientSecret,
		AgentIdentityRef:   t.Namespace + "/" + t.Name,
		SessionRef:         payload.SessionRef,
	})
	if err != nil {
		logger.Info("link/agent-oauth: state store NewState failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Internal error", "Couldn't track the OAuth flow.")
		return
	}

	// 10. Build authorize URL + 302, redirect_uri = the AGENT callback.
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {agentOAuthRedirectURI(s.deps.ExternalBaseURL(), credName)},
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

// resolveAgentOAuthClient mirrors resolveOAuthClient, but registers the AGENT
// flow's OWN redirect_uri list (agentOAuthRegistrationRedirects) rather than
// the per-user one — requirement B. No registration_endpoint is a hard error,
// same rationale as the per-user path: identityd cannot prompt for a
// pre-registered client.
func (s *Server) resolveAgentOAuthClient(ctx context.Context, hc *http.Client, srv *spiceboxv1alpha1.MCPServer, meta *mcpoauth.Metadata, credName string) (clientID, clientSecret string, err error) {
	if meta.RegistrationEndpoint == "" {
		return "", "", &mcpoauth.ErrNoDynamicRegistration{ServerURL: srv.Spec.Server.URL}
	}
	return mcpoauth.Register(ctx, hc, meta.RegistrationEndpoint, s.agentOAuthRegistrationRedirects(credName))
}

// agentOAuthRegistrationRedirects is oauthRegistrationRedirects' agent-flow
// counterpart: the redirect_uri list DCR registers, built from
// agentOAuthRedirectURI rather than oauthRedirectURI. Must stay in step with
// the authorize step's own redirect_uri and the exchange's RedirectURI —
// all three MUST name the agent callback, never the per-user one.
func (s *Server) agentOAuthRegistrationRedirects(credName string) []string {
	base := strings.TrimRight(s.deps.ExternalBaseURL(), "/")
	perCred := base + "/oauth/agent-callback/" + credName
	return []string{perCred, perCred + "/"}
}

// handleOAuthAgentCallbackGet handles GET /oauth/agent-callback/<credname>:
// validates the state token, refuses anything that isn't a genuine AGENT-flow
// entry (requirement B), re-runs the workshop-starter authority against the
// entry's OWN recorded SessionRef + Subject, exchanges the code, and relays
// the resulting bundle to the operator via AgentCredentialWriter.Replace.
// identityd itself never writes W's Secret, and no token value is ever
// logged.
//
// Ordering mirrors handleOAuthCallbackGet's own documented rationale:
// ?error= short-circuits before any state lookup; Consume runs before every
// other check (single-use, so a stolen state token cannot be replayed after a
// legitimate flow abandons); the AgentIdentityRef / credential-name checks
// come next (state is already consumed either way); only then the cookie +
// starter re-check; only then the network calls.
func (s *Server) handleOAuthAgentCallbackGet(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	// 1. Extract credname; reject any slash.
	credName := strings.TrimPrefix(r.URL.Path, "/oauth/agent-callback/")
	if credName == "" || strings.Contains(credName, "/") {
		logger.Info("oauth agent callback: invalid credname", "path", r.URL.Path)
		s.writeError(w, r, http.StatusBadRequest, "Invalid path", "Credential name missing or malformed.")
		return
	}

	q := r.URL.Query()

	// 2. Provider-side error: never proceed past this point.
	if denial := q.Get("error"); denial != "" {
		logger.Info("oauth agent callback: provider returned error",
			"cred", credName, "error", denial, "desc", q.Get("error_description"))
		s.writeError(w, r, http.StatusBadRequest, "Sign-in cancelled",
			"You declined to authorize this service, or the provider rejected the request. You can try connecting again.")
		return
	}

	// 3. State token. Single-use; consumed even if subsequent checks fail so a
	//    stolen state token can't be replayed.
	stateTok := q.Get("state")
	if stateTok == "" {
		logger.Info("oauth agent callback: missing state param", "cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Invalid callback",
			"The callback is missing the state parameter.")
		return
	}
	entry, ok := s.oauthState.Consume(stateTok)
	if !ok {
		logger.Info("oauth agent callback: state missing/expired/used", "cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
			"This sign-in attempt is no longer valid. Try connecting again.")
		return
	}

	// 4. (B) Refuse anything that isn't a genuine AGENT-flow entry. A
	//    per-user state has NO AgentIdentityRef — redeeming it here would
	//    relay a PERSON's own credential to the operator as though it were an
	//    agent-owned write.
	if entry.AgentIdentityRef == "" {
		logger.Info("oauth agent callback: state entry is not an agent-flow entry (no AgentIdentityRef); refusing",
			"cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Invalid callback",
			"This sign-in wasn't started as a bot-credential connection. Try connecting again from the workshop.")
		return
	}

	// 5. Credential binding: the flow's credential is the state entry's,
	//    never the callback URL's — same cross-service injection defense the
	//    per-user callback documents.
	if entry.CredentialName == "" || entry.CredentialName != credName {
		logger.Info("oauth agent callback: credential name does not match the flow's state entry; refusing",
			"callback_cred", credName, "state_cred", entry.CredentialName)
		s.writeError(w, r, http.StatusBadRequest, "Invalid callback",
			"This sign-in was started for a different credential. Try connecting again.")
		return
	}
	credName = entry.CredentialName

	// 6. Cookie gate: the cookie subject must equal the state entry's
	//    Subject. The state is already consumed even when this fails.
	cookieSubject, cookieOK := s.checkOIDCCookie(r)
	if !cookieOK || cookieSubject != entry.Subject {
		logger.Info("oauth agent callback: cookie subject mismatch",
			"cred", credName, "cookie_ok", cookieOK, "state_subject", entry.Subject)
		s.writeError(w, r, http.StatusForbidden, "Not your session",
			"You're signed in as a different user than this connection was started for.")
		return
	}

	// 7. Re-run the workshop-starter authority against the entry's OWN
	//    recorded SessionRef — the SAME authority function the authorize step
	//    and the card-link render already ran, re-checked here rather than
	//    trusted to still hold from authorize time. A Workshop deleted or
	//    re-provisioned (a new starter) between authorize and callback must
	//    refuse, not just a cookie/subject mismatch.
	if ok, why := s.mayUpdateWorkshopCredential(r.Context(), cookieSubject,
		passthroughlink.Payload{SessionRef: entry.SessionRef}, logger); !ok {
		s.refuseAgentOwned(w, r, passthroughlink.PurposeWorkshopCredential, why, logger)
		return
	}

	// 8. Authorization code.
	code := q.Get("code")
	if code == "" {
		logger.Info("oauth agent callback: missing code param", "cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Invalid callback",
			"The callback is missing the authorization code.")
		return
	}

	// 9. Re-load the MCPServer by the state entry's namespace/name — the
	//    trusted mapping; credname is only a UX handle from the URL.
	var srv spiceboxv1alpha1.MCPServer
	if err := s.deps.K8s.Get(r.Context(), client.ObjectKey{
		Namespace: entry.MCPServerNamespace,
		Name:      entry.MCPServerName,
	}, &srv); err != nil {
		logger.Info("oauth agent callback: MCPServer Get failed",
			"ns", entry.MCPServerNamespace, "name", entry.MCPServerName, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Service unreachable",
			"Couldn't load the upstream service configuration.")
		return
	}

	// 10. Re-discover: the token endpoint may have rotated.
	hc := newOAuthHTTPClient()
	meta, err := discoverOAuthMetadata(r.Context(), hc, srv.Spec.Server.URL)
	if err != nil {
		logger.Info("oauth agent callback: discovery failed",
			"cred", credName, "url", srv.Spec.Server.URL, "err", err.Error())
		s.writeError(w, r, http.StatusBadGateway, "Service unreachable",
			"Couldn't discover the OAuth configuration of the upstream service.")
		return
	}

	if entry.ClientID == "" {
		logger.Info("oauth agent callback: state entry missing ClientID; refusing", "cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
			"This sign-in flow is no longer valid. Try connecting again.")
		return
	}

	// 11. identityd must have somewhere to relay the result. Checked BEFORE
	//     the token exchange: no operator client wired is a deployment wiring
	//     fault, not something an exchanged token could ever fix.
	if s.deps.AgentCredentialWriter == nil {
		logger.Info("oauth agent callback: no operator client wired; refusing", "cred", credName)
		s.writeError(w, r, http.StatusInternalServerError, "This can't be saved here yet",
			agentOwnedResolveMessage(errNoCredentialWriter))
		return
	}

	// 12. Exchange code + PKCE verifier for tokens, against the AGENT flow's
	//     OWN redirect_uri (requirement B) — must byte-match what was
	//     registered at DCR and sent at authorize.
	tok, err := mcpoauth.ExchangeCode(r.Context(), hc, meta.TokenEndpoint, mcpoauth.ExchangeCodeParams{
		Code:         code,
		RedirectURI:  agentOAuthRedirectURI(s.deps.ExternalBaseURL(), credName),
		ClientID:     entry.ClientID,
		ClientSecret: entry.ClientSecret,
		CodeVerifier: entry.PKCEVerifier,
	})
	if err != nil {
		// Never log tok or any field derived from it — this branch runs before
		// tok exists, so there is nothing to accidentally include; token values
		// never appear in a log line anywhere in this handler.
		logger.Info("oauth agent callback: token exchange failed", "cred", credName, "err", err.Error())
		s.writeError(w, r, http.StatusBadGateway, "Token exchange failed",
			"The upstream service rejected the token exchange. Try connecting again.")
		return
	}

	var expiresAt int64
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + int64(tok.ExpiresIn)
	}

	agentNS, agentName, ok := splitSessionRef(entry.AgentIdentityRef)
	if !ok {
		// Unreachable in practice: this process is the only writer of
		// AgentIdentityRef, always as "<ns>/<name>" (step 9 of the authorize
		// handler). Fail closed rather than panic if that invariant is ever
		// broken by a future edit.
		logger.Info("oauth agent callback: state entry AgentIdentityRef is malformed", "ref", entry.AgentIdentityRef)
		s.writeError(w, r, http.StatusInternalServerError, "Something went wrong",
			"We couldn't determine which agent this credential belongs to. Nothing was saved.")
		return
	}

	// 13. Relay to the operator. identityd NEVER writes W's Secret itself —
	//     only the operator, which independently re-authorizes the write, may.
	_, err = s.deps.AgentCredentialWriter.Replace(r.Context(), cookieSubject, agentcred.Request{
		AgentIdentityRef: &spiceboxv1alpha1.NamespacedRef{Namespace: agentNS, Name: agentName},
		Credential:       credName,
		OAuth: &agentcred.OAuthBundle{
			AccessToken:   tok.AccessToken,
			RefreshToken:  tok.RefreshToken,
			TokenType:     tok.TokenType,
			Scope:         tok.Scope,
			TokenEndpoint: meta.TokenEndpoint,
			ClientID:      entry.ClientID,
			ClientSecret:  entry.ClientSecret,
			ExpiresAt:     expiresAt,
		},
	})
	if errors.Is(err, agentcred.ErrRefused) {
		// The operator's FullyConsistent, authoritative check said no even
		// though identityd's own click-time gate said yes — render the
		// permission page, not a value-specific error.
		s.refuseAgentOwned(w, r, passthroughlink.PurposeWorkshopCredential, "the operator refused: "+err.Error(), logger)
		return
	}
	if err != nil {
		logger.Info("oauth agent callback: credential relay failed",
			"identity", entry.AgentIdentityRef, "cred", credName, "err", err.Error())
		s.writeError(w, r, http.StatusConflict, "Couldn't connect the credential",
			agentOwnedResolveMessage(err))
		return
	}

	logger.Info("oauth agent callback: the operator connected a workshop AgentIdentity's OAuth credential",
		"identity", entry.AgentIdentityRef, "cred", credName, "subject", cookieSubject.String())

	// 14. Redirect back, mirroring the per-user callback's own notice shape.
	http.Redirect(w, r, "/my/accounts?linked="+url.QueryEscape(credName), http.StatusFound)
}
