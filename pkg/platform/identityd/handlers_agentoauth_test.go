// pkg/platform/identityd/handlers_agentoauth_test.go — coverage for the
// agent-owned (workshop-credential) OAuth flow: GET /link/agent-oauth/<cred>
// (authorize) and GET /oauth/agent-callback/<cred> (callback), plus the two
// cross-flow security properties shared with the per-user flow in
// handlers_oauth_test.go:
//
//   - (A) the authorize step's MCPServer lookup is scoped to the workshop
//     namespace W — a same-named MCPServer elsewhere in the cluster must
//     never be chosen.
//   - (B) the agent flow's state entries and the per-user flow's state
//     entries must never be redeemable through the OTHER flow's callback.
//
// Reuses newWorkshopCredFixture + the wcred* fixtures from
// credupdate_agentowned_test.go, and startOAuthFakeProvider /
// installOAuthHTTPClient / seedOAuthState from handlers_oauth_test.go — all
// in this same package.
package identityd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
)

// === GET /link/agent-oauth/<cred> (authorize) ==============================

func TestHandleLinkAgentOAuthGet_HappyPath(t *testing.T) {
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred(wcredWorkshopNS, "weather-mcp", wcredCred, provider.srv.URL)
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity(), mcpSrv)

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	d, sig, splitOK := splitRaw(raw)
	require.True(t, splitOK)
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/agent-oauth/"+wcredCred+"?d="+d+"&sig="+sig, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code, "the workshop starter's authorize must 302; body=%s", rec.Body.String())
	loc := rec.Header().Get("Location")
	require.NotEmpty(t, loc)

	u, err := url.Parse(loc)
	require.NoError(t, err)
	assert.Equal(t, provider.srv.URL, u.Scheme+"://"+u.Host, "authorize URL host matches the provider")
	assert.Equal(t, "/authorize", u.Path)

	q := u.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "fake-client-123", q.Get("client_id"), "client_id from DCR")
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.NotEmpty(t, q.Get("code_challenge"))
	stateTok := q.Get("state")
	require.NotEmpty(t, stateTok)

	// (B) the redirect_uri MUST be the AGENT callback, never the per-user one.
	wantRedirect := "https://identityd.example.org/oauth/agent-callback/" + wcredCred
	notWantRedirect := "https://identityd.example.org/oauth/callback/" + wcredCred
	assert.Equal(t, wantRedirect, q.Get("redirect_uri"))
	assert.NotEqual(t, notWantRedirect, q.Get("redirect_uri"),
		"redirect_uri must never be the per-user /oauth/callback/ path")

	entry, ok := fx.srv.oauthState.Consume(stateTok)
	require.True(t, ok, "state token resolves to a stored entry")
	assert.Equal(t, wcredCred, entry.CredentialName)
	assert.Equal(t, wcredStarter, entry.Subject)
	assert.NotEmpty(t, entry.PKCEVerifier)
	assert.Equal(t, wcredWorkshopNS, entry.MCPServerNamespace)
	assert.Equal(t, "weather-mcp", entry.MCPServerName)
	assert.Equal(t, wcredWorkshopNS+"/"+wcredAgentID, entry.AgentIdentityRef,
		"the state entry must carry AgentIdentityRef — this is the AGENT-flow discriminant")
	assert.Equal(t, wcredBuilderNS+"/"+wcredBuilderSess, entry.SessionRef,
		"the state entry must carry the builder SessionRef for the callback's starter re-check")
}

func TestHandleLinkAgentOAuthGet_NonStarterRefused(t *testing.T) {
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred(wcredWorkshopNS, "weather-mcp", wcredCred, provider.srv.URL)
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity(), mcpSrv)

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	d, sig, splitOK := splitRaw(raw)
	require.True(t, splitOK)
	cookie := mintCookie(t, fx.signer, wcredBystander, time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/agent-oauth/"+wcredCred+"?d="+d+"&sig="+sig, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "a non-starter holder must be refused")
	assert.Zero(t, provider.registrationHits, "a refused visitor must never trigger DCR")
}

func TestHandleLinkAgentOAuthGet_NoCookieRedirectsToLogin(t *testing.T) {
	mcpSrv := mcpServerWithCred(wcredWorkshopNS, "weather-mcp", wcredCred, "https://example.com/mcp")
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity(), mcpSrv)

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	d, sig, splitOK := splitRaw(raw)
	require.True(t, splitOK)

	req := httptest.NewRequest(http.MethodGet, "/link/agent-oauth/"+wcredCred+"?d="+d+"&sig="+sig, nil)
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code, "no cookie must redirect into sign-in, not 500/401")
	assert.Contains(t, rec.Header().Get("Location"), "/oidc/login?d=")
}

