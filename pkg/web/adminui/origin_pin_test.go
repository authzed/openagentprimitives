package adminui_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// The admin console's mutating routes kill a session, install an agent from a
// registry, and submit channel credentials — all cookie-authenticated POSTs.
// None of them pinned the Origin header, and the framework ships that pin
// (webui.TrustedOriginMatch) with seven other surfaces using it.
//
// Defense in depth rather than the whole story: the shipped mcp-ui client
// sandboxes inline widgets to an opaque origin and artifact HTML is inert under
// default-src 'none', so reaching these routes needs an external same-site
// scripting flaw. It is still the convention this codebase follows everywhere
// else, and following it costs one wrapper.

const adminTrustedOrigin = "https://ap.example"

func adminDeps(t *testing.T) stubDeps {
	t.Helper()
	return stubDeps{
		baseURL: "http://admind.test", token: "tok",
		allow:         map[string]bool{"view_overview": true, "view_sessions": true, "kill_session": true, "view_audit": true},
		logger:        testr.New(t),
		trustedOrigin: adminTrustedOrigin,
	}
}

func serveAdmin(t *testing.T, rt webui.Route, method, path, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	require.NotNil(t, rt.Handler, "route %s must be a raw handler", path)
	rt.Handler.ServeHTTP(rec, req)
	return rec
}

func TestAdminAPI_MutatingRouteWithoutTheTrustedOriginIsRefused(t *testing.T) {
	routes := routesOf(t, adminDeps(t))
	rt, ok := routes["/admin/api/sessions/"]
	require.True(t, ok)

	rec := serveAdmin(t, rt, http.MethodDelete, "/admin/api/sessions/ns1/sess1", "https://evil.example")

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"killing a session from another origin must be refused before it reaches admind")
}

func TestAdminAPI_MutatingRouteWithNoOriginHeaderIsRefused(t *testing.T) {
	routes := routesOf(t, adminDeps(t))
	rt, ok := routes["/admin/api/agents/oap-install"]
	require.True(t, ok)

	rec := serveAdmin(t, rt, http.MethodPost, "/admin/api/agents/oap-install", "")

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an absent Origin is not a match; TrustedOriginMatch fails closed")
}

// The counterweight: a GET is a read, and a top-level navigation sends no
// Origin at all — pinning those would refuse the SPA itself. A GET must reach
// the proxy, which then fails with a 502 against the unreachable stub backend.
// That 502 is the proof it got past the pin.
func TestAdminAPI_ReadRouteIsNotPinned(t *testing.T) {
	routes := routesOf(t, adminDeps(t))
	rt, ok := routes["/admin/api/overview"]
	require.True(t, ok)

	rec := serveAdmin(t, rt, http.MethodGet, "/admin/api/overview", "")

	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"a read must not be pinned; got %d", rec.Code)
}

func TestAdminAPI_MutatingRouteWithTheTrustedOriginPasses(t *testing.T) {
	routes := routesOf(t, adminDeps(t))
	rt, ok := routes["/admin/api/sessions/"]
	require.True(t, ok)

	rec := serveAdmin(t, rt, http.MethodDelete, "/admin/api/sessions/ns1/sess1", adminTrustedOrigin)

	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"the console's own origin must still be able to kill a session; got %d", rec.Code)
}
