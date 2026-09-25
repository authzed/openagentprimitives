package identityd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	fakeidpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// newOIDCLoginFixtureWithIdP builds a Server wired with the given
// authenticators and a fake k8s client pre-seeded with the ClusterIdentityProvider
// CR + Secret (using defaultCR("fake") + defaultSecret()) plus any extra objs.
// Used by tests that exercise the IdP branch of handleOIDCLogin.
func newOIDCLoginFixtureWithIdP(t *testing.T, externalBase string, auths map[string]channelkinds.WebAuthenticator, objs ...clientpkg.Object) *Server {
	t.Helper()
	scheme := idpLoaderScheme(t)
	// seed the IdP CR + Secret alongside any test objects
	all := []clientpkg.Object{defaultCR("fake"), defaultSecret()}
	all = append(all, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).Build()
	return NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New(signerKey),
		ExternalBaseURL: func() string { return externalBase },
		Authenticators:  auths,
	})
}

// doIdPCallback fires a GET /oidc/callback/idp with the given code + state.
func doIdPCallback(t *testing.T, srv *Server, code, state string) *httptest.ResponseRecorder {
	t.Helper()
	return doOIDCCallback(t, srv, "idp", code, state)
}

// doIdPCallbackWithBinding is doIdPCallback for a test that drove the real begin
// handler and therefore holds a real, randomly-minted binding.
func doIdPCallbackWithBinding(t *testing.T, srv *Server, code, state, binding string) *httptest.ResponseRecorder {
	t.Helper()
	return doOIDCCallbackWithBinding(t, srv, "idp", code, state, binding)
}

// bindingValue pulls the login binding the begin handler set on rec. Failing
// here means a begin path stopped binding its flow to the browser at all.
func bindingValue(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	c := findCookie(rec, loginBindingCookie)
	require.NotNil(t, c, "starting a login must set the browser binding cookie")
	return c.Value
}