// --- (A) MCPServer lookup is scoped to the workshop namespace W ---

func TestHandleLinkAgentOAuthGet_MCPServerScope_OtherNamespaceNotChosen(t *testing.T) {
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	// The MCPServer with the matching credential name lives OUTSIDE the
	// workshop namespace — e.g. a real production MCPServer. It must never be
	// chosen by the workshop-scoped authorize step.
	prodSrv := mcpServerWithCred("prod-ns", "weather-mcp", wcredCred, provider.srv.URL)
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity(), prodSrv)

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	d, sig, splitOK := splitRaw(raw)
	require.True(t, splitOK)
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/agent-oauth/"+wcredCred+"?d="+d+"&sig="+sig, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"an MCPServer outside the workshop namespace must never be selected; body=%s", rec.Body.String())
	assert.Zero(t, provider.registrationHits,
		"no DCR — and so no redirect to the PRODUCTION provider — when no in-namespace MCPServer resolves")
}

func TestHandleLinkAgentOAuthGet_MCPServerScope_NoneInWorkshopFailsClosed(t *testing.T) {
	// No MCPServer at all, anywhere.
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity())

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	d, sig, splitOK := splitRaw(raw)
	require.True(t, splitOK)
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/agent-oauth/"+wcredCred+"?d="+d+"&sig="+sig, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "zero MCPServers in W must fail closed")
}

func TestHandleLinkAgentOAuthGet_MalformedOrWrongPurposeRefused(t *testing.T) {
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())
	mcpSrv := mcpServerWithCred(wcredWorkshopNS, "weather-mcp", wcredCred, provider.srv.URL)

	cases := []struct {
		name   string
		mutate func(p passthroughlink.Payload) passthroughlink.Payload
	}{
		{
			name: "wrong purpose (not workshop_credential)",
			mutate: func(p passthroughlink.Payload) passthroughlink.Payload {
				p.Purpose = passthroughlink.PurposeArtifactView
				return p
			},
		},
		{
			name: "expired link",
			mutate: func(p passthroughlink.Payload) passthroughlink.Payload {
				p.ExpiresAt = time.Now().Add(-1 * time.Minute).Unix()
				return p
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity(), mcpSrv)
			cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

			p := passthroughlink.Payload{
				Issuer:              passthroughlink.IssuerChannelsd,
				Audience:            passthroughlink.AudienceIdentityd,
				SessionRef:          wcredBuilderNS + "/" + wcredBuilderSess,
				AgentIdentityRef:    wcredWorkshopNS + "/" + wcredAgentID,
				RequiredCredentials: []string{wcredCred},
				Purpose:             passthroughlink.PurposeWorkshopCredential,
				ExpiresAt:           time.Now().Add(5 * time.Minute).Unix(),
			}
			p = tc.mutate(p)
			raw, err := fx.signer.Mint(p)
			require.NoError(t, err)
			d, sig, splitOK := splitRaw(raw)
			require.True(t, splitOK)

			req := httptest.NewRequest(http.MethodGet, "/link/agent-oauth/"+wcredCred+"?d="+d+"&sig="+sig, nil)
			req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
			rec := httptest.NewRecorder()
			fx.srv.Handler().ServeHTTP(rec, req)

			assert.NotEqual(t, http.StatusFound, rec.Code, "none of these may reach a provider redirect")
			assert.Zero(t, provider.registrationHits)
		})
	}
}

// === GET /oauth/agent-callback/<cred> (callback) ============================

