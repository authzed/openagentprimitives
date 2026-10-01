package identityd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	webuiregistry "github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

// fakeWebDeps is a stand-in for internal/cmd/webd's concrete deps; it satisfies the
// identityd.WebDeps interface so ui.Routes can build a Server.
type fakeWebDeps struct {
	k8s  clientpkg.Client
	icon http.Handler
}

func newFakeWebDeps(t *testing.T, icon http.Handler) fakeWebDeps {
	t.Helper()
	scheme := newScheme(t)
	return fakeWebDeps{
		k8s:  fake.NewClientBuilder().WithScheme(scheme).Build(),
		icon: icon,
	}
}

func (d fakeWebDeps) K8s() clientpkg.Client               { return d.k8s }
func (d fakeWebDeps) LinkSigner() *passthroughlink.Signer { return passthroughlink.New(signerKey) }
func (d fakeWebDeps) ExternalBaseURL() string             { return "https://identityd.example.org" }
func (d fakeWebDeps) IconHandler() http.Handler           { return d.icon }
func (d fakeWebDeps) Authenticators() map[string]channelkinds.WebAuthenticator {
	return map[string]channelkinds.WebAuthenticator{}
}
func (d fakeWebDeps) InsecureTrustLinks() bool { return false }

// ConsentClasses makes fakeWebDeps satisfy identityd.ConsentDeps (the Task 7
// optional interface), so the OAuth authorization-server routes (metadata,
// register, authorize, consent) are part of the base route set these tests
// assert against.
func (d fakeWebDeps) ConsentClasses(_ context.Context, _ identity.CanonicalUserID) ([]ConsentClass, error) {
	return []ConsentClass{{ID: "default/demo-agent", DisplayName: "Demo"}}, nil
}

// routeKey is patterns paired with the auth level we expect.
func routeIndex(routes []webui.Route) map[string]webui.Route {
	out := make(map[string]webui.Route, len(routes))
	for _, r := range routes {
		out[r.Pattern] = r
	}
	return out
}

func TestIdentityUI_RegistersViaInit(t *testing.T) {
	names := map[string]bool{}
	for _, u := range webuiregistry.All() {
		names[u.Name()] = true
	}
	assert.True(t, names["identity"], "identity UI must self-register via init()")
}

func TestIdentityUI_Routes_NonWebDeps_ReturnsNil(t *testing.T) {
	// A deps value that does NOT implement WebDeps must fail closed.
	assert.Nil(t, ui{}.Routes("not-web-deps"), "non-WebDeps deps must yield no routes")
	assert.Nil(t, ui{}.Routes(nil), "nil deps must yield no routes")
}

func TestIdentityUI_Routes_PatternsAndAuthLevels(t *testing.T) {
	routes := ui{}.Routes(newFakeWebDeps(t, nil))
	idx := routeIndex(routes)

	// /healthz is intentionally omitted (the health WebUI owns it).
	assert.NotContains(t, idx, "/healthz", "/healthz must not be served by identity")

	type want struct {
		auth    webui.AuthLevel
		methods []string
	}
	expected := map[string]want{
		"/.well-known/oauth-authorization-server": {webui.AuthNone, []string{http.MethodGet}},
		"/oauth/register":                         {webui.AuthHandlerManaged, []string{http.MethodPost}},
		"/oauth/authorize":                        {webui.AuthLoginIfNecessary, []string{http.MethodGet}},
		"/oauth/consent":                          {webui.AuthAuthenticated, []string{http.MethodPost}},
		"/link":                                   {webui.AuthNone, []string{http.MethodGet}},
		"/link/agent-oauth/":                      {webui.AuthNone, []string{http.MethodGet}},
		"/oidc/login":                             {webui.AuthNone, []string{http.MethodGet}},
		"/oidc/callback/":                         {webui.AuthNone, []string{http.MethodGet}},
		"/oauth/callback/":                        {webui.AuthNone, []string{http.MethodGet}},
		"/oauth/agent-callback/":                  {webui.AuthNone, []string{http.MethodGet}},
		"/my/accounts":                            {webui.AuthNone, []string{http.MethodGet}},
		"/cli/login":                              {webui.AuthNone, []string{http.MethodGet}},
		"/cli/exchange":                           {webui.AuthHandlerManaged, []string{http.MethodPost}},
		"/password/login":                         {webui.AuthNone, []string{http.MethodGet}},
		"/password/verify":                        {webui.AuthHandlerManaged, []string{http.MethodPost}},
		"/link/submit":                            {webui.AuthAuthenticated, []string{http.MethodPost}},
		"/my/accounts/":                           {webui.AuthAuthenticated, []string{http.MethodGet, http.MethodPost}},
		"/heartbeat":                              {webui.AuthAuthenticated, []string{http.MethodPost}},
		"/link/oauth/":                            {webui.AuthAuthenticated, []string{http.MethodGet}},
	}
	assert.Len(t, routes, len(expected), "no-icon deps yields exactly the base route set")
	for pattern, w := range expected {
		rt, ok := idx[pattern]
		require.True(t, ok, "route %q must be present", pattern)
		assert.Equal(t, w.auth, rt.Auth, "route %q auth level", pattern)
		assert.Equal(t, w.methods, rt.Methods, "route %q methods", pattern)
		assert.Equal(t, webui.OriginTrusted, rt.Origin, "route %q must be trusted-origin", pattern)
		assert.NotNil(t, rt.Handler, "route %q must have a handler", pattern)
	}
}