// primeIdPStateStore mints a state token into the server's state store and
// returns the (stateTok, linkRaw) pair.
func primeIdPStateStore(t *testing.T, srv *Server, subject identity.Subject) (stateTok, linkRaw string) {
	t.Helper()
	var err error
	linkRaw, err = srv.deps.LinkSigner.Mint(passthroughlink.Payload{
		SessionRef: "default/s1",
		Subject:    subject,
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	stateTok, err = srv.stateStore.NewStateWithNext(linkRaw, "", testLoginBinding, "idp")
	require.NoError(t, err)
	return stateTok, linkRaw
}

// TestHandleIdPCallback covers the major branches of the cluster-IdP callback.
func TestHandleIdPCallback(t *testing.T) {
	const (
		email     = "alice@example.com"
		canonical = "user:" + email
	)

	cases := []struct {
		name  string
		pre   func(t *testing.T, srv *Server) (code, state string)
		check func(t *testing.T, rec *httptest.ResponseRecorder)
	}{
		{
			name: "happy path: verified email in allowed domain → 302 + cookie",
			pre: func(t *testing.T, srv *Server) (string, string) {
				t.Helper()
				stateTok, _ := primeIdPStateStore(t, srv, canonical)
				fakeidpkind.NextPrincipal = identity.IdPUser(email, true, "Alice")
				t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })
				return "fake-code", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				require.Equal(t, http.StatusFound, rec.Code)
				loc := rec.Header().Get("Location")
				assert.True(t, strings.HasPrefix(loc, "/link?") || loc != "",
					"must redirect after IdP callback, got %q", loc)
				c := findCookie(rec, cookieName)
				require.NotNil(t, c, "idd_session cookie must be set")
				assert.True(t, c.HttpOnly, "cookie must be HttpOnly")
				assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
				// IdP callback uses sessionTTL (12h default), NOT cookieTTL (10m).
				// 12h = 43200s; cookieTTL = 600s.
				assert.Greater(t, c.MaxAge, int(cookieTTL/time.Second),
					"IdP-verified cookie must use sessionTTL, not cookieTTL")
			},
		},
		{
			name: "email not verified → 403 Sign-in not permitted",
			pre: func(t *testing.T, srv *Server) (string, string) {
				t.Helper()
				stateTok, _ := primeIdPStateStore(t, srv, canonical)
				fakeidpkind.NextPrincipal = identity.IdPUser(email, false, "Alice")
				t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })
				return "fake-code", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusForbidden, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in not permitted")
				assert.Contains(t, rec.Body.String(), "unverified")
			},
		},
		{
			name: "email domain not in allowed list → 403 Sign-in not permitted",
			pre: func(t *testing.T, srv *Server) (string, string) {
				t.Helper()
				stateTok, _ := primeIdPStateStore(t, srv, canonical)
				// defaultCR allows only "example.com"; attacker@evil.com must be denied.
				fakeidpkind.NextPrincipal = identity.IdPUser("attacker@evil.com", true, "Attacker")
				t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })
				return "fake-code", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusForbidden, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in not permitted")
				assert.Contains(t, rec.Body.String(), "permitted")
			},
		},
		{
			name: "missing state param → 400 Sign-in session expired",
			pre: func(t *testing.T, srv *Server) (string, string) {
				t.Helper()
				return "fake-code", "" // no state
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in session expired")
			},
		},
		{
			name: "unknown/used state token → 400 Sign-in session expired",
			pre: func(t *testing.T, srv *Server) (string, string) {
				t.Helper()
				return "fake-code", "nonexistent-state-token"
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in session expired")
			},
		},
		{
			name: "provider.Complete returns error → 500 Sign-in failed",
			pre: func(t *testing.T, srv *Server) (string, string) {
				t.Helper()
				stateTok, _ := primeIdPStateStore(t, srv, canonical)
				// Wrong code → fakekind returns an error.
				return "wrong-code", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusInternalServerError, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in failed")
			},
		},
		{
			name: "state already consumed: replay → 400 Sign-in session expired",
			pre: func(t *testing.T, srv *Server) (string, string) {
				t.Helper()
				stateTok, _ := primeIdPStateStore(t, srv, canonical)
				fakeidpkind.NextPrincipal = identity.IdPUser(email, true, "Alice")
				t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })
				// First call consumes the token.
				rec := doIdPCallback(t, srv, "fake-code", stateTok)
				require.Equal(t, http.StatusFound, rec.Code, "first call must succeed")
				// Return same state for the replay.
				return "fake-code", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in session expired")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOIDCLoginFixtureWithIdP(t, "https://identityd.example.org",
				map[string]channelkinds.WebAuthenticator{})
			code, state := tc.pre(t, srv)
			rec := doIdPCallback(t, srv, code, state)
			tc.check(t, rec)
		})
	}
}

// TestHandleIdPCallback_NoIdPConfigured ensures an /oidc/callback/idp call
// when no ClusterIdentityProvider is configured returns 500, not a panic or 404.
func TestHandleIdPCallback_NoIdPConfigured(t *testing.T) {
	// No CR or secret seeded — loader will return ErrIdPNotConfigured.
	scheme := idpLoaderScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New(signerKey),
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators:  map[string]channelkinds.WebAuthenticator{},
	})

	stateTok, _ := primeIdPStateStore(t, srv, "user:bob@example.com")
	rec := doIdPCallback(t, srv, "fake-code", stateTok)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "Sign-in unavailable")
}

// TestHandleOIDCLogin_IdPPresent_StartsIdPFlow exercises handleOIDCLogin's step-2
// branch: when a valid ClusterIdentityProvider is configured, /oidc/login must
// redirect to the IdP's authorize URL (not the channel-kind authenticator) and
// carry next in the state store.
func TestHandleOIDCLogin_IdPPresent_StartsIdPFlow(t *testing.T) {
	const (
		ns      = "default"
		name    = "s1"
		canon   = "user:alice@example.com"
		sessRef = ns + "/" + name
		nextURL = "https://trusted.example/artifact/v/abc"
	)
	// defaultCR uses "fake" kind whose Begin returns "<issuer>/authorize?state=<state>".
	// The issuer is empty in defaultCR spec (Issuer is ""), so Begin returns "/authorize?...".
	srv := newOIDCLoginFixtureWithIdP(t, "https://identityd.example.org",
		map[string]channelkinds.WebAuthenticator{"slack": &beginRecordingAuth{}},
		makeAgentSessionWithKind(ns, name, canon, "slack"))

	raw := mintLink(t, srv.deps.LinkSigner, sessRef, canon, []string{"cred"}, time.Time{})
	d, sig, ok := splitSignedLink(raw)
	require.True(t, ok)

	rec := doOIDCLogin(t, srv, d, sig, nextURL)
	require.Equal(t, http.StatusFound, rec.Code)
	loc := rec.Header().Get("Location")
	// Fake kind's Begin returns "<issuer>/authorize?state=<state>"; issuer is empty string.
	assert.Contains(t, loc, "authorize?state=", "must redirect to the IdP's Begin URL, got %q", loc)

	// State token must carry the next destination.
	q, err := url.ParseQuery(strings.TrimPrefix(strings.SplitN(loc, "?", 2)[1], ""))
	require.NoError(t, err)
	stateTok := q.Get("state")
	require.NotEmpty(t, stateTok)
	bindingCookie := findCookie(rec, loginBindingCookie)
	require.NotNil(t, bindingCookie, "starting a login must bind the flow to this browser")
	_, gotNext, refusal := srv.stateStore.Consume(stateTok, bindingCookie.Value, "idp")
	require.Equal(t, stateAccepted, refusal, "state token must be in store")
	assert.Equal(t, nextURL, gotNext, "state must carry next")
}