func TestHandleOAuthAgentCallbackGet_HappyPath(t *testing.T) {
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred(wcredWorkshopNS, "weather-mcp", wcredCred, provider.srv.URL)
	writer := &fakeCredentialWriter{}
	fx := newWorkshopCredFixture(t, wcredStarterBare, writer, makeWorkshopOAuthAgentIdentity(), mcpSrv)
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     wcredCred,
		Subject:            wcredStarter,
		PKCEVerifier:       "fake-pkce-verifier",
		MCPServerNamespace: wcredWorkshopNS,
		MCPServerName:      "weather-mcp",
		ClientID:           "test-client-id",
		AgentIdentityRef:   wcredWorkshopNS + "/" + wcredAgentID,
		SessionRef:         wcredBuilderNS + "/" + wcredBuilderSess,
	})

	q := url.Values{"code": {"fake-auth-code"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/"+wcredCred+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code, "happy path → 302; body=%s", rec.Body.String())
	assert.Equal(t, 1, provider.tokenHits, "token endpoint called once")

	require.Equal(t, 1, writer.calls, "the credential must be relayed via AgentCredentialWriter.Replace")
	assert.Equal(t, wcredStarter, writer.subject, "the asserted subject is the starter")
	require.NotNil(t, writer.req.AgentIdentityRef, "the request must target the AgentIdentity directly")
	assert.Equal(t, wcredWorkshopNS, writer.req.AgentIdentityRef.Namespace)
	assert.Equal(t, wcredAgentID, writer.req.AgentIdentityRef.Name)
	assert.Equal(t, wcredCred, writer.req.Credential)
	assert.Empty(t, writer.req.Token, "OAuth arm must not also set Token")
	assert.Empty(t, writer.req.SessionRef.Namespace, "must target via AgentIdentityRef, never SessionRef")

	require.NotNil(t, writer.req.OAuth, "the OAuth bundle must be set")
	assert.Equal(t, "fake-access-tok", writer.req.OAuth.AccessToken)
	assert.Equal(t, "fake-refresh-tok", writer.req.OAuth.RefreshToken)
	assert.Equal(t, "Bearer", writer.req.OAuth.TokenType)
	assert.Equal(t, "read write", writer.req.OAuth.Scope)
	assert.NotZero(t, writer.req.OAuth.ExpiresAt, "expires_at computed from expires_in")
	assert.Equal(t, provider.srv.URL+"/token", writer.req.OAuth.TokenEndpoint)
	assert.Equal(t, "test-client-id", writer.req.OAuth.ClientID)

	// identityd itself must NEVER write W's Secret directly — only the
	// operator (behind AgentCredentialWriter) may.
	assert.Equal(t, wcredStale, workshopSecretValue(t, fx),
		"identityd must not touch the workshop AgentIdentity's Secret directly")

	_, ok := fx.srv.oauthState.Consume(stateTok)
	assert.False(t, ok, "state token must be single-use")
}

func TestHandleOAuthAgentCallbackGet_RealOperatorWritesTheOAuthBundle(t *testing.T) {
	// The round-trip proof spanning this task and Task 2 (admind's OAuth
	// arm): a fake writer only proves identityd SENT the right shape; a real
	// operator over the same cluster proves the bundle is actually accepted
	// and lands on the workshop AgentIdentity's Secret.
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	scheme := newScheme(t)
	mcpSrv := mcpServerWithCred(wcredWorkshopNS, "weather-mcp", wcredCred, provider.srv.URL)
	objs := []clientpkg.Object{
		makeAgentSession(wcredBuilderNS, wcredBuilderSess, string(wcredStarter)),
		makeWorkshopNamespace(),
		makeWorkshop(wcredStarterBare),
		makeWorkshopOAuthAgentIdentity(),
		mcpSrv,
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	signer := passthroughlink.New(signerKey)

	const operatorToken = "test-admind-token"
	adm, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     c,
		Checker: &fakeAgentAuthz{}, // unused by the AgentIdentityRef branch; admind.New still requires non-nil
		Token:   operatorToken,
		Logger:  testr.New(t),
	})
	require.NoError(t, err, "build the operator-side admind handler")
	opSrv := httptest.NewServer(adm.Handler())
	t.Cleanup(opSrv.Close)
	writer := agentcred.New(opSrv.URL, operatorToken)

	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators: map[string]channelkinds.WebAuthenticator{
			"fake": fakeAuthenticator{externalBaseURL: "https://identityd.example.org"},
		},
		AgentCredentialWriter: writer,
	})
	fx := linkFixture{srv: srv, signer: signer, c: c}
	fx.web = buildWebHandler(t, fx)

	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})
	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     wcredCred,
		Subject:            wcredStarter,
		PKCEVerifier:       "fake-pkce-verifier",
		MCPServerNamespace: wcredWorkshopNS,
		MCPServerName:      "weather-mcp",
		ClientID:           "test-client-id",
		AgentIdentityRef:   wcredWorkshopNS + "/" + wcredAgentID,
		SessionRef:         wcredBuilderNS + "/" + wcredBuilderSess,
	})

	q := url.Values{"code": {"fake-auth-code"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/"+wcredCred+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code, "the real operator must accept the starter's connect; body=%s", rec.Body.String())

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		clientpkg.ObjectKey{Namespace: wcredWorkshopNS, Name: wcredSecret}, &sec))
	assert.Equal(t, "fake-access-tok", string(sec.Data["access_token"]))
	assert.Equal(t, "fake-refresh-tok", string(sec.Data["refresh_token"]))
}

