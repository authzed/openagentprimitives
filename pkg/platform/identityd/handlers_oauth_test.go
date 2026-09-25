// pkg/platform/identityd/handlers_oauth_test.go — coverage for GET /link/oauth/<credname>
// (Slice 3 ε5) and GET /oauth/callback/<credname> (Slice 3 ε6). Reuses
// linkFixture + cookie helpers from handlers_link_test.go; stubs the
// discovery + token-exchange HTTP path via httptest + the package-level
// test seam installOAuthHTTPClient.
package identityd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	mcpoauth "github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
)

// installOAuthHTTPClient swaps the package-level OAuth HTTP-client factory
// for one bound to the given httptest server's *http.Client. The default
// is safehttp.Client() which (correctly) refuses loopback — incompatible
// with httptest. Cleanup restores the default.
func installOAuthHTTPClient(t *testing.T, hc *http.Client) {
	t.Helper()
	prev := newOAuthHTTPClient
	newOAuthHTTPClient = func() *http.Client { return hc }
	t.Cleanup(func() { newOAuthHTTPClient = prev })
}

// oauthFakeProvider builds an httptest server that serves a minimal OAuth
// authorization-server. By default it exposes well-known discovery, DCR
// registration, and a /token endpoint that returns canned tokens. handler
// customizers can override individual paths.
type oauthFakeProvider struct {
	srv                *httptest.Server
	wellKnownHandler   http.HandlerFunc
	registrationHits   int
	registerHandler    http.HandlerFunc
	tokenHandler       http.HandlerFunc
	tokenHits          int
	registrationOpenAt string // empty = include registration_endpoint in metadata
	scopes             []string
}

