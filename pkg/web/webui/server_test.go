package webui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

type testUI struct {
	name   string
	routes []webui.Route
}

func (u testUI) Name() string                    { return u.name }
func (u testUI) Routes(webui.Deps) []webui.Route { return u.routes }

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
}

// newServer wires uis into a webui.Server with the given authenticate func
// (nil ⇒ never authenticated) and no component deps — server_test.go covers
// framework-only concerns.
func newServer(t *testing.T, authenticate func(*http.Request) (string, bool), uis ...webui.WebUI) *webui.Server {
	t.Helper()
	s, err := webui.NewServer(authenticate, nil, hostGetter("trusted.example"), hostGetter("sandbox.example"), nil, nil, uis)
	require.NoError(t, err)
	return s
}

// hostGetter returns a fixed-host getter for tests that don't exercise the
// dynamic-rotation path (dispatch is per-request but the value never changes).
func hostGetter(h string) func() string { return func() string { return h } }

func req(host, method, path string) *http.Request {
	r := httptest.NewRequest(method, "http://"+host+path, nil)
	r.Host = host
	return r
}

func TestServer_DynamicHostDispatch(t *testing.T) {
	th, sh := "trusted.example", "sandbox.example"
	ui := testUI{name: "d", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/t", Handler: okHandler("T"), Auth: webui.AuthNone},
		{Origin: webui.OriginSandbox, Pattern: "/s", Handler: okHandler("S"), Auth: webui.AuthNone},
	}}
	s, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "", false }, nil,
		func() string { return th }, func() string { return sh },
		nil, nil, []webui.WebUI{ui})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/t"))
	assert.Equal(t, "T", rec.Body.String(), "dispatch trusted")

	th, sh = "new-trusted.example", "new-sandbox.example"
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req("new-trusted.example", http.MethodGet, "/t"))
	assert.Equal(t, "T", rec.Body.String(), "dispatch follows the live trusted getter")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/t"))
	assert.Equal(t, http.StatusNotFound, rec.Code, "the OLD host no longer matches")
}

func TestServer_EmptyHostsAreIdleNotFound(t *testing.T) {
	ui := testUI{name: "d", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/t", Handler: okHandler("T"), Auth: webui.AuthNone},
	}}
	s, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "", false }, nil,
		func() string { return "" }, func() string { return "" },
		nil, nil, []webui.WebUI{ui})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("anything", http.MethodGet, "/healthz"))
	assert.Equal(t, http.StatusOK, rec.Code, "healthz answers while idle-but-ready")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req("anything", http.MethodGet, "/t"))
	assert.Equal(t, http.StatusNotFound, rec.Code, "empty hosts → 404, never a route")
}

func TestServer_EqualHostsWithoutSharedOriginFailClosed(t *testing.T) {
	ui := testUI{name: "d", routes: []webui.Route{
		{Origin: webui.OriginSandbox, Pattern: "/s", Handler: okHandler("S"), Auth: webui.AuthNone},
	}}
	s, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "", false }, nil,
		func() string { return "same.example" }, func() string { return "same.example" },
		nil /* sharedOriginOK: never shared */, nil, []webui.WebUI{ui})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("same.example", http.MethodGet, "/s"))
	assert.Equal(t, http.StatusNotFound, rec.Code, "equal hosts w/o shared-origin → fail closed (no implicit merge)")
}

func TestServer_SharedOrigin_ServesBothRouteSets(t *testing.T) {
	ui := testUI{name: "d", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/t", Handler: okHandler("T"), Auth: webui.AuthNone},
		{Origin: webui.OriginSandbox, Pattern: "/s", Handler: okHandler("S"), Auth: webui.AuthNone},
	}}
	s, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "", false }, nil,
		hostGetter("shared.ngrok-free.app"), hostGetter("shared.ngrok-free.app"),
		func(string) bool { return true } /* sharedOriginOK */, nil, []webui.WebUI{ui})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("shared.ngrok-free.app", http.MethodGet, "/t"))
	assert.Equal(t, "T", rec.Body.String(), "trusted route served on the shared host")

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req("shared.ngrok-free.app", http.MethodGet, "/s"))
	assert.Equal(t, "S", rec.Body.String(), "sandbox route also served on the shared host")
}

