package identityd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// scriptedAuth is a minimal WebAuthenticator: Begin returns a fixed URL.
type scriptedAuth struct{ beginURL string }

func (s scriptedAuth) Begin(_ context.Context, state string) (string, error) {
	return s.beginURL + "?state=" + url.QueryEscape(state), nil
}
func (s scriptedAuth) Complete(context.Context, channelkinds.CallbackParams) (string, error) {
	return "user:YWRtaW4", nil
}

// newAdminFixture builds a test Server with the given authenticators, using
// the package's existing test scaffolding (signerKey package var, newScheme).
func newAdminFixture(t *testing.T, auths map[string]channelkinds.WebAuthenticator) (*Server, *passthroughlink.Signer) {
	t.Helper()
	signer := passthroughlink.New(signerKey)
	srv := NewServer(Deps{
		K8s:             fake.NewClientBuilder().WithScheme(newScheme(t)).Build(),
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators:  auths,
	})
	return srv, signer
}

func mintAdminLink(t *testing.T, signer *passthroughlink.Signer) (d, sig string) {
	t.Helper()
	raw, err := signer.Mint(passthroughlink.Payload{
		Purpose:   passthroughlink.PurposeAdminLogin,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err)
	// Mint output is "<b64>.<hexmac>" — split on the final '.' into d + sig.
	i := strings.LastIndexByte(raw, '.')
	require.Greater(t, i, 0)
	return raw[:i], raw[i+1:]
}

func TestAdminLogin_DispatchPreference(t *testing.T) {
	cases := []struct {
		name      string
		auths     map[string]channelkinds.WebAuthenticator
		kindParam string
		wantHost  string // expected Begin redirect host; "" = expect 5xx error page
	}{
		{"single authenticator → used", map[string]channelkinds.WebAuthenticator{
			"fake": scriptedAuth{beginURL: "https://fake.test/auth"}}, "", "fake.test"},
		{"slack preferred over others", map[string]channelkinds.WebAuthenticator{
			"fake":  scriptedAuth{beginURL: "https://fake.test/auth"},
			"slack": scriptedAuth{beginURL: "https://slack.test/auth"}}, "", "slack.test"},
		{"?kind= override wins", map[string]channelkinds.WebAuthenticator{
			"fake":  scriptedAuth{beginURL: "https://fake.test/auth"},
			"slack": scriptedAuth{beginURL: "https://slack.test/auth"}}, "fake", "fake.test"},
		{"no authenticators → error page, NOT trust-link cookie", map[string]channelkinds.WebAuthenticator{}, "", ""},
		{"bogus ?kind= with valid authenticators → error, no cookie", map[string]channelkinds.WebAuthenticator{
			"fake":  scriptedAuth{beginURL: "https://fake.test/auth"},
			"slack": scriptedAuth{beginURL: "https://slack.test/auth"}}, "nonexistent", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, signer := newAdminFixture(t, tc.auths)
			d, sig := mintAdminLink(t, signer)
			u := "/oidc/login?d=" + url.QueryEscape(d) + "&sig=" + url.QueryEscape(sig) + "&next=%2Fadmin"
			if tc.kindParam != "" {
				u += "&kind=" + tc.kindParam
			}
			req := httptest.NewRequest(http.MethodGet, u, nil)
			w := httptest.NewRecorder()
			srv.handleOIDCLogin(w, req)

			if tc.wantHost == "" {
				assert.GreaterOrEqual(t, w.Code, 500, "no authenticator must be an error, never a trust-link cookie")
				assert.Empty(t, w.Result().Cookies(), "must NOT set a cookie")
				return
			}
			require.Equal(t, http.StatusFound, w.Code)
			loc, err := url.Parse(w.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, tc.wantHost, loc.Host)
		})
	}
}

