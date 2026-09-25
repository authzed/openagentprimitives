package adminui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/adminui"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

type stubDeps struct {
	baseURL, token string
	allow          map[string]bool // permission → allowed
	checkErr       error
	logger         logr.Logger
	// trustedOrigin is what the Origin pin on state-changing routes compares
	// against. Empty makes TrustedOriginMatch fail closed, which is the right
	// default for every row not about that gate.
	trustedOrigin string
}

func (d stubDeps) AdmindBaseURL() string { return d.baseURL }
func (d stubDeps) AdmindToken() string   { return d.token }
func (d stubDeps) CheckPlatformPermission(_ context.Context, permission string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	if d.checkErr != nil {
		return false, d.checkErr
	}
	return d.allow[permission], nil
}
func (d stubDeps) TrustedOrigin() string { return d.trustedOrigin }
func (d stubDeps) Logger() logr.Logger   { return d.logger }

func routesOf(t *testing.T, d webui.Deps) map[string]webui.Route {
	t.Helper()
	out := map[string]webui.Route{}
	for _, rt := range adminui.New().Routes(d) {
		out[rt.Pattern] = rt
	}
	return out
}

func TestRoutes_FailClosedWithoutConfig(t *testing.T) {
	assert.Empty(t, adminui.New().Routes(nil), "nil deps → no routes")
	assert.Empty(t, adminui.New().Routes(stubDeps{logger: testr.New(t)}), "no admind URL/token → no routes")
}

func TestRoutes_ShapeAndAuthorize(t *testing.T) {
	d := stubDeps{baseURL: "http://admind.test", token: "tok",
		allow:  map[string]bool{"view_overview": true, "view_sessions": true, "view_audit": false, "kill_session": false, "view_config": true},
		logger: testr.New(t)}
	routes := routesOf(t, d)

	page, ok := routes["/admin"]
	require.True(t, ok)
	assert.Equal(t, webui.AuthLoginIfNecessary, page.Auth)
	require.NotNil(t, page.Page)
	assert.Equal(t, "admin", page.Page.App)
	require.NotNil(t, page.Authorize, "page must carry the view_overview gate (Overview is the landing view)")
	assert.NoError(t, page.Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodGet, "/admin", nil)))

	overviewRt, ok := routes["/admin/api/overview"]
	require.True(t, ok, "overview proxy route must exist")
	assert.Equal(t, webui.AuthAuthorized, overviewRt.Auth)
	assert.NoError(t, overviewRt.Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)), "view_overview allowed")

	healthRt, ok := routes["/admin/api/health"]
	require.True(t, ok, "health proxy route must exist")
	assert.NoError(t, healthRt.Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodGet, "/admin/api/health", nil)), "view_overview allowed")

	clusterRt, ok := routes["/admin/api/cluster"]
	require.True(t, ok, "cluster proxy route must exist")
	assert.Equal(t, webui.AuthAuthorized, clusterRt.Auth)
	assert.NoError(t, clusterRt.Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodGet, "/admin/api/cluster", nil)), "view_overview allowed")

	budgetRt, ok := routes["/admin/api/budget"]
	require.True(t, ok, "budget proxy route must exist")
	assert.Equal(t, webui.AuthAuthorized, budgetRt.Auth)
	assert.NoError(t, budgetRt.Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodGet, "/admin/api/budget", nil)), "view_overview allowed")

	api, ok := routes["/admin/api/sessions/"]
	require.True(t, ok)
	assert.Equal(t, webui.AuthAuthorized, api.Auth)
	getReq := httptest.NewRequest(http.MethodGet, "/admin/api/sessions/default/s1", nil)
	assert.NoError(t, api.Authorize(context.Background(), "user:abc", getReq), "GET → view_sessions (allowed)")
	delReq := httptest.NewRequest(http.MethodDelete, "/admin/api/sessions/default/s1", nil)
	assert.Error(t, api.Authorize(context.Background(), "user:abc", delReq), "DELETE → kill_session (denied)")

	auditRt, ok := routes["/admin/api/audit/"]
	require.True(t, ok)
	assert.Error(t, auditRt.Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodPost, "/admin/api/audit/query", nil)))

	configRt, ok := routes["/admin/api/config/"]
	require.True(t, ok, "config proxy route must exist")
	assert.Equal(t, webui.AuthAuthorized, configRt.Auth)
	assert.NoError(t, configRt.Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodGet, "/admin/api/config/agents", nil)), "view_config allowed")

	// The exact artifacts route lists; the subtree route serves the detail
	// endpoint /admin/api/artifacts/{ns}/{name}. Both are view_audit-gated.
	artifactsRt, ok := routes["/admin/api/artifacts"]
	require.True(t, ok, "artifacts list proxy route must exist")
	assert.Error(t, artifactsRt.Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodGet, "/admin/api/artifacts", nil)), "view_audit denied here")

	artifactDetailRt, ok := routes["/admin/api/artifacts/"]
	require.True(t, ok, "artifacts detail subtree proxy route must exist")
	assert.Equal(t, webui.AuthAuthorized, artifactDetailRt.Auth)
	assert.Error(t, artifactDetailRt.Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodGet, "/admin/api/artifacts/default/ar-1", nil)), "view_audit denied here")
}