func TestHandleOAuthAgentCallbackGet_ErrorParameter(t *testing.T) {
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity())
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/agent-callback/"+wcredCred+"?error=access_denied&error_description=user%20refused", nil)
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Sign-in cancelled")
}

func TestHandleOAuthAgentCallbackGet_MissingState(t *testing.T) {
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity())
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/"+wcredCred+"?code=foo", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Invalid callback")
}

func TestHandleOAuthAgentCallbackGet_StateAlreadyConsumedRefused(t *testing.T) {
	writer := &fakeCredentialWriter{}
	fx := newWorkshopCredFixture(t, wcredStarterBare, writer, makeWorkshopOAuthAgentIdentity())
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:   wcredCred,
		Subject:          wcredStarter,
		PKCEVerifier:     "v",
		ClientID:         "test-client-id",
		AgentIdentityRef: wcredWorkshopNS + "/" + wcredAgentID,
		SessionRef:       wcredBuilderNS + "/" + wcredBuilderSess,
	})
	_, ok := fx.srv.oauthState.Consume(stateTok)
	require.True(t, ok)

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/"+wcredCred+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "a replayed state must be refused")
	assert.Zero(t, writer.calls)
}

func TestHandleOAuthAgentCallbackGet_ExpiredStateRefused(t *testing.T) {
	writer := &fakeCredentialWriter{}
	fx := newWorkshopCredFixture(t, wcredStarterBare, writer, makeWorkshopOAuthAgentIdentity())
	fx.srv.oauthState.ttl = 0
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:   wcredCred,
		Subject:          wcredStarter,
		PKCEVerifier:     "v",
		ClientID:         "test-client-id",
		AgentIdentityRef: wcredWorkshopNS + "/" + wcredAgentID,
		SessionRef:       wcredBuilderNS + "/" + wcredBuilderSess,
	})

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/"+wcredCred+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "an expired state must be refused")
	assert.Zero(t, writer.calls)
}

func TestHandleOAuthAgentCallbackGet_CookieSubjectMismatchRefused(t *testing.T) {
	writer := &fakeCredentialWriter{}
	fx := newWorkshopCredFixture(t, wcredStarterBare, writer, makeWorkshopOAuthAgentIdentity())
	bystanderCookie := mintCookie(t, fx.signer, wcredBystander, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:   wcredCred,
		Subject:          wcredStarter, // the flow's own subject is the starter
		PKCEVerifier:     "v",
		ClientID:         "test-client-id",
		AgentIdentityRef: wcredWorkshopNS + "/" + wcredAgentID,
		SessionRef:       wcredBuilderNS + "/" + wcredBuilderSess,
	})

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/"+wcredCred+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: bystanderCookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "cookie subject != entry.Subject must refuse")
	assert.Zero(t, writer.calls, "no write for a refused callback")

	_, ok := fx.srv.oauthState.Consume(stateTok)
	assert.False(t, ok, "state must be consumed even on refusal (replay defense)")
}

func TestHandleOAuthAgentCallbackGet_StarterRevokedBetweenAuthorizeAndCallbackRefused(t *testing.T) {
	// entry.Subject == cookie subject (both the ORIGINAL starter), but the
	// Workshop's own starterCanonical has since changed underneath the
	// in-flight state (e.g. the workshop's provisioning re-ran). The
	// callback's OWN re-run of the starter gate against entry.SessionRef must
	// catch this — proving the re-check is not redundant with "cookie ==
	// entry.Subject" alone.
	writer := &fakeCredentialWriter{}
	fx := newWorkshopCredFixture(t, wcredStarterBare, writer, makeWorkshopOAuthAgentIdentity())
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:   wcredCred,
		Subject:          wcredStarter,
		PKCEVerifier:     "v",
		ClientID:         "test-client-id",
		AgentIdentityRef: wcredWorkshopNS + "/" + wcredAgentID,
		SessionRef:       wcredBuilderNS + "/" + wcredBuilderSess,
	})

	var ws spiceboxv1alpha1.Workshop
	require.NoError(t, fx.c.Get(context.Background(),
		clientpkg.ObjectKey{Namespace: wcredBuilderNS, Name: spiceboxv1alpha1.WorkshopName(wcredBuilderSess)}, &ws))
	ws.Spec.StarterCanonical = "someone-else-entirely"
	require.NoError(t, fx.c.Update(context.Background(), &ws))

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/"+wcredCred+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"the callback must re-run the starter gate against entry.SessionRef, not just trust cookie==entry.Subject")
	assert.Zero(t, writer.calls)
}

