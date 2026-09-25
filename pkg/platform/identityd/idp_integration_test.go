package identityd

// Integration test for the cluster-IdP login feature. These subtests wire
// identityd.NewServer onto a real httptest.Server and drive the full HTTP
// stack (cookie jar, manual redirect following) rather than calling handler
// methods directly.
//
// NOTE: fakekind's package-level seams (NextPrincipal, NextErr) are NOT
// parallel-safe. Do NOT add t.Parallel() to any subtest here.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	fakeidpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	// integrationEmail is the test user's email for integration tests.
	integrationEmail = "alice@example.com"
	// integrationSubject is the SpiceDB subject for alice@example.com.
	// base64url("alice@example.com") = "YWxpY2VAZXhhbXBsZS5jb20".
	integrationSubject = "user:YWxpY2VAZXhhbXBsZS5jb20"
	// integrationCanonical is the canonical (unprefixed) id.
	integrationCanonical = "YWxpY2VAZXhhbXBsZS5jb20"
)

// newIntegrationClient returns an http.Client that captures each redirect
// manually (does not auto-follow) and stores cookies via a jar. The caller
// must step through each redirect explicitly.
func newIntegrationClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err, "cookiejar.New must not fail")
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// newIntegrationServer builds a real httptest.Server backed by an identityd
// Server wired with the fake IdP kind (kind="fake"), a valid
// ClusterIdentityProvider CR + Secret, and no channel-kind authenticators.
// The returned *httptest.Server is closed via t.Cleanup.
func newIntegrationServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	scheme := idpLoaderScheme(t)
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaultCR("fake"), defaultSecret()).
		Build()

	// The signer carries iss=identityd + aud=identityd as defaults, matching
	// what internal/cmd/webd wires in production (see internal/cmd/webd/main.go WithIssuer /
	// WithAudience). This is required so the cookie minted by handleIdPCallback
	// (which sets no explicit Issuer/Audience in the Payload) carries the values
	// that checkOIDCCookie expects when verifying it.
	idSrv := NewServer(Deps{
		K8s: k8s,
		LinkSigner: passthroughlink.New(signerKey,
			passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
			passthroughlink.WithAudience(passthroughlink.AudienceIdentityd)),
		ExternalBaseURL: func() string { return "" }, // overridden below after httptest.Server starts
		Authenticators:  map[string]channelkinds.WebAuthenticator{},
	})

	// Construct the httptest.Server BEFORE wiring ExternalBaseURL — we need
	// the URL the test server listens on so the "Secure" cookie logic is
	// consistent. Override ExternalBaseURL via a local string so the closure
	// captures a pointer to a mutable string.
	ts := httptest.NewServer(idSrv.Handler())
	t.Cleanup(ts.Close)

	// Re-wire ExternalBaseURL to the real test-server URL so the idpLoader
	// builds the correct redirect_uri and the cookie Secure flag is consistent
	// with the scheme. (ts.URL is http://127.0.0.1:PORT.)
	baseURL := ts.URL
	idSrv.deps.ExternalBaseURL = func() string { return baseURL }
	// Also refresh the idpLoader's getter so it picks up the new base.
	idSrv.idp.externalURL = func() string { return baseURL }

	return ts, idSrv
}

// mintOIDCLoginLink mints a signed /oidc/login link (unverified, aud=identityd)
// and returns the d and sig query parameters already split.
func mintOIDCLoginLink(t *testing.T, sgn *passthroughlink.Signer) (d, sig string) {
	t.Helper()
	raw, err := sgn.Mint(passthroughlink.Payload{
		Issuer:     passthroughlink.IssuerChannelsd,
		Audience:   passthroughlink.AudienceIdentityd,
		SessionRef: "default/s1",
		Subject:    integrationSubject,
		ExpiresAt:  time.Now().Add(5 * time.Minute).Unix(),
	})
	require.NoError(t, err)
	d, sig, ok := splitSignedLink(raw)
	require.True(t, ok, "Mint must produce a splittable link")
	return d, sig
}