func TestCatchAllRoute_RegisteredTitledAndGated(t *testing.T) {
	d := stubDeps{baseURL: "http://admind.test", token: "tok",
		allow: map[string]bool{"view_overview": true}, logger: testr.New(t)}
	routes := routesOf(t, d)

	// Both the exact landing route and the deep-link catch-all must be Page
	// routes for the "admin" app, gated by view_overview, and titled with the
	// A1 browser-tab rename.
	for _, pat := range []string{"/admin", "/admin/{path...}"} {
		rt, ok := routes[pat]
		require.True(t, ok, "route %q must be registered so deep links load the SPA", pat)
		require.NotNil(t, rt.Page, "route %q must be a Page route", pat)
		assert.Equal(t, "admin", rt.Page.App)
		assert.Equal(t, webui.AuthLoginIfNecessary, rt.Auth)
		require.NotNil(t, rt.Authorize, "route %q must carry the view_overview gate", pat)
		assert.NoError(t, rt.Authorize(context.Background(), "user:abc",
			httptest.NewRequest(http.MethodGet, "/admin/anything/deep", nil)))

		_, meta, err := rt.Page.Build(context.Background(),
			httptest.NewRequest(http.MethodGet, "/admin", nil))
		require.NoError(t, err)
		assert.Equal(t, "Open Agent Primitives · Admin", meta.Title, "browser tab title names the product, not an abbreviation")
	}
}

// TestServer_CatchAllPageVsProxyPrecedence stands up a real webui.Server with
// the admin UI mounted and proves the Go 1.22 mux precedence the router relies
// on: a deep SPA link falls to the /admin/{path...} page, while the more-
// specific /admin/api/* proxy routes still win and reach admind.
func TestServer_CatchAllPageVsProxyPrecedence(t *testing.T) {
	var upstreamPaths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	d := stubDeps{baseURL: upstream.URL, token: "tok",
		allow:  map[string]bool{"view_overview": true, "view_sessions": true},
		logger: testr.New(t)}

	srv, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "user:abc", true }, // always authenticated
		nil,
		func() string { return "trusted.example" },
		func() string { return "sandbox.example" },
		nil, d, []webui.WebUI{adminui.New()},
	)
	require.NoError(t, err)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://trusted.example"+path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// A deep link into the SPA renders the admin page shell, not the proxy.
	deep := get("/admin/agent/default/x")
	assert.Equal(t, http.StatusOK, deep.Code)
	assert.Contains(t, deep.Body.String(), `data-app="admin"`, "deep link must serve the SPA document")
	assert.Empty(t, upstreamPaths, "a deep link must NOT hit the admind proxy")

	// The more-specific /admin/api/overview proxy route still wins over the
	// catch-all and forwards to admind's /admin/v1/overview.
	api := get("/admin/api/overview")
	assert.Equal(t, http.StatusOK, api.Code)
	assert.NotContains(t, api.Body.String(), `data-app="admin"`, "an api path must not render the page shell")
	require.Equal(t, []string{"/admin/v1/overview"}, upstreamPaths,
		"GET /admin/api/overview must proxy to admind, proving mux precedence over the catch-all")
}