// startOAuthFakeProvider builds and starts the fake provider. The MCPServer
// URL the test wires up must equal srv.URL so Discover lands on the
// well-known path served here.
func startOAuthFakeProvider(t *testing.T) *oauthFakeProvider {
	t.Helper()
	p := &oauthFakeProvider{
		scopes: []string{"read", "write"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		// Discover probes the MCP root URL first. A 200 (or 404) without a
		// 401+WWW-Authenticate causes Discover to fall through to the
		// /.well-known fallback on the same base — which is what we serve.
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, _ *http.Request) {
		// Not actually visited in unit tests — the handler under test
		// returns the authorize URL in a Location header; the browser
		// would visit. Present for completeness.
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		if p.wellKnownHandler != nil {
			p.wellKnownHandler(w, r)
			return
		}
		meta := map[string]any{
			"authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint":         p.srv.URL + "/token",
			"scopes_supported":       p.scopes,
		}
		if p.registrationOpenAt == "" {
			meta["registration_endpoint"] = p.srv.URL + "/register"
		} else if p.registrationOpenAt != "absent" {
			meta["registration_endpoint"] = p.registrationOpenAt
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		p.registrationHits++
		if p.registerHandler != nil {
			p.registerHandler(w, r)
			return
		}
		// Default DCR success: return a deterministic client_id.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"fake-client-123"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		p.tokenHits++
		if p.tokenHandler != nil {
			p.tokenHandler(w, r)
			return
		}
		// Default success: canned access + refresh + expires_in.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fake-access-tok","refresh_token":"fake-refresh-tok","token_type":"Bearer","expires_in":3600,"scope":"read write"}`))
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// === GET /link/oauth/<credname> ============================================

func TestHandleLinkOAuthGet_HappyPath(t *testing.T) {
	const (
		canonical = "user:alice@example.com"
		credName  = "linear-oauth"
	)
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred("default", "linear-mcp", credName, provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/oauth/"+credName, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code, "happy path → 302; body=%s", rec.Body.String())
	loc := rec.Header().Get("Location")
	require.NotEmpty(t, loc, "Location header set")

	u, err := url.Parse(loc)
	require.NoError(t, err)
	assert.Equal(t, provider.srv.URL, u.Scheme+"://"+u.Host, "authorize URL host matches provider")
	assert.Equal(t, "/authorize", u.Path, "authorize path matches")

	q := u.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "fake-client-123", q.Get("client_id"), "client_id from DCR")
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.NotEmpty(t, q.Get("code_challenge"), "code_challenge present")
	assert.NotEmpty(t, q.Get("state"), "state token present")
	assert.Equal(t,
		"https://identityd.example.org/oauth/callback/"+credName,
		q.Get("redirect_uri"),
		"redirect_uri = externalBaseURL + /oauth/callback/<credname>",
	)
	assert.Equal(t, "read write", q.Get("scope"), "scope joined from ScopesSupported")
}

func TestHandleLinkOAuthGet_NoCookie_Unauthorized(t *testing.T) {
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred("default", "linear-mcp", "linear-oauth", provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)

	req := httptest.NewRequest(http.MethodGet, "/link/oauth/linear-oauth", nil)
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no cookie → 401")
	assert.Contains(t, rec.Body.String(), "Sign-in required")
}

func TestHandleLinkOAuthGet_MissingCredName(t *testing.T) {
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, "user:alice@example.com", time.Time{})

	// GET /link/oauth/ (no credname) — the trailing slash matches the
	// registered prefix, the handler sees an empty credname.
	req := httptest.NewRequest(http.MethodGet, "/link/oauth/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Credential name")
}

func TestHandleLinkOAuthGet_MCPServerNotFound(t *testing.T) {
	// No MCPServer with the requested credname.
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, "user:alice@example.com", time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/oauth/nonexistent", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "No service")
}

func TestHandleLinkOAuthGet_DiscoveryFailure(t *testing.T) {
	// Build an MCPServer pointing at a host that won't respond.
	// safehttp would block it; use the stub HTTP client so the dial
	// itself happens but returns a connection error.
	installOAuthHTTPClient(t, &http.Client{Timeout: 1 * time.Second})

	mcpSrv := mcpServerWithCred("default", "linear-mcp", "linear-oauth", "http://127.0.0.1:1/unreachable")
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	cookie := mintCookie(t, fx.signer, "user:alice@example.com", time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/oauth/linear-oauth", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code, "discovery failure → 502; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unreachable")
}

func TestHandleLinkOAuthGet_OAuthClientUnavailable(t *testing.T) {
	// MCPServer advertises no DCR endpoint + no pre-registered client →
	// the handler cannot resolve a client_id and must 500 with a clear
	// message.
	provider := startOAuthFakeProvider(t)
	provider.registrationOpenAt = "absent" // omit registration_endpoint from metadata
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred("default", "linear-mcp", "linear-oauth", provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	cookie := mintCookie(t, fx.signer, "user:alice@example.com", time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/oauth/linear-oauth", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"no DCR + no pre-registered client → 500; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "OAuth client unavailable")
}

func TestHandleLinkOAuthGet_StateStoreEntryWritten(t *testing.T) {
	const (
		canonical = "user:bob@example.com"
		credName  = "linear-oauth"
	)
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred("default", "linear-mcp", credName, provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/oauth/"+credName, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code, "body=%s", rec.Body.String())

	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	stateTok := loc.Query().Get("state")
	require.NotEmpty(t, stateTok)

	entry, ok := fx.srv.oauthState.Consume(stateTok)
	require.True(t, ok, "state token resolves to a stored entry")
	assert.Equal(t, credName, entry.CredentialName)
	assert.Equal(t, identity.Subject(canonical), entry.Subject)
	assert.NotEmpty(t, entry.PKCEVerifier)
	assert.Equal(t, "default", entry.MCPServerNamespace)
	assert.Equal(t, "linear-mcp", entry.MCPServerName)
}

func TestHandleLinkOAuthGet_PathTraversalRejected(t *testing.T) {
	// Multi-segment credname must be rejected before any I/O — defense
	// against /link/oauth/foo/bar tricks.
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, "user:alice@example.com", time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/link/oauth/foo/bar", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// === GET /oauth/callback/<credname> ========================================

// seedOAuthState pre-populates the server's oauthStateStore with an entry
// keyed on a fresh state token. Returns the token. Tests call the
// callback handler with this token to exercise the Consume path without
// rebuilding the entire ε5 authorize path.
func seedOAuthState(t *testing.T, srv *Server, entry oauthStateEntry) string {
	t.Helper()
	tok, err := srv.oauthState.NewState(entry)
	require.NoError(t, err)
	return tok
}

func TestHandleOAuthCallbackGet_HappyPath(t *testing.T) {
	const (
		canonical = "user:alice@example.com"
		credName  = "linear-oauth"
	)
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred("default", "linear-mcp", credName, provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	// Seed state as if ε5 had just run: PKCEVerifier set, subject + MCP
	// server ref recorded.
	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     credName,
		Subject:            canonical,
		PKCEVerifier:       "fake-pkce-verifier",
		MCPServerNamespace: "default",
		MCPServerName:      "linear-mcp",
		ClientID:           "test-client-id",
		ClientSecret:       "",
	})

	q := url.Values{"code": {"fake-auth-code"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/"+credName+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code, "happy path → 302; body=%s", rec.Body.String())
	loc := rec.Header().Get("Location")
	assert.Equal(t, "/my/accounts?linked="+credName, loc, "redirects back to /my/accounts with ?linked=")

	// Token endpoint was hit exactly once.
	assert.Equal(t, 1, provider.tokenHits, "token endpoint called once")

	// Master Secret was created with the canned tokens.
	uiName := useridentity.NameForSubject(canonical)
	secName := useridentity.MasterSecretName(uiName, credName)
	var sec corev1.Secret
	require.NoError(t,
		fx.c.Get(context.Background(),
			clientpkg.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secName},
			&sec),
		"master Secret must be created")
	assert.Equal(t, "fake-access-tok", string(sec.Data["access_token"]))
	assert.Equal(t, "fake-refresh-tok", string(sec.Data["refresh_token"]))
	assert.Equal(t, "Bearer", string(sec.Data["token_type"]))
	assert.Equal(t, "read write", string(sec.Data["scope"]))
	assert.NotEmpty(t, sec.Data["expires_at"], "expires_at written (computed from expires_in)")

	// UserIdentity has the OAuth credential.
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui))
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, credName, ui.Spec.Credentials[0].Name)
	assert.Equal(t, "oauth", ui.Spec.Credentials[0].Type)

	// The state token is single-use: a second consume must fail.
	_, ok := fx.srv.oauthState.Consume(stateTok)
	assert.False(t, ok, "state token must be consumed after callback")
}

func TestHandleOAuthCallbackGet_ErrorParameter(t *testing.T) {
	// ?error=access_denied → 400 with a user-facing "Sign-in cancelled"
	// message; nothing else should run.
	fx := newLinkFixtureNoAuth(t)
	// No cookie needed — the provider-error short-circuits before
	// cookie/state checks (actually we run after Consume in the
	// happy-path code, but the ?error= branch fires first per the task
	// spec: "render error + return" before any state lookup).
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/callback/linear-oauth?error=access_denied&error_description=user%20refused", nil)
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "?error → 400")
	assert.Contains(t, rec.Body.String(), "Sign-in cancelled")
}

func TestHandleOAuthCallbackGet_MissingState(t *testing.T) {
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, "user:alice@example.com", time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/linear-oauth?code=foo", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Invalid callback")
}

func TestHandleOAuthCallbackGet_StateAlreadyConsumed(t *testing.T) {
	const canonical = "user:alice@example.com"
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     "linear-oauth",
		Subject:            canonical,
		PKCEVerifier:       "v",
		MCPServerNamespace: "default",
		MCPServerName:      "linear-mcp",
		ClientID:           "test-client-id",
		ClientSecret:       "",
	})
	// Consume once directly to simulate either replay or a race.
	_, ok := fx.srv.oauthState.Consume(stateTok)
	require.True(t, ok)

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/linear-oauth?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "consumed state → 400")
	assert.Contains(t, rec.Body.String(), "session expired")
}