// TestIdPCallback_FederatedLogin_WritesIdPIdentitySecret asserts that when the
// IdP provider returns a non-nil *idp.TokenSet with a RefreshToken, the handler
// writes a Secret named via useridentity.IdPIdentitySecretName into
// IdentitiesNamespace containing all 7 expected keys with correct values.
func TestIdPCallback_FederatedLogin_WritesIdPIdentitySecret(t *testing.T) {
	const email = "alice@example.com"

	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	ts := &idp.TokenSet{
		RefreshToken:  "rt",
		AccessToken:   "at",
		TokenEndpoint: "https://idp.example.com/token",
		ClientID:      "ap",
		ClientSecret:  "sec",
		Scope:         "openid offline_access",
		ExpiresAt:     expiresAt,
	}

	srv := newOIDCLoginFixtureWithIdP(t, "https://identityd.example.org",
		map[string]channelkinds.WebAuthenticator{})

	stateTok, _ := primeIdPStateStore(t, srv, "user:"+email)
	fakeidpkind.NextPrincipal = identity.IdPUser(email, true, "Alice")
	fakeidpkind.NextTokenSet = ts
	t.Cleanup(func() {
		fakeidpkind.NextPrincipal = identity.Principal{}
		fakeidpkind.NextTokenSet = nil
	})

	rec := doIdPCallback(t, srv, "fake-code", stateTok)
	require.Equal(t, http.StatusFound, rec.Code, "callback must succeed with 302")

	// Retrieve the written Secret from the fake K8s client.
	principal := identity.IdPUser(email, true, "Alice")
	subject, err := principal.Subject()
	require.NoError(t, err)
	secretName := useridentity.IdPIdentitySecretName(subject.String())

	var sec corev1.Secret
	require.NoError(t,
		srv.deps.K8s.Get(context.Background(),
			clientpkg.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secretName},
			&sec),
		"IdP identity Secret must have been created in IdentitiesNamespace")

	// Assert all 7 keys are present with correct values.
	assert.Equal(t, "at", string(sec.Data["access_token"]))
	assert.Equal(t, "rt", string(sec.Data["refresh_token"]))
	assert.Equal(t, "https://idp.example.com/token", string(sec.Data["token_endpoint"]))
	assert.Equal(t, "ap", string(sec.Data["client_id"]))
	assert.Equal(t, "sec", string(sec.Data["client_secret"]))
	assert.Equal(t, "openid offline_access", string(sec.Data["scope"]))

	// expires_at must be present and parse as RFC3339.
	expiresAtRaw, ok := sec.Data["expires_at"]
	require.True(t, ok, "expires_at key must be present")
	parsed, err := time.Parse(time.RFC3339, string(expiresAtRaw))
	require.NoError(t, err, "expires_at must be a valid RFC3339 timestamp")
	assert.True(t, parsed.Equal(expiresAt), "expires_at must round-trip: got %s, want %s",
		parsed.UTC(), expiresAt.UTC())
}