func TestAuthorize_SpiceDBErrorIsPageError500(t *testing.T) {
	d := stubDeps{baseURL: "http://admind.test", token: "tok",
		checkErr: context.DeadlineExceeded, logger: testr.New(t)}
	routes := routesOf(t, d)
	err := routes["/admin"].Authorize(context.Background(), "user:abc", httptest.NewRequest(http.MethodGet, "/admin", nil))
	require.Error(t, err)
	var pe *webui.PageError
	require.ErrorAs(t, err, &pe, "check ERROR must surface as 500, not 403")
	assert.Equal(t, http.StatusInternalServerError, pe.Status)
}

func TestProxy_RewritesPathAndInjectsHeaders(t *testing.T) {
	var got *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	d := stubDeps{baseURL: upstream.URL, token: "tok",
		allow: map[string]bool{"view_sessions": true}, logger: testr.New(t)}
	routes := routesOf(t, d)
	h := routes["/admin/api/sessions"].Handler
	require.NotNil(t, h)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/sessions", nil)
	req = req.WithContext(webui.WithSubjectForTest(req.Context(), "user:abc"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, got)
	assert.Equal(t, "/admin/v1/sessions", got.URL.Path)
	assert.Equal(t, "Bearer tok", got.Header.Get("Authorization"))
	assert.Equal(t, "user:abc", got.Header.Get("X-Admin-Subject"))
}

func TestLogout_ExpiresCookieRedirectsAndNeedsNoAuth(t *testing.T) {
	d := stubDeps{baseURL: "http://admind.test", token: "tok",
		allow: map[string]bool{"view_overview": true}, logger: testr.New(t)}
	routes := routesOf(t, d)

	rt, ok := routes["/admin/logout"]
	require.True(t, ok, "logout route must be registered")
	assert.Equal(t, webui.AuthNone, rt.Auth, "logout must NOT require a valid session")
	require.NotNil(t, rt.Handler, "logout is a raw handler, not a page")

	req := httptest.NewRequest(http.MethodGet, "/admin/logout", nil)
	// A top-level navigation, which is what the Sign Out button produces
	// (window.location.assign). The route serves only this shape -- see
	// logout_navigation_test.go for why, and for the subresource cases.
	req.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	rt.Handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusFound, w.Code, "logout redirects")
	assert.Equal(t, "/admin", w.Header().Get("Location"))

	var cleared *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "idd_session" {
			cleared = c
		}
	}
	require.NotNil(t, cleared, "logout must Set-Cookie idd_session to clear it")
	assert.Less(t, cleared.MaxAge, 0, "idd_session must be expired (MaxAge<0)")
	assert.Empty(t, cleared.Value, "expired cookie carries no value")
	assert.Equal(t, "/", cleared.Path, "path must match the mint so the browser clears the same cookie")
}

func TestLoginLink_MintsAdminPurpose(t *testing.T) {
	signer := passthroughlink.New([]byte(strings.Repeat("k", 32)))
	d, sig, err := adminui.LoginLink(signer)
	require.NoError(t, err)
	p, err := signer.Verify(d + "." + sig)
	require.NoError(t, err)
	assert.Equal(t, passthroughlink.PurposeAdminLogin, p.Purpose)
	assert.Empty(t, p.Subject, "admin link must carry no subject")
	_ = url.QueryEscape(d) // links ride as query params
}

// --- the proxy-coverage guard ---------------------------------------------------

// wildcard matches one Go 1.22 mux wildcard segment, "{ns}" or "{id...}".
var wildcard = regexp.MustCompile(`\{([^}]*)\}`)

// samplePath turns a mux PATTERN into a concrete path a request can carry, by
// substituting each wildcard with a literal.
//
// A multi-segment wildcard ("{id...}") becomes TWO segments, because that is
// the case a prefix-swap proxy route has to survive: the config detail path is
// /config/{resource}/{ns}/{name}, and a sample with one segment would match a
// narrower proxy pattern than the real traffic does.
func samplePath(pattern string) string {
	return wildcard.ReplaceAllStringFunc(pattern, func(m string) string {
		if strings.HasSuffix(m, "...}") {
			return "sample/sample"
		}
		return "sample"
	})
}