func TestHandleOAuthAgentCallbackGet_CredentialNameMismatchRejected(t *testing.T) {
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())
	writer := &fakeCredentialWriter{}
	mcpSrv := mcpServerWithCred(wcredWorkshopNS, "weather-mcp", wcredCred, provider.srv.URL)
	fx := newWorkshopCredFixture(t, wcredStarterBare, writer, makeWorkshopOAuthAgentIdentity(), mcpSrv)
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     wcredCred,
		Subject:            wcredStarter,
		PKCEVerifier:       "v",
		MCPServerNamespace: wcredWorkshopNS,
		MCPServerName:      "weather-mcp",
		ClientID:           "test-client-id",
		AgentIdentityRef:   wcredWorkshopNS + "/" + wcredAgentID,
		SessionRef:         wcredBuilderNS + "/" + wcredBuilderSess,
	})

	// The URL names a DIFFERENT credential than the flow's own state entry.
	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/some-other-credential?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Zero(t, writer.calls, "nothing may be written under a credential the flow was never started for")
}

func TestHandleOAuthAgentCallbackGet_NoWriterWiredFailsClosed(t *testing.T) {
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())
	mcpSrv := mcpServerWithCred(wcredWorkshopNS, "weather-mcp", wcredCred, provider.srv.URL)
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity(), mcpSrv) // nil writer
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     wcredCred,
		Subject:            wcredStarter,
		PKCEVerifier:       "v",
		MCPServerNamespace: wcredWorkshopNS,
		MCPServerName:      "weather-mcp",
		ClientID:           "test-client-id",
		AgentIdentityRef:   wcredWorkshopNS + "/" + wcredAgentID,
		SessionRef:         wcredBuilderNS + "/" + wcredBuilderSess,
	})

	q := url.Values{"code": {"fake-auth-code"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/"+wcredCred+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code, "no operator client wired must fail closed, not silently succeed")
}

// === (B) cross-flow: an entry from ONE flow must be refused by the OTHER's callback ===

func TestHandleOAuthCallbackGet_RefusesAgentFlowState(t *testing.T) {
	// The per-user callback must refuse a state entry carrying
	// AgentIdentityRef, even with an otherwise-valid cookie/code.
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())
	mcpSrv := mcpServerWithCred("default", "linear-mcp", "linear-oauth", provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	const canonical = identity.Subject("user:alice@example.com")
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     "linear-oauth",
		Subject:            canonical,
		PKCEVerifier:       "v",
		MCPServerNamespace: "default",
		MCPServerName:      "linear-mcp",
		ClientID:           "test-client-id",
		AgentIdentityRef:   wcredWorkshopNS + "/" + wcredAgentID, // marks this as an AGENT-flow entry
		SessionRef:         wcredBuilderNS + "/" + wcredBuilderSess,
	})

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/linear-oauth?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"the per-user callback must refuse an agent-flow state; body=%s", rec.Body.String())
	assert.Equal(t, 0, provider.tokenHits, "no token exchange for a refused cross-flow state")

	var ui spiceboxv1alpha1.UserIdentity
	err := fx.c.Get(context.Background(),
		clientpkg.ObjectKey{Name: useridentity.NameForSubject(canonical)}, &ui)
	assert.Error(t, err,
		"no personal UserIdentity may be written from an agent-flow state landing on the per-user callback — "+
			"this is the exact bot-token-into-a-personal-identity failure requirement (B) exists to prevent")
}

func TestHandleOAuthAgentCallbackGet_RefusesPerUserFlowState(t *testing.T) {
	// The agent callback must refuse a state entry with NO AgentIdentityRef
	// (a per-user-flow entry), even with an otherwise-valid starter cookie.
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())
	mcpSrv := mcpServerWithCred(wcredWorkshopNS, "weather-mcp", wcredCred, provider.srv.URL)
	writer := &fakeCredentialWriter{}
	fx := newWorkshopCredFixture(t, wcredStarterBare, writer, makeWorkshopOAuthAgentIdentity(), mcpSrv)
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     wcredCred,
		Subject:            wcredStarter,
		PKCEVerifier:       "v",
		MCPServerNamespace: wcredWorkshopNS,
		MCPServerName:      "weather-mcp",
		ClientID:           "test-client-id",
		// AgentIdentityRef intentionally left empty — a per-user-flow state.
	})

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/agent-callback/"+wcredCred+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"the agent callback must refuse a per-user-flow state; body=%s", rec.Body.String())
	assert.Zero(t, writer.calls, "no agent-credential write for a refused cross-flow state")
	assert.Equal(t, 0, provider.tokenHits)
}