func TestHandleOAuthCallbackGet_StateExpired(t *testing.T) {
	const canonical = "user:alice@example.com"
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	// Force the state store TTL to zero so any entry is expired the
	// instant it lands. This exercises the gcLocked/expired branches of
	// oauthStateStore.Consume.
	fx.srv.oauthState.ttl = 0

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     "linear-oauth",
		Subject:            canonical,
		PKCEVerifier:       "v",
		MCPServerNamespace: "default",
		MCPServerName:      "linear-mcp",
		ClientID:           "test-client-id",
		ClientSecret:       "",
	})

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/linear-oauth?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "expired state → 400")
	assert.Contains(t, rec.Body.String(), "session expired")
}

func TestHandleOAuthCallbackGet_CookieSubjectMismatch(t *testing.T) {
	// entry.Subject = alice, cookie = bob → 403 + state still consumed
	// (no replay).
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred("default", "linear-mcp", "linear-oauth", provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	bobCookie := mintCookie(t, fx.signer, "user:bob@example.com", time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     "linear-oauth",
		Subject:            "user:alice@example.com",
		PKCEVerifier:       "v",
		MCPServerNamespace: "default",
		MCPServerName:      "linear-mcp",
		ClientID:           "test-client-id",
		ClientSecret:       "",
	})

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/linear-oauth?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: bobCookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "cookie subject mismatch → 403")
	assert.Contains(t, rec.Body.String(), "Not your session")

	// State must have been consumed even though the cookie check failed —
	// prevents replay with a stolen state token.
	_, ok := fx.srv.oauthState.Consume(stateTok)
	assert.False(t, ok, "state must be consumed before cookie-gating to prevent replay")
}