// proxyMux rebuilds adminui's own routes as an http.ServeMux, so a coverage
// question can be answered by ASKING THE ROUTER rather than by comparing
// pattern strings.
//
// That distinction is the whole value of this guard. The bug it exists to catch
// was not a missing string — it was a POST falling through to the GET-only
// /admin/{path...} SPA catch-all, which no string comparison notices and which
// the router reports immediately. Registering every route, catch-all included,
// is what makes the fall-through visible.
func proxyMux(t *testing.T, d webui.Deps) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	for _, rt := range adminui.New().Routes(d) {
		methods := rt.Methods
		if len(methods) == 0 {
			methods = []string{http.MethodGet}
		}
		for _, m := range methods {
			mux.Handle(m+" "+rt.Pattern, http.NotFoundHandler())
		}
	}
	return mux
}

// TestEveryBrowserFacingAdmindRouteIsProxied.
//
// THE LIST IS DERIVED, not transcribed. It comes from admind.Routes(), which is
// the same slice admind.Handler() iterates to build its mux — so a route added
// there is checked here from that one edit. A hand-written list of "routes that
// must be proxied" would be the identical defect one layer up: it would have
// been written from the routes that existed the day it was written, and the
// next one added would be just as silently unreachable.
//
// WHAT IT PREVENTS HAD ALREADY SHIPPED. admind registered
// POST /admin/v1/agents/oap-install; the console's own TypeScript POSTed
// /admin/api/agents/oap-install (ui/lib/api.ts, postOapInstall); and no proxy
// entry connected them, so the request matched only the GET-only
// /admin/{path...} SPA catch-all. The install button had never worked, and
// nothing failed loudly enough for anyone to notice.
//
// A route that no browser reaches says so on its own row (admind.AudienceCluster)
// rather than being listed as an exception here — an exception list is the same
// transcription hazard in a smaller font.
func TestEveryBrowserFacingAdmindRouteIsProxied(t *testing.T) {
	d := stubDeps{baseURL: "http://admind.test", token: "tok", logger: testr.New(t)}
	mux := proxyMux(t, d)

	browserFacing := 0
	for _, rt := range admind.Routes() {
		if rt.Audience != admind.AudienceBrowser {
			continue
		}
		browserFacing++
		require.True(t, strings.HasPrefix(rt.Pattern, admind.APIPathPrefix),
			"admind route %q does not start with %q, so its browser path cannot be derived",
			rt.Pattern, admind.APIPathPrefix)

		path := admind.BrowserAPIPathPrefix + strings.TrimPrefix(samplePath(rt.Pattern), admind.APIPathPrefix)
		req := httptest.NewRequest(rt.Method, path, nil)
		_, matched := mux.Handler(req)
		// ServeMux reports the matched pattern as "<METHOD> <path>"; only the
		// path half answers "did this reach the proxy or the SPA catch-all?".
		_, matchedPath, _ := strings.Cut(matched, " ")

		// The failure message names the ROUTE, not a count: the whole failure
		// mode is "which one is missing", and a test that only reported a
		// mismatched total would leave that to be worked out by hand.
		assert.True(t, strings.HasPrefix(matchedPath, admind.BrowserAPIPathPrefix),
			"admind serves %s %s and nothing in adminui proxies it: %s %s matched %q "+
				"(want a %s* route). Add an entry to Routes() gated on %q, "+
				"or mark the route admind.AudienceCluster if no browser should reach it.",
			rt.Method, rt.Pattern, rt.Method, path, matched, admind.BrowserAPIPathPrefix, rt.Permission)
	}
	require.Positive(t, browserFacing,
		"no browser-facing routes were checked, so this guard asserted nothing")
}