// desktopSharedOriginOK is webd's own predicate for a cluster kind whose
// InstallProfile allows a shared origin: the tunnel's host qualifies, and so
// does the loopback the operator's browser actually reaches webd on. Written
// here rather than imported because internal/cmd/webd is a main package; the
// loopback half is webui.IsLoopbackHost, which is the half that must not drift.
func desktopSharedOriginOK(host string) bool {
	return strings.Contains(host, "ngrok") || webui.IsLoopbackHost(host)
}

// TestServer_SharedOrigin_LoopbackIsStillServedOnceTheTunnelIsUp is the
// desktop's own UI, and it broke precisely when the tunnel SUCCEEDED: the
// PublicEndpoint controller writes the public URL into both external-URL keys,
// so trusted and sandbox both name the tunnel, while the desktop opens
// http://127.0.0.1:<port>/admin over a port-forward. A dispatch that required
// host == th answered "No page is configured at this address" for every local
// surface — dashboard, sessions, built-in chat — from the moment the feature
// started working.
//
// The control below is the constraint on the fix: a kind that does not allow a
// shared origin has a nil predicate, and the same loopback request must still
// fall through to the distinct-origin switch and 404. Nothing about the remote
// case may move.
func TestServer_SharedOrigin_LoopbackIsStillServedOnceTheTunnelIsUp(t *testing.T) {
	routes := []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/admin", Handler: okHandler("T"), Auth: webui.AuthNone},
		{Origin: webui.OriginSandbox, Pattern: "/content", Handler: okHandler("S"), Auth: webui.AuthNone},
	}
	const tunnel = "demo-tunnel.ngrok-free.app"

	shared, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "", false }, nil,
		hostGetter(tunnel), hostGetter(tunnel),
		desktopSharedOriginOK, nil, []webui.WebUI{testUI{name: "d", routes: routes}})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	shared.ServeHTTP(rec, req("127.0.0.1", http.MethodGet, "/admin"))
	assert.Equal(t, "T", rec.Body.String(),
		"the desktop opens its own console over a port-forward; the tunnel taking the advertised host must not unmount it")

	rec = httptest.NewRecorder()
	shared.ServeHTTP(rec, req("localhost", http.MethodGet, "/content"))
	assert.Equal(t, "S", rec.Body.String(),
		"the same host serves both route sets on this kind, which is what the artifact viewer needs locally")

	rec = httptest.NewRecorder()
	shared.ServeHTTP(rec, req(tunnel, http.MethodGet, "/admin"))
	assert.Equal(t, "T", rec.Body.String(), "and the advertised host still serves what it always did")

	// The control: no shared origin allowed ⇒ nil predicate ⇒ a loopback
	// request is nobody's origin and stays a 404, exactly as before.
	distinct, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "", false }, nil,
		hostGetter("trusted.demo.test"), hostGetter("sandbox.demo.test"),
		nil, nil, []webui.WebUI{testUI{name: "d", routes: routes}})
	require.NoError(t, err)

	rec = httptest.NewRecorder()
	distinct.ServeHTTP(rec, req("127.0.0.1", http.MethodGet, "/admin"))
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a kind with real ingress serves nothing on a loopback Host header")
}

func TestServer_PartialConfig_UnconfiguredSideUnreachable(t *testing.T) {
	ui := testUI{name: "d", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/t", Handler: okHandler("T"), Auth: webui.AuthNone},
		{Origin: webui.OriginSandbox, Pattern: "/s", Handler: okHandler("S"), Auth: webui.AuthNone},
	}}
	s, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "", false }, nil,
		hostGetter("trusted.example"), hostGetter(""),
		nil, nil, []webui.WebUI{ui})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/t"))
	assert.Equal(t, "T", rec.Body.String(), "trusted route served while sandbox host is unconfigured")

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/s"))
	assert.Equal(t, http.StatusNotFound, rec.Code, "sandbox route not served on the trusted host (unconfigured side unreachable)")
}