func TestHandleOAuthCallbackGet_MissingCode(t *testing.T) {
	const canonical = "user:alice@example.com"
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred("default", "linear-mcp", "linear-oauth", provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     "linear-oauth",
		Subject:            canonical,
		PKCEVerifier:       "v",
		MCPServerNamespace: "default",
		MCPServerName:      "linear-mcp",
		ClientID:           "test-client-id",
		ClientSecret:       "",
	})

	// No code= param.
	q := url.Values{"state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/linear-oauth?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "missing code → 400")
	assert.Contains(t, rec.Body.String(), "Invalid callback")
	assert.Equal(t, 0, provider.tokenHits, "token endpoint never reached")
}

func TestHandleOAuthCallbackGet_TokenExchangeFailure(t *testing.T) {
	// Token endpoint returns 5xx → handler 502s.
	const canonical = "user:alice@example.com"
	provider := startOAuthFakeProvider(t)
	provider.tokenHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"server_error","error_description":"upstream broken"}`))
	}
	installOAuthHTTPClient(t, provider.srv.Client())

	mcpSrv := mcpServerWithCred("default", "linear-mcp", "linear-oauth", provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     "linear-oauth",
		Subject:            canonical,
		PKCEVerifier:       "v",
		MCPServerNamespace: "default",
		MCPServerName:      "linear-mcp",
		ClientID:           "test-client-id",
		ClientSecret:       "",
	})

	q := url.Values{"code": {"c"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/linear-oauth?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code, "token endpoint 5xx → 502")
	assert.Contains(t, rec.Body.String(), "Token exchange failed")

	// No credential should have been persisted.
	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	err := fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui)
	assert.Error(t, err, "no UserIdentity created on token-exchange failure")
}

func TestHandleOAuthCallbackGet_InvalidCredName(t *testing.T) {
	// Multi-segment credname must be rejected before any I/O.
	fx := newLinkFixtureNoAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/foo/bar?code=c&state=s", nil)
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Credential name")
}

func TestHandleOAuthCallbackGet_CredentialNameMismatchRejected(t *testing.T) {
	// Cross-service credential injection. The user links a HOSTILE provider's
	// credential ("hostile-oauth"); that provider controls where it redirects
	// the browser and what its own token endpoint returns, so it calls back on
	// /oauth/callback/<a DIFFERENT credential> carrying the same state + code.
	// The cookie check passes (same subject, same flow), and the token it
	// issues must NOT be filed under the victim's unrelated credential.
	const (
		canonical   = "user:alice@example.com"
		startedFor  = "hostile-oauth" // what the flow was actually started for
		victimCred  = "payroll-oauth" // an unrelated credential the user holds
		mcpServerNS = "default"
	)
	provider := startOAuthFakeProvider(t)
	installOAuthHTTPClient(t, provider.srv.Client())

	// Only the hostile MCPServer exists; the callback's credName is never
	// looked up, which is precisely what makes the injection work.
	mcpSrv := mcpServerWithCred(mcpServerNS, "hostile-mcp", startedFor, provider.srv.URL)
	fx := newLinkFixtureNoAuth(t, mcpSrv)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	stateTok := seedOAuthState(t, fx.srv, oauthStateEntry{
		CredentialName:     startedFor,
		Subject:            canonical,
		PKCEVerifier:       "fake-pkce-verifier",
		MCPServerNamespace: mcpServerNS,
		MCPServerName:      "hostile-mcp",
		ClientID:           "test-client-id",
	})

	q := url.Values{"code": {"fake-auth-code"}, "state": {stateTok}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/"+victimCred+"?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"a callback naming a credential the flow was not started for must be refused; body=%s", rec.Body.String())

	// The load-bearing assertion: nothing may be written under the credential
	// the attacker named.
	uiName := useridentity.NameForSubject(canonical)
	var sec corev1.Secret
	err := fx.c.Get(context.Background(), clientpkg.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      useridentity.MasterSecretName(uiName, victimCred),
	}, &sec)
	assert.Error(t, err,
		"the hostile provider's token was written into %q — a credential this flow was never started for", victimCred)

	var ui spiceboxv1alpha1.UserIdentity
	if getErr := fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui); getErr == nil {
		for _, cred := range ui.Spec.Credentials {
			assert.NotEqual(t, victimCred, cred.Name,
				"UserIdentity gained a %q entry from a flow started for %q", victimCred, startedFor)
		}
	}
}

// keep mcpoauth import referenced so future test additions don't need to
// re-add it. Removed if unused.
var _ mcpoauth.Metadata