// TestAProxiedRouteAsksForTheSamePermissionAdmindDoes.
//
// The proxy check is not decorative: it is what decides whether webd forwards
// the request at all, and admind's own `require` re-checks the same permission
// on the far side. Two different answers is either a door the console opens for
// someone admind will refuse (a confusing dead end) or — the direction that
// matters — a door the console opens for someone who should not have reached it.
//
// Only routes whose proxy pattern is EXACT are checked. A subtree entry
// ("/admin/api/sessions/") deliberately fronts several admind routes with
// different permissions — GET is view_sessions and DELETE is kill_session on
// the same path — and is resolved by its own Authorize func, which
// TestRoutes_ShapeAndAuthorize already covers.
func TestAProxiedRouteAsksForTheSamePermissionAdmindDoes(t *testing.T) {
	allow := map[string]bool{}
	for _, rt := range admind.Routes() {
		if rt.Permission != "" {
			allow[rt.Permission] = true
		}
	}
	d := stubDeps{baseURL: "http://admind.test", token: "tok", allow: allow, logger: testr.New(t)}

	byPattern := map[string]webui.Route{}
	for _, rt := range adminui.New().Routes(d) {
		byPattern[rt.Pattern] = rt
	}

	checked := 0
	for _, rt := range admind.Routes() {
		if rt.Audience != admind.AudienceBrowser || rt.Permission == "" {
			continue
		}
		want := admind.BrowserAPIPathPrefix + strings.TrimPrefix(rt.Pattern, admind.APIPathPrefix)
		proxy, ok := byPattern[want]
		if !ok {
			// A subtree route fronts this one; see the doc comment.
			continue
		}
		checked++
		require.NotNil(t, proxy.Authorize, "%s has no Authorize func", want)

		// Asked of the Authorize func rather than read off a field, because a
		// field does not exist: authorize(perm) is a closure. Denying exactly
		// this permission and allowing every other must make it refuse.
		denied := map[string]bool{}
		for p := range allow {
			denied[p] = p != rt.Permission
		}
		one := stubDeps{baseURL: "http://admind.test", token: "tok", allow: denied, logger: testr.New(t)}
		var target webui.Route
		for _, r := range adminui.New().Routes(one) {
			if r.Pattern == want {
				target = r
			}
		}
		require.NotNil(t, target.Authorize)
		err := target.Authorize(context.Background(), "user:abc", httptest.NewRequest(rt.Method, want, nil))
		assert.Error(t, err,
			"the proxy entry for %s must refuse a subject lacking %q — admind's own gate does",
			want, rt.Permission)
	}
	require.Positive(t, checked, "no exact-pattern routes were compared, so this guard asserted nothing")
}

// TestNoClusterOnlyAdmindRouteIsProxied is the OTHER direction, and it is not
// symmetry for its own sake.
//
// The coverage guard above skips every non-browser row, and its own failure
// text advertises "or mark the route admind.AudienceCluster" as the way out.
// With only that direction asserted, two opposite mistakes pass: a
// browser-facing route mislabelled AudienceCluster to silence the guard, and a
// server-to-server route accidentally published on the console's origin. The
// second is the one that matters — credentials/agent-update is called by
// identityd with its own service token and has no business behind a session
// cookie.
func TestNoClusterOnlyAdmindRouteIsProxied(t *testing.T) {
	d := stubDeps{baseURL: "http://admind.test", token: "tok", logger: testr.New(t)}
	mux := proxyMux(t, d)

	clusterOnly := 0
	for _, rt := range admind.Routes() {
		if rt.Audience != admind.AudienceCluster {
			continue
		}
		clusterOnly++
		path := admind.BrowserAPIPathPrefix + strings.TrimPrefix(samplePath(rt.Pattern), admind.APIPathPrefix)
		req := httptest.NewRequest(rt.Method, path, nil)
		_, matched := mux.Handler(req)
		_, matchedPath, _ := strings.Cut(matched, " ")

		assert.False(t, strings.HasPrefix(matchedPath, admind.BrowserAPIPathPrefix),
			"admind serves %s %s to in-cluster callers only, and adminui publishes it on the console origin: "+
				"%s %s matched %q. Remove the proxy entry, or change the route's Audience if a browser really should reach it.",
			rt.Method, rt.Pattern, rt.Method, path, matched)
	}
	require.Positive(t, clusterOnly,
		"no cluster-only routes were checked, so this guard asserted nothing")
}