// TestIdPCallback_FederatedLogin_SecretWriteFailure_LoginStillSucceeds proves
// best-effort behavior: a Secret write failure must NOT cause login to fail.
// The handler must still redirect (302) even when K8s Create returns an error.
func TestIdPCallback_FederatedLogin_SecretWriteFailure_LoginStillSucceeds(t *testing.T) {
	const email = "alice@example.com"

	expiresAt := time.Now().Add(time.Hour).UTC()
	ts := &idp.TokenSet{
		RefreshToken:  "rt",
		AccessToken:   "at",
		TokenEndpoint: "https://idp.example.com/token",
		ClientID:      "ap",
		ClientSecret:  "sec",
		Scope:         "openid offline_access",
		ExpiresAt:     expiresAt,
	}

	// Build a fake client that fails on Secret Create, to force the write error.
	scheme := idpLoaderScheme(t)
	all := []clientpkg.Object{defaultCR("fake"), defaultSecret()}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl clientpkg.WithWatch, obj clientpkg.Object, opts ...clientpkg.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return errors.New("injected Secret create failure")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New(signerKey),
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators:  map[string]channelkinds.WebAuthenticator{},
	})

	stateTok, _ := primeIdPStateStore(t, srv, "user:"+email)
	fakeidpkind.NextPrincipal = identity.IdPUser(email, true, "Alice")
	fakeidpkind.NextTokenSet = ts
	t.Cleanup(func() {
		fakeidpkind.NextPrincipal = identity.Principal{}
		fakeidpkind.NextTokenSet = nil
	})

	rec := doIdPCallback(t, srv, "fake-code", stateTok)
	// Login must still succeed (302) even though the Secret write failed.
	assert.Equal(t, http.StatusFound, rec.Code,
		"Secret write failure must not abort login; got body: %s", rec.Body.String())
	c2 := findCookie(rec, cookieName)
	assert.NotNil(t, c2, "idd_session cookie must still be set despite Secret write failure")
}

// TestEnforceIdPPolicy covers the policy check rules in isolation.
func TestEnforceIdPPolicy(t *testing.T) {
	cases := []struct {
		name       string
		principal  identity.Principal
		resolved   *resolvedIdP
		wantEmpty  bool // true → allowed ("" returned)
		wantSubstr string
	}{
		{
			name:       "unverified email → denied",
			principal:  identity.IdPUser("alice@example.com", false, "Alice"),
			resolved:   &resolvedIdP{allowedDomains: []string{"example.com"}},
			wantEmpty:  false,
			wantSubstr: "unverified",
		},
		{
			name:      "verified email + allowed domain → permitted",
			principal: identity.IdPUser("alice@example.com", true, "Alice"),
			resolved:  &resolvedIdP{allowedDomains: []string{"example.com"}},
			wantEmpty: true,
		},
		{
			name:      "verified email + allowAny → permitted",
			principal: identity.IdPUser("anyone@anywhere.io", true, "Anyone"),
			resolved:  &resolvedIdP{allowAny: true},
			wantEmpty: true,
		},
		{
			name:       "verified email + domain not in allowed list → denied",
			principal:  identity.IdPUser("attacker@evil.com", true, "Attacker"),
			resolved:   &resolvedIdP{allowedDomains: []string{"example.com"}},
			wantEmpty:  false,
			wantSubstr: "permitted",
		},
		{
			name:       "verified email + empty domain list (not allowAny) → denied",
			principal:  identity.IdPUser("alice@example.com", true, "Alice"),
			resolved:   &resolvedIdP{allowedDomains: nil, allowAny: false},
			wantEmpty:  false,
			wantSubstr: "permitted",
		},
		{
			name:      "verified email + domain match is case-insensitive → permitted",
			principal: identity.IdPUser("alice@EXAMPLE.COM", true, "Alice"),
			resolved:  &resolvedIdP{allowedDomains: []string{"example.com"}},
			wantEmpty: true,
		},
		{
			name:       "empty email under allowAny → denied",
			principal:  identity.IdPUser("", true, ""),
			resolved:   &resolvedIdP{allowAny: true},
			wantEmpty:  false,
			wantSubstr: "email address",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := enforceIdPPolicy(tc.principal, tc.resolved)
			if tc.wantEmpty {
				assert.Empty(t, msg, "expected allowed (empty message)")
			} else {
				assert.NotEmpty(t, msg, "expected denial message")
				if tc.wantSubstr != "" {
					assert.Contains(t, msg, tc.wantSubstr)
				}
			}
		})
	}
}