func TestIdentityUI_Routes_IconConditional(t *testing.T) {
	// expected mirrors the base route set from TestIdentityUI_Routes_PatternsAndAuthLevels
	// so that "icon adds exactly one route" is expressed as len(expected)+1.
	expected := map[string]struct{}{
		"/.well-known/oauth-authorization-server": {},
		"/oauth/register":                         {},
		"/oauth/authorize":                        {},
		"/oauth/consent":                          {},
		"/link":                                   {},
		"/link/agent-oauth/":                      {},
		"/oidc/login":                             {},
		"/oidc/callback/":                         {},
		"/oauth/callback/":                        {},
		"/oauth/agent-callback/":                  {},
		"/my/accounts":                            {},
		"/cli/login":                              {},
		"/cli/exchange":                           {},
		"/password/login":                         {},
		"/password/verify":                        {},
		"/link/submit":                            {},
		"/my/accounts/":                           {},
		"/heartbeat":                              {},
		"/link/oauth/":                            {},
	}
	iconH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	routes := ui{}.Routes(newFakeWebDeps(t, iconH))
	idx := routeIndex(routes)

	assert.Len(t, routes, len(expected)+1, "icon deps adds exactly one route")
	rt, ok := idx["/icon/"]
	require.True(t, ok, "/icon/ must be present when IconHandler is non-nil")
	assert.Equal(t, webui.AuthNone, rt.Auth)
	assert.Equal(t, webui.OriginTrusted, rt.Origin)
	assert.Equal(t, []string{http.MethodGet}, rt.Methods)

	// And absent when the handler is nil.
	noIcon := routeIndex(ui{}.Routes(newFakeWebDeps(t, nil)))
	assert.NotContains(t, noIcon, "/icon/", "/icon/ must be absent when IconHandler is nil")
}

// TestServer_Handler_MountsEveryDeclaredRoute is the regression guard for the
// bug where /cli/login was declared in one route registration but never
// mounted on the surface webd serves. Now there is ONE table (Server.routes())
// feeding both Handler() and ui.Routes(); this asserts every entry in that
// table is actually reachable on Handler() — a mounted route returns the
// handler's own status (400/403/302), never a 404 from an unmounted pattern.
func TestServer_Handler_MountsEveryDeclaredRoute(t *testing.T) {
	srv := NewServer(Deps{
		K8s:             fake.NewClientBuilder().WithScheme(newScheme(t)).Build(),
		LinkSigner:      passthroughlink.New(signerKey),
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators:  map[string]channelkinds.WebAuthenticator{},
	})
	h := srv.Handler()
	for _, rt := range srv.routes() {
		method := http.MethodGet
		if len(rt.Methods) > 0 {
			method = rt.Methods[0]
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "http://id.example"+rt.Pattern, nil))
		assert.NotEqual(t, http.StatusNotFound, rec.Code,
			"declared route %q (%s) must be mounted on Handler()", rt.Pattern, method)
	}
}

func TestIdentityUI_Routes_MountSucceeds(t *testing.T) {
	// The framework's NewServer enforces that AuthNone routes are GET/HEAD
	// only + that auth levels are known. Mounting our routes must pass.
	_, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "", false },
		nil,
		func() string { return "trusted.example" }, func() string { return "sandbox.example" }, nil,
		newFakeWebDeps(t, nil),
		[]webui.WebUI{ui{}},
	)
	require.NoError(t, err, "identity routes must mount cleanly on the webui framework")
}