// TestEveryProxyPatternUsesTheSharedPrefix pins the one string this package
// still spells for itself.
//
// proxyHandler's rewrite is built from admind.APIPathPrefix and
// admind.BrowserAPIPathPrefix, so the two halves of the swap cannot drift. The
// Pattern strings in the route table are the remaining copies, and admind
// derives its handoff CALLBACK URL from the same constant — so a table entry
// under a different prefix would produce an App created against an address that
// 404s, which is exactly what admind's base-URL check fails closed to avoid.
func TestEveryProxyPatternUsesTheSharedPrefix(t *testing.T) {
	d := stubDeps{baseURL: "http://admind.test", token: "tok", logger: testr.New(t)}
	api := 0
	for _, rt := range adminui.New().Routes(d) {
		// The SPA page routes are not API routes and are addressed elsewhere.
		if rt.Handler == nil {
			continue
		}
		if !strings.HasPrefix(rt.Pattern, "/admin/api") {
			continue
		}
		api++
		assert.True(t, strings.HasPrefix(rt.Pattern, admind.BrowserAPIPathPrefix),
			"route %q does not sit under admind.BrowserAPIPathPrefix (%q), so the proxy rewrite would not strip it",
			rt.Pattern, admind.BrowserAPIPathPrefix)
	}
	require.Positive(t, api, "no API routes were checked, so this guard asserted nothing")
}

// TestHandoffCallbackLogsInRatherThan401ingWithTheAppAlreadyCreated.
//
// By the time the external service redirects, the App EXISTS. AuthAuthorized
// answers a lapsed session with a 401 and leaves it orphaned — precisely the
// case admind's begin log line exists to make diagnosable. AuthLoginIfNecessary
// sends the operator through login and back with the query intact.
//
// It costs no authorization, and that is the half worth pinning rather than
// asserting from memory: the framework runs Authorize whenever it is set, on
// this level as much as on AuthAuthorized (webui/server.go's
// AuthLoginIfNecessary arm). An earlier version of this route used
// AuthAuthorized on the belief that AuthLoginIfNecessary "authenticates without
// authorizing", which is false for this framework.
func TestHandoffCallbackLogsInRatherThan401ingWithTheAppAlreadyCreated(t *testing.T) {
	d := stubDeps{baseURL: "http://admind.test", token: "tok",
		allow: map[string]bool{"install_agent": false}, logger: testr.New(t)}
	routes := routesOf(t, d)

	cb, ok := routes["/admin/api/agents/channel-handoff/callback"]
	require.True(t, ok, "the callback must be proxied at all")
	assert.Equal(t, webui.AuthLoginIfNecessary, cb.Auth,
		"a lapsed session on this route means an App already created and now unreachable")
	assert.Equal(t, []string{http.MethodGet}, cb.Methods,
		"AuthLoginIfNecessary is GET/HEAD-only, and a redirect is a GET")

	// The authorization is REAL, not skipped: the same subject the sibling
	// routes refuse is refused here.
	require.NotNil(t, cb.Authorize, "an unauthorized callback would drive a credential exchange for anyone")
	err := cb.Authorize(context.Background(), "user:abc",
		httptest.NewRequest(http.MethodGet, "/admin/api/agents/channel-handoff/callback?code=c&state=s", nil))
	assert.Error(t, err, "install_agent is denied for this subject, so the callback must refuse it")

	// And its siblings stay AuthAuthorized: only the redirect target has a
	// lapsed-session case worth spending a login round trip on.
	for _, p := range []string{
		"/admin/api/agents/oap-install",
		"/admin/api/agents/channel-setup",
		"/admin/api/agents/channel-handoff",
	} {
		rt, ok := routes[p]
		require.True(t, ok, "%s must be proxied", p)
		assert.Equal(t, webui.AuthAuthorized, rt.Auth, "%s is not a redirect target", p)
	}
}