// buildLoginURL returns the full /oidc/login URL for ts with d, sig, and next.
func buildLoginURL(ts *httptest.Server, d, sig, next string) string {
	q := url.Values{}
	q.Set("d", d)
	q.Set("sig", sig)
	if next != "" {
		q.Set("next", next)
	}
	return ts.URL + "/oidc/login?" + q.Encode()
}

// TestIdPIntegration drives four end-to-end scenarios through a real
// httptest.Server. Each scenario is a subtest; they must NOT run in
// parallel because fakekind's package-level seams are shared state.
func TestIdPIntegration(t *testing.T) {
	ts, idSrv := newIntegrationServer(t)
	sgn := idSrv.deps.LinkSigner

	t.Run("browser login via IdP", func(t *testing.T) {
		fakeidpkind.NextPrincipal = identity.IdPUser(integrationEmail, true, "Alice")
		t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })

		client := newIntegrationClient(t)
		const nextPath = "/my/accounts"

		// Step 1: GET /oidc/login — server should 302 to the fake IdP authorize URL.
		d, sig := mintOIDCLoginLink(t, sgn)
		loginURL := buildLoginURL(ts, d, sig, nextPath)

		resp1, err := client.Get(loginURL)
		require.NoError(t, err)
		resp1.Body.Close()
		require.Equal(t, http.StatusFound, resp1.StatusCode)

		authorizeURL := resp1.Header.Get("Location")
		assert.Contains(t, authorizeURL, "authorize?state=",
			"must redirect to the fake IdP's authorize URL, got %q", authorizeURL)

		// Extract the state token from the authorize URL.
		parsedAuthorize, err := url.Parse(authorizeURL)
		require.NoError(t, err)
		stateTok := parsedAuthorize.Query().Get("state")
		require.NotEmpty(t, stateTok, "authorize URL must carry a state token")

		// Step 2: GET /oidc/callback/idp?code=fake-code&state=<tok>
		cbQ := url.Values{}
		cbQ.Set("code", "fake-code")
		cbQ.Set("state", stateTok)
		cbURL := ts.URL + "/oidc/callback/idp?" + cbQ.Encode()

		resp2, err := client.Get(cbURL)
		require.NoError(t, err)
		resp2.Body.Close()
		require.Equal(t, http.StatusFound, resp2.StatusCode)

		// Cookie assertions: idd_session must be set with MaxAge == 43200 (12h).
		var sessionCookie *http.Cookie
		for _, c := range resp2.Cookies() {
			if c.Name == cookieName {
				sessionCookie = c
				break
			}
		}
		require.NotNil(t, sessionCookie, "idd_session cookie must be set after IdP callback")
		assert.Equal(t, 43200, sessionCookie.MaxAge, "IdP cookie MaxAge must be 12h (43200s)")

		// Verify the cookie payload: should carry integrationSubject.
		payload, err := sgn.Verify(sessionCookie.Value)
		require.NoError(t, err, "idd_session cookie must verify with the server's signer")
		assert.Equal(t, identity.Subject(integrationSubject), payload.Subject,
			"cookie Subject must be the canonical user subject")

		// The 302 after callback must point to the next path.
		loc := resp2.Header.Get("Location")
		assert.Equal(t, nextPath, loc, "after IdP callback, must redirect to next=%q, got %q", nextPath, loc)
	})

	t.Run("session-bootstrap link skips the provider", func(t *testing.T) {
		// A portal link is the one shape identityd exchanges for a session
		// directly → it never calls Complete. If Complete is ever called,
		// NextErr causes a 500, which the assertion below catches.
		fakeidpkind.NextErr = errors.New("provider must not be called")
		t.Cleanup(func() { fakeidpkind.NextErr = nil })

		client := newIntegrationClient(t)
		const nextPath = "/artifacts/v/abc"

		// Mint a portal-purpose link, the shape internal/cmd/channelsd's portal minter
		// produces. SubjectVerified rides along exactly as it does in
		// production, and is deliberately not what unlocks this path.
		raw, err := sgn.Mint(passthroughlink.Payload{
			Issuer:          passthroughlink.IssuerChannelsd,
			Audience:        passthroughlink.AudienceIdentityd,
			Purpose:         purposePortal,
			Subject:         integrationSubject,
			SubjectVerified: true,
			ExpiresAt:       time.Now().Add(5 * time.Minute).Unix(),
		})
		require.NoError(t, err)
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		loginURL := buildLoginURL(ts, d, sig, nextPath)
		resp, err := client.Get(loginURL)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusFound, resp.StatusCode,
			"a portal link must set cookie + 302, not call the provider")

		// Cookie must be set with cookieTTL (600s), not sessionTTL.
		var sessionCookie *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == cookieName {
				sessionCookie = c
				break
			}
		}
		require.NotNil(t, sessionCookie, "idd_session cookie must be set by the portal-link bootstrap")
		assert.Equal(t, 600, sessionCookie.MaxAge, "link-bootstrapped cookie uses cookieTTL (600s)")

		// Must redirect directly to next, not to the provider.
		loc := resp.Header.Get("Location")
		assert.Equal(t, nextPath, loc, "bootstrap must redirect to next, got %q", loc)
	})

	t.Run("CLI login end-to-end", func(t *testing.T) {
		fakeidpkind.NextPrincipal = identity.IdPUser(integrationEmail, true, "Alice")
		t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })

		client := newIntegrationClient(t)
		const (
			cliState = "my-cli-state-integration"
			cliPort  = "4567"
		)

		// Step 1: GET /cli/login?state=<s>&port=<p>
		cliLoginQ := url.Values{}
		cliLoginQ.Set("state", cliState)
		cliLoginQ.Set("port", cliPort)
		resp1, err := client.Get(ts.URL + "/cli/login?" + cliLoginQ.Encode())
		require.NoError(t, err)
		resp1.Body.Close()
		require.Equal(t, http.StatusFound, resp1.StatusCode, "CLI login must redirect to IdP authorize URL")

		authorizeURL := resp1.Header.Get("Location")
		assert.Contains(t, authorizeURL, "authorize?state=",
			"CLI login must redirect to IdP, got %q", authorizeURL)

		// Extract the state token.
		parsedAuthorize, err := url.Parse(authorizeURL)
		require.NoError(t, err)
		stateTok := parsedAuthorize.Query().Get("state")
		require.NotEmpty(t, stateTok, "authorize URL must carry a state token")

		// Step 2: GET /oidc/callback/idp?code=fake-code&state=<tok>
		cbQ := url.Values{}
		cbQ.Set("code", "fake-code")
		cbQ.Set("state", stateTok)
		resp2, err := client.Get(ts.URL + "/oidc/callback/idp?" + cbQ.Encode())
		require.NoError(t, err)
		resp2.Body.Close()
		require.Equal(t, http.StatusFound, resp2.StatusCode, "IdP callback must redirect")

		// Must NOT set an idd_session cookie in the CLI flow.
		for _, c := range resp2.Cookies() {
			assert.NotEqual(t, cookieName, c.Name, "CLI flow must NOT set idd_session cookie")
		}

		// Must redirect to loopback with code + state.
		loopbackLoc := resp2.Header.Get("Location")
		assert.True(t, strings.HasPrefix(loopbackLoc, "http://127.0.0.1:"+cliPort+"/callback?"),
			"loopback redirect must point at CLI port %s, got %q", cliPort, loopbackLoc)

		parsedLoopback, err := url.Parse(loopbackLoc)
		require.NoError(t, err)
		code := parsedLoopback.Query().Get("code")
		gotState := parsedLoopback.Query().Get("state")
		require.NotEmpty(t, code, "loopback redirect must carry a one-time code")
		assert.Equal(t, cliState, gotState, "loopback redirect must carry the original CLI state")

		// Step 3: POST /cli/exchange
		exchangeBody, err := json.Marshal(map[string]string{"code": code, "state": cliState})
		require.NoError(t, err)
		resp3, err := client.Post(ts.URL+"/cli/exchange",
			"application/json", bytes.NewReader(exchangeBody))
		require.NoError(t, err)
		defer resp3.Body.Close()
		require.Equal(t, http.StatusOK, resp3.StatusCode, "CLI exchange must succeed")

		var exchangeResp cliExchangeResponse
		require.NoError(t, json.NewDecoder(resp3.Body).Decode(&exchangeResp))

		// Verify the assertion.
		assertionPayload, err := sgn.Verify(exchangeResp.Assertion,
			passthroughlink.WithExpectedAudience(passthroughlink.AudienceCLIIdentity))
		require.NoError(t, err, "CLI assertion must verify with WithExpectedAudience(cli-identity)")
		assert.True(t, assertionPayload.SubjectVerified, "CLI assertion must have SubjectVerified=true")
		assert.Equal(t, identity.Subject(integrationSubject), assertionPayload.Subject,
			"CLI assertion Subject must be the canonical user subject")

		// exp ≈ now + 12h; allow 60s drift.
		approxExp := time.Now().Add(12 * time.Hour)
		assert.InDelta(t, approxExp.Unix(), assertionPayload.ExpiresAt, 60,
			"CLI assertion exp must be ≈now+12h")

		// Response JSON fields.
		assert.Equal(t, identity.CanonicalFromTrusted(integrationCanonical, "test fixture"), exchangeResp.Subject,
			"exchange response Subject must be the unprefixed canonical")
		assert.Equal(t, integrationEmail, exchangeResp.Email)
	})

	t.Run("domain denial mints nothing", func(t *testing.T) {
		// bob@evil.example is not in the allowed domain list ("example.com").
		fakeidpkind.NextPrincipal = identity.IdPUser("bob@evil.example", true, "Bob")
		t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })

		client := newIntegrationClient(t)

		// Step 1: GET /oidc/login → should redirect to fake IdP authorize.
		d, sig := mintOIDCLoginLink(t, sgn)
		loginURL := buildLoginURL(ts, d, sig, "/my/accounts")

		resp1, err := client.Get(loginURL)
		require.NoError(t, err)
		resp1.Body.Close()
		require.Equal(t, http.StatusFound, resp1.StatusCode)

		authorizeURL := resp1.Header.Get("Location")
		parsedAuthorize, err := url.Parse(authorizeURL)
		require.NoError(t, err)
		stateTok := parsedAuthorize.Query().Get("state")
		require.NotEmpty(t, stateTok)

		// Step 2: GET /oidc/callback/idp — policy must deny bob@evil.example.
		cbQ := url.Values{}
		cbQ.Set("code", "fake-code")
		cbQ.Set("state", stateTok)
		resp2, err := client.Get(ts.URL + "/oidc/callback/idp?" + cbQ.Encode())
		require.NoError(t, err)
		resp2.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp2.StatusCode,
			"domain outside allowed list must be denied at callback")

		// No idd_session cookie must have been set.
		for _, c := range resp2.Cookies() {
			assert.NotEqual(t, cookieName, c.Name,
				"policy-denied login must NOT set idd_session cookie")
		}
	})
}