// doAdminLogin fires GET /oidc/login for a freshly-minted admin link with
// next=/admin and the optional ?kind= override.
func doAdminLogin(t *testing.T, srv *Server, kindParam string) *httptest.ResponseRecorder {
	t.Helper()
	d, sig := mintAdminLink(t, srv.deps.LinkSigner)
	u := "/oidc/login?d=" + url.QueryEscape(d) + "&sig=" + url.QueryEscape(sig) + "&next=%2Fadmin"
	if kindParam != "" {
		u += "&kind=" + url.QueryEscape(kindParam)
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	srv.handleOIDCLogin(w, req)
	return w
}

// TestAdminLogin_PrefersClusterIdP asserts that when a cluster IdP is
// configured, admin login starts the IdP OIDC flow (the cluster's canonical
// browser identity) rather than a channel-kind authenticator — even though a
// channel-kind authenticator IS registered. This is the bug the user hit: a
// configured Google IdP was ignored and admin login fell through to a
// channel-kind authenticator that wasn't usable.
func TestAdminLogin_PrefersClusterIdP(t *testing.T) {
	// A channel-kind authenticator IS registered; the IdP must still win.
	srv := newOIDCLoginFixtureWithIdP(t, "https://identityd.example.org",
		map[string]channelkinds.WebAuthenticator{
			"slack": scriptedAuth{beginURL: "https://slack.test/auth"}})

	w := doAdminLogin(t, srv, "")

	require.Equal(t, http.StatusFound, w.Code)
	loc := w.Header().Get("Location")
	// The fake IdP kind's Begin returns "<issuer>/authorize?state=…" (issuer
	// empty in defaultCR → "/authorize?state=…").
	assert.Contains(t, loc, "authorize?state=",
		"admin login must start the cluster IdP flow, got %q", loc)
	assert.NotContains(t, loc, "slack.test",
		"must NOT use a channel-kind authenticator when a cluster IdP is configured")
}

// TestAdminLogin_ExplicitKindDoesNotOverrideAConfiguredIdP pins the reversal of
// what this test used to assert.
//
// It previously required ?kind= to select a channel authenticator even with a
// cluster IdP configured, framed as "keeping every channel kind reachable for
// the multi-auth case". That is a policy BYPASS rather than a preference:
// enforceIdPPolicy is the only enforcement of the cluster's declared
// emailVerified and allowedEmailDomains rules, it runs on the IdP callback, and
// the channel-kind callback never calls it. So the override took a real,
// registered authenticator and minted a full idd_session with the cluster's
// login policy unconsulted — reachable with the signed d/sig pair that GET
// /admin hands any cookie-less browser.
//
// The escape hatch survives where it was needed: with NO cluster IdP
// configured, ?kind= still selects among the channel authenticators
// (TestAdminLogin_NoIdP_UsesExplicitKind below).
func TestAdminLogin_ExplicitKindDoesNotOverrideAConfiguredIdP(t *testing.T) {
	srv := newOIDCLoginFixtureWithIdP(t, "https://identityd.example.org",
		map[string]channelkinds.WebAuthenticator{
			"slack": scriptedAuth{beginURL: "https://slack.test/auth"}})

	w := doAdminLogin(t, srv, "slack")

	require.Equal(t, http.StatusFound, w.Code)
	loc := w.Header().Get("Location")
	assert.Contains(t, loc, "authorize?state=",
		"a configured cluster IdP governs admin sign-in; ?kind= must not route around it")
	assert.NotContains(t, loc, "slack.test",
		"the channel authenticator would skip enforceIdPPolicy entirely")
}

// The escape hatch itself, unchanged: with no cluster IdP there is no policy to
// skip, and ?kind= is the only way to pick among several channel kinds.
func TestAdminLogin_NoIdP_UsesExplicitKind(t *testing.T) {
	srv := newOIDCLoginFixture(t, "https://identityd.example.org",
		map[string]channelkinds.WebAuthenticator{
			"slack": scriptedAuth{beginURL: "https://slack.test/auth"},
			"bento": scriptedAuth{beginURL: "https://bento.test/auth"}})

	w := doAdminLogin(t, srv, "bento")

	require.Equal(t, http.StatusFound, w.Code)
	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "bento.test", loc.Host,
		"with no cluster IdP, ?kind= still selects among the channel authenticators")
}