func TestServer_OriginDispatch(t *testing.T) {
	ui := testUI{name: "t", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/t", Handler: okHandler("trusted-ok"), Auth: webui.AuthNone},
		{Origin: webui.OriginSandbox, Pattern: "/s", Handler: okHandler("sandbox-ok"), Auth: webui.AuthNone},
	}}
	s := newServer(t, nil, ui)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/t"))
	assert.Equal(t, "trusted-ok", rec.Body.String())

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req("sandbox.example", http.MethodGet, "/s"))
	assert.Equal(t, "sandbox-ok", rec.Body.String())

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req("sandbox.example", http.MethodGet, "/t"))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req("evil.example", http.MethodGet, "/t"))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_MethodGuard(t *testing.T) {
	ui := testUI{name: "t", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/g", Methods: []string{http.MethodGet}, Handler: okHandler("ok"), Auth: webui.AuthNone},
	}}
	s := newServer(t, nil, ui)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodPost, "/g"))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "GET", rec.Header().Get("Allow"), "405 must advertise the allowed methods")
}

func TestServer_PublicRouteMustBeGetOnly_Panics(t *testing.T) {
	ui := testUI{name: "bad", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/x", Methods: []string{http.MethodPost}, Handler: okHandler("x"), Auth: webui.AuthNone},
	}}
	_, err := webui.NewServer(nil, nil, hostGetter("trusted.example"), hostGetter("sandbox.example"), nil, nil, []webui.WebUI{ui})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GET/HEAD")
}

func TestServer_AuthorizedWithoutAuthorizeFunc_Errors(t *testing.T) {
	ui := testUI{name: "t", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/z", Handler: okHandler("z"), Auth: webui.AuthAuthorized},
	}}
	_, err := webui.NewServer(nil, nil, hostGetter("trusted.example"), hostGetter("sandbox.example"), nil, nil, []webui.WebUI{ui})
	require.Error(t, err, "AuthAuthorized route with no Authorize func must be rejected at NewServer")
	assert.Contains(t, err.Error(), "Authorize")
}

func TestServer_Authorized_DeniesWith403(t *testing.T) {
	authenticate := func(r *http.Request) (string, bool) { return "user:abc", true }
	ui := testUI{name: "t", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/z", Handler: okHandler("authz-ok"), Auth: webui.AuthAuthorized,
			Authorize: func(ctx context.Context, subject string, r *http.Request) error { return context.Canceled }},
	}}
	s := newServer(t, authenticate, ui)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/z"))
	assert.Equal(t, http.StatusForbidden, rec.Code, "Authorize error must produce 403")
	assert.NotEqual(t, "authz-ok", rec.Body.String(), "a denied request must never reach the handler")
}

func TestServer_NilHostGettersRejected(t *testing.T) {
	_, err := webui.NewServer(nil, nil, nil, hostGetter("sandbox.example"), nil, nil, nil)
	require.Error(t, err, "a nil trustedHost getter must be rejected")

	_, err = webui.NewServer(nil, nil, hostGetter("trusted.example"), nil, nil, nil, nil)
	require.Error(t, err, "a nil sandboxHost getter must be rejected")
}

func TestServer_Authenticated_RequiresCookie(t *testing.T) {
	authenticate := func(r *http.Request) (string, bool) {
		if s := r.Header.Get("X-Test-Subject"); s != "" {
			return s, true
		}
		return "", false
	}
	ui := testUI{name: "t", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/a", Handler: okHandler("auth-ok"), Auth: webui.AuthAuthenticated},
	}}
	s := newServer(t, authenticate, ui)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/a"))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = httptest.NewRecorder()
	r := req("trusted.example", http.MethodGet, "/a")
	r.Header.Set("X-Test-Subject", "user:abc")
	s.ServeHTTP(rec, r)
	assert.Equal(t, "auth-ok", rec.Body.String())
}

func TestServer_HealthzAnyHost(t *testing.T) {
	s := newServer(t, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("anything.example", http.MethodGet, "/healthz"))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

func TestServer_Authorized_RunsAuthorize(t *testing.T) {
	authenticate := func(r *http.Request) (string, bool) { return "user:abc", true }
	ui := testUI{name: "t", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/z", Handler: okHandler("authz-ok"), Auth: webui.AuthAuthorized,
			Authorize: func(ctx context.Context, subject string, r *http.Request) error {
				if subject == "user:abc" {
					return nil
				}
				return context.Canceled
			}},
	}}
	s := newServer(t, authenticate, ui)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/z"))
	assert.Equal(t, "authz-ok", rec.Body.String())
}