// TestLinkClusterIdPRoundTripReachesMenu asserts the end-to-end fix for the
// cookieless /link → cluster-IdP sign-in → credential menu flow:
//
//  1. A cookieless GET /link, with a Valid ClusterIdentityProvider configured,
//     must 302 to /oidc/login? (Task 1 fix — not to an inline OIDC ceremony).
//  2. Following the cluster-IdP sign-in (fake kind) for the SAME canonical
//     subject that started the session sets the idd_session cookie.
//  3. Returning to /link with that cookie reaches the credential menu
//     (HTTP 200, body mounts data-app="identity-link").
//
// NOTE: written against the already-fixed server (cookieless /link redirects to
// /oidc/login), so the pre-fix inline-OIDC behavior is not reproduced here.
func TestLinkClusterIdPRoundTripReachesMenu(t *testing.T) {
	// fakekind package-level seams are NOT parallel-safe; see file header.
	fakeidpkind.NextPrincipal = identity.IdPUser(integrationEmail, true, "Alice")
	t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })

	// Build the integration server (reuses the cluster-IdP fixture).
	ts, idSrv := newIntegrationServer(t)
	sgn := idSrv.deps.LinkSigner

	// Seed an AgentSession annotated with the link's canonical subject so
	// handleLinkGet's starter-recipient gate passes.
	sess := makeAgentSession("default", "link-rt", integrationSubject)
	require.NoError(t, idSrv.deps.K8s.Create(context.Background(), sess),
		"AgentSession must be created in the fake K8s client")

	// Mint a /link signed payload (iss=channelsd, aud=identityd).
	raw := mintLink(t, sgn, "default/link-rt", integrationSubject, []string{"github-pat"}, time.Time{})
	d, sig, ok := splitSignedLink(raw)
	require.True(t, ok, "mintLink must produce a splittable link")
	linkQ := url.Values{}
	linkQ.Set("d", d)
	linkQ.Set("sig", sig)
	linkURL := ts.URL + "/link?" + linkQ.Encode()

	client := newIntegrationClient(t)

	// Step 1: cookieless GET /link → 302 to /oidc/login?
	// (This is the behavior Task 1 introduced — previously it tried to run an
	// inline OIDC ceremony that had no cluster-IdP wiring.)
	resp1, err := client.Get(linkURL)
	require.NoError(t, err)
	resp1.Body.Close()
	require.Equal(t, http.StatusFound, resp1.StatusCode,
		"cookieless /link must redirect to /oidc/login")
	loc1 := resp1.Header.Get("Location")
	require.True(t, strings.HasPrefix(loc1, "/oidc/login?"),
		"Location must be /oidc/login?, got %q", loc1)

	// Step 2a: Follow /oidc/login → fake IdP authorize URL.
	resp2, err := client.Get(ts.URL + loc1)
	require.NoError(t, err)
	resp2.Body.Close()
	require.Equal(t, http.StatusFound, resp2.StatusCode,
		"/oidc/login must redirect to the fake IdP authorize URL")
	authorizeURL := resp2.Header.Get("Location")
	require.Contains(t, authorizeURL, "authorize?state=",
		"/oidc/login must redirect to fake IdP, got %q", authorizeURL)

	parsedAuth, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	stateTok := parsedAuth.Query().Get("state")
	require.NotEmpty(t, stateTok, "authorize URL must carry a state token")

	// Step 2b: Fake IdP callback → idd_session cookie set for integrationSubject.
	cbQ := url.Values{}
	cbQ.Set("code", "fake-code")
	cbQ.Set("state", stateTok)
	resp3, err := client.Get(ts.URL + "/oidc/callback/idp?" + cbQ.Encode())
	require.NoError(t, err)
	resp3.Body.Close()
	require.Equal(t, http.StatusFound, resp3.StatusCode,
		"IdP callback must set cookie + 302")

	var sessionCookie *http.Cookie
	for _, c := range resp3.Cookies() {
		if c.Name == cookieName {
			sessionCookie = c
			break
		}
	}
	require.NotNil(t, sessionCookie,
		"idd_session cookie must be set after IdP callback")

	// Step 3: GET /link WITH the idd_session cookie → credential menu.
	//
	// Route through the webui framework (buildWebHandler / serveWeb) so
	// renderApp has a renderer in the request context. The integration
	// httptest.Server calls idSrv.Handler() directly, bypassing the webui
	// framework renderer; using buildWebHandler here avoids that gap while
	// reusing the same K8s client and signer so the /link gates still pass.
	idpFx := linkFixture{
		srv:    idSrv,
		signer: sgn,
		c:      idSrv.deps.K8s,
	}
	idpFx.web = buildWebHandler(t, idpFx)

	menuReq := httptest.NewRequest(http.MethodGet, "/link?"+linkQ.Encode(), nil)
	menuReq.AddCookie(&http.Cookie{Name: cookieName, Value: sessionCookie.Value})
	menuRec := serveWeb(t, idpFx, menuReq)

	assert.Equal(t, http.StatusOK, menuRec.Code,
		"with valid idd_session cookie, /link must render the credential menu (HTTP 200)")
	assert.Contains(t, menuRec.Body.String(), `data-app="identity-link"`,
		"menu page must mount the identity-link React app")
}