func TestServer_UnknownAuthLevel_Rejected(t *testing.T) {
	ui := testUI{name: "t", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/u", Handler: okHandler("u"), Auth: webui.AuthLevel(99)},
	}}
	_, err := webui.NewServer(nil, nil, hostGetter("trusted.example"), hostGetter("sandbox.example"), nil, nil, []webui.WebUI{ui})
	require.Error(t, err, "an unknown AuthLevel must be rejected at NewServer (fail-closed)")
	assert.Contains(t, err.Error(), "AuthLevel")
}

func newServerWithLogin(t *testing.T, authenticate, beginLogin func(*http.Request) (string, bool), uis ...webui.WebUI) *webui.Server {
	t.Helper()
	s, err := webui.NewServer(authenticate, beginLogin, hostGetter("trusted.example"), hostGetter("sandbox.example"), nil, nil, uis)
	require.NoError(t, err)
	return s
}

func TestAuthLoginIfNecessary_NoCookie_RedirectsToLoginWithNext(t *testing.T) {
	authenticate := func(r *http.Request) (string, bool) { return "", false }
	beginLogin := func(r *http.Request) (string, bool) {
		return "https://trusted.example/oidc/login?next=" + url.QueryEscape(r.URL.String()), true
	}
	ui := testUI{name: "p", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/p", Auth: webui.AuthLoginIfNecessary, Handler: okHandler("page")},
	}}
	s := newServerWithLogin(t, authenticate, beginLogin, ui)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/p?x=1"))
	assert.Equal(t, http.StatusFound, rec.Code, "no cookie → 302 to login")
	assert.Contains(t, rec.Header().Get("Location"), "/oidc/login?next=", "carries next")
}

func TestAuthLoginIfNecessary_WithCookie_ServesAndInjectsSubject(t *testing.T) {
	authenticate := func(r *http.Request) (string, bool) { return "user:abc", true }
	ui := testUI{name: "p", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/p", Auth: webui.AuthLoginIfNecessary,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(webui.SubjectFromContext(r.Context())))
			})},
	}}
	s := newServerWithLogin(t, authenticate, nil, ui)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/p"))
	assert.Equal(t, "user:abc", rec.Body.String(), "cookie present → serve + inject subject")
}

func TestAuthLoginIfNecessary_NoLoginProvider_FailsClosed401(t *testing.T) {
	authenticate := func(r *http.Request) (string, bool) { return "", false }
	ui := testUI{name: "p", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/p", Auth: webui.AuthLoginIfNecessary, Handler: okHandler("x")},
	}}
	s := newServerWithLogin(t, authenticate, nil, ui) // beginLogin nil
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req("trusted.example", http.MethodGet, "/p"))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no login provider → 401 fail-closed")
}

func TestAuthLoginIfNecessary_PostRejectedAtMount(t *testing.T) {
	ui := testUI{name: "p", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/p", Methods: []string{http.MethodPost},
			Auth: webui.AuthLoginIfNecessary, Handler: okHandler("x")},
	}}
	_, err := webui.NewServer(func(*http.Request) (string, bool) { return "", false }, nil,
		hostGetter("trusted.example"), hostGetter("sandbox.example"), nil, nil, []webui.WebUI{ui})
	require.Error(t, err, "AuthLoginIfNecessary must be GET/HEAD-only, rejected at mount")
}

func TestServer_InjectsSubjectIntoContext(t *testing.T) {
	authenticate := func(r *http.Request) (string, bool) { return "user:abc", true }
	var seen string
	ui := testUI{name: "subj", routes: []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/s", Auth: webui.AuthAuthenticated,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = webui.SubjectFromContext(r.Context()) })},
	}}
	s := newServer(t, authenticate, ui)
	s.ServeHTTP(httptest.NewRecorder(), req("trusted.example", http.MethodGet, "/s"))
	assert.Equal(t, "user:abc", seen, "handler must read the authenticated subject from context")
}
