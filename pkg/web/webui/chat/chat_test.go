package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// gateTestDeps is a fully-controllable chat.Deps for exercising Routes'
// fail-closed gate: every prerequisite (NATS, Authz, OperatorURL) is
// independently toggleable.
//
// It carries NO cluster-shape marker, deliberately. ensureRegistry's three
// prerequisites are the whole gate now, and a fixture that had to set some
// "this deployment may serve chat" flag to reach the mounted case would let a
// re-introduced deployment-shaped gate keep every row below green.
type gateTestDeps struct {
	fakeDeps
	nats        *nats.Conn
	authz       pipeline.Authz
	operatorURL string
}

func (d *gateTestDeps) NATS() *nats.Conn      { return d.nats }
func (d *gateTestDeps) Authz() pipeline.Authz { return d.authz }
func (d *gateTestDeps) OperatorURL() string   { return d.operatorURL }

// fullyConfiguredDeps returns a gateTestDeps that passes every gate.
func fullyConfiguredDeps(t *testing.T) *gateTestDeps {
	t.Helper()
	srv := natstest.RunServer(&server.Options{Port: -1})
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return &gateTestDeps{
		fakeDeps:    fakeDeps{k8s: newFakeK8sClient(t)},
		nats:        nc,
		authz:       (*fakeAuthzStub)(nil),
		operatorURL: "http://operator.example:8082",
	}
}

// captureLogs points d's logger at a slice and returns a reader for it. The
// three prerequisite rows below assert the LOG as well as the empty route set:
// ensureRegistry is the only gate, so which prerequisite is missing is the
// only diagnosis an operator gets, and a refusal that logged nothing would be
// indistinguishable from a plugin that was never registered.
func captureLogs(t *testing.T, d *gateTestDeps) func() string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	d.fakeDeps.logger = funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{})
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

// fakeAuthzStub is a cheap placeholder for the gate tests, which only assert
// that Authz() returns a non-nil pipeline.Authz — they never call any of its
// ~12 methods. Embedding the interface promotes every method without
// implementing any of them; (*fakeAuthzStub)(nil) is a nil POINTER of a
// concrete type, which — stored in the pipeline.Authz interface field below —
// produces a genuinely non-nil interface value (type=*fakeAuthzStub,
// value=nil). That is deliberate here (see AGENTS.md's nil-interface
// section for why this same shape is a bug when it happens by ACCIDENT in
// production wiring): it is exactly what "Authz is configured" should look
// like to ensureRegistry's `d.Authz() == nil` check, without hand-writing a
// dozen no-op methods just to satisfy the interface for a gate test.
type fakeAuthzStub struct{ pipeline.Authz }

func TestRoutes_NilWhenDepsDontImplementChatDeps(t *testing.T) {
	t.Cleanup(ResetForTest)
	var other webui.Deps = struct{}{}
	assert.Nil(t, ui{}.Routes(other))
}

// TestRoutes_UnmetPrerequisite_NoRoutesAndSaysWhich covers the whole gate.
// Each row asserts BOTH halves — no routes, and a log naming the prerequisite
// that was missing. The log half is the discriminating one: an early return
// that mounts nothing and says nothing leaves an operator with a 404 and no
// way to tell "misconfigured" from "not installed".
func TestRoutes_UnmetPrerequisite_NoRoutesAndSaysWhich(t *testing.T) {
	cases := []struct {
		name       string
		break_     func(d *gateTestDeps)
		wantLogged string
	}{
		{
			name:       "NATS unconfigured: no routes, and the log names NATS",
			break_:     func(d *gateTestDeps) { d.nats = nil },
			wantLogged: "NATS is not configured",
		},
		{
			name:       "SpiceDB unconfigured: no routes, and the log names authorization",
			break_:     func(d *gateTestDeps) { d.authz = nil },
			wantLogged: "SpiceDB (Authz) is not configured",
		},
		{
			name:       "operator URL unconfigured: no routes, and the log names it",
			break_:     func(d *gateTestDeps) { d.operatorURL = "" },
			wantLogged: "operator memory URL is not configured",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(ResetForTest)
			d := fullyConfiguredDeps(t)
			logged := captureLogs(t, d)
			tc.break_(d)

			assert.Nil(t, ui{}.Routes(d), "an unmet prerequisite must mount nothing")
			assert.Contains(t, logged(), tc.wantLogged,
				"the refusal must name which prerequisite is missing — it is the only diagnosis an operator gets")
		})
	}
}

// TestCanHostBrowserSessions runs the SAME four fixtures the gate rows above
// use, because internal/cmd/webd decides whether to hand the shell a start function at
// all from this answer. A start control offered by a webd that cannot host a
// browser session produces a session whose every reply is silently dropped.
func TestCanHostBrowserSessions(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(d *gateTestDeps)
		want   bool
	}{
		{name: "every prerequisite met: true", want: true},
		{name: "NATS unconfigured: false", break_: func(d *gateTestDeps) { d.nats = nil }},
		{name: "SpiceDB unconfigured: false", break_: func(d *gateTestDeps) { d.authz = nil }},
		{name: "operator URL unconfigured: false", break_: func(d *gateTestDeps) { d.operatorURL = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(ResetForTest)
			d := fullyConfiguredDeps(t)
			if tc.break_ != nil {
				tc.break_(d)
			}
			assert.Equal(t, tc.want, CanHostBrowserSessions(d))
		})
	}
}

// TestRoutes_MountsAllRoutesWhenFullyConfigured asserts the route
// pattern set BY EXACT STRING, so a path typo fails here rather than as a
// 404 in the browser.
//
// It is also the flip's own assertion: the fixture sets no cluster-shape
// marker of any kind, so the six routes mount on the strength of the three
// prerequisites alone. Re-introducing a deployment-shaped early return in
// Routes fails HERE, with zero routes.
//
// It also asserts every session-scoped route carries a
// non-nil Authorize — webui.Server.mount panics on AuthAuthorized with a nil
// one, and a panic at webd startup is a worse discovery point than a
// failing unit test here (this test never calls webui.NewServer; it
// inspects the Route slice Routes() returns directly, so a missing
// Authorize surfaces as an assertion failure, never a panic).
func TestRoutes_MountsAllRoutesWhenFullyConfigured(t *testing.T) {
	t.Cleanup(ResetForTest)
	d := fullyConfiguredDeps(t)
	routes := ui{}.Routes(d)
	require.Len(t, routes, 6)

	patterns := map[string]webui.Route{}
	for _, r := range routes {
		patterns[r.Pattern] = r
	}
	sessionScoped := []string{
		"/sessions/api/{ns}/{name}/detail",
		"/sessions/api/{ns}/{name}/messages",
		"/sessions/api/{ns}/{name}/message",
		"/sessions/api/{ns}/{name}/interrupt",
		"/sessions/api/{ns}/{name}/decision",
		"/sessions/api/{ns}/{name}/ws",
	}
	for _, p := range sessionScoped {
		require.Contains(t, patterns, p)
		assert.Equal(t, webui.AuthAuthorized, patterns[p].Auth, "route %q must be AuthAuthorized", p)
		assert.NotNil(t, patterns[p].Authorize, "route %q must carry a non-nil Authorize", p)
	}
	for _, r := range routes {
		assert.Equal(t, webui.OriginTrusted, r.Origin, "every chat route is trusted-origin only")
	}
}

// TestRoutes_AuthorizeClosure_DenyErrorAllow is I4's regression:
// TestRoutes_MountsAllRoutesWhenFullyConfigured only asserts Authorize is
// non-nil, never what it actually returns. Two real mutations left the whole
// package green before this test existed: returning a bare error instead of
// *webui.PageError{503} on a CheckInteract failure — which
// webui.Server.renderAuthorizeFailure (pkg/web/webui/server.go) collapses to a
// generic 403, silently converting a SpiceDB outage into a denial at the
// outermost gate — and making the closure unconditionally return nil (a
// no-op gate nothing else would catch).
func TestRoutes_AuthorizeClosure_DenyErrorAllow(t *testing.T) {
	t.Cleanup(ResetForTest)
	d := fullyConfiguredDeps(t)
	routes := ui{}.Routes(d)
	require.NotEmpty(t, routes)

	cases := []struct {
		name          string
		checkInteract func(ctx context.Context, ns, name, subject string) (bool, error)
		wantErr       bool
		wantStatus    int
	}{
		{
			name:          "CheckInteract admits: nil error",
			checkInteract: func(context.Context, string, string, string) (bool, error) { return true, nil },
		},
		{
			name:          "CheckInteract denies: *webui.PageError{403}",
			checkInteract: func(context.Context, string, string, string) (bool, error) { return false, nil },
			wantErr:       true,
			wantStatus:    http.StatusForbidden,
		},
		{
			name: "CheckInteract errors: *webui.PageError{503}, never 403",
			checkInteract: func(context.Context, string, string, string) (bool, error) {
				return false, errors.New("spicedb: dial tcp: connection refused")
			},
			wantErr:    true,
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/api/demo-ns/shared-name/messages", nil)
	req.SetPathValue("ns", "demo-ns")
	req.SetPathValue("name", "shared-name")

	for _, rt := range routes {
		if rt.Auth != webui.AuthAuthorized {
			continue
		}
		for _, tc := range cases {
			t.Run(rt.Pattern+"/"+tc.name, func(t *testing.T) {
				d.checkInteract = tc.checkInteract
				err := rt.Authorize(context.Background(), "user:owner", req)
				if !tc.wantErr {
					assert.NoError(t, err)
					return
				}
				require.Error(t, err)
				var pe *webui.PageError
				require.True(t, errors.As(err, &pe),
					"Authorize must return a *webui.PageError, not a bare error — a bare error is collapsed to 403 by renderAuthorizeFailure regardless of the real cause")
				assert.Equal(t, tc.wantStatus, pe.Status)
			})
		}
	}
}

// TestRoutes_AuthorizeClosure_ConsultedForTheURLsObject is New-1's
// regression: TestRoutes_AuthorizeClosure_DenyErrorAllow scripts
// CheckInteract by a FIXED return value, ignoring the ns/name it was asked
// about — so nothing proves the route-level gate, the OUTERMOST one and the
// one that fires FIRST in production (a request never reaches the handler's
// own authorize call if this one denies), is actually asked about the URL's
// own session rather than some other one. This is the same class I2 closed
// at Registry.checkInteract, but the route closure makes its own, separate
// d.CheckInteract call and was never covered there. Two same-named sessions
// across two namespaces, admitted in only one, is the only fixture that
// catches a gate that answers from the wrong object.
func TestRoutes_AuthorizeClosure_ConsultedForTheURLsObject(t *testing.T) {
	t.Cleanup(ResetForTest)
	d := fullyConfiguredDeps(t)
	d.checkInteract = checkInteractOnlyForObject("demo-ns", "shared-name")
	routes := ui{}.Routes(d)
	require.NotEmpty(t, routes)

	admitted := httptest.NewRequest(http.MethodGet, "/sessions/api/demo-ns/shared-name/messages", nil)
	admitted.SetPathValue("ns", "demo-ns")
	admitted.SetPathValue("name", "shared-name")
	denied := httptest.NewRequest(http.MethodGet, "/sessions/api/other-ns/shared-name/messages", nil)
	denied.SetPathValue("ns", "other-ns")
	denied.SetPathValue("name", "shared-name")

	for _, rt := range routes {
		if rt.Auth != webui.AuthAuthorized {
			continue
		}
		t.Run(rt.Pattern, func(t *testing.T) {
			assert.NoError(t, rt.Authorize(context.Background(), "user:owner", admitted),
				"CheckInteract was scripted to admit demo-ns/shared-name")

			err := rt.Authorize(context.Background(), "user:owner", denied)
			require.Error(t, err, "CheckInteract was scripted to deny other-ns/shared-name — same name, different namespace")
			var pe *webui.PageError
			require.True(t, errors.As(err, &pe))
			assert.Equal(t, http.StatusForbidden, pe.Status)
		})
	}
}

func TestRoutes_ReusesSingletonRegistryAcrossCalls(t *testing.T) {
	t.Cleanup(ResetForTest)
	d := fullyConfiguredDeps(t)
	require.NotNil(t, ui{}.Routes(d))
	first := regSingleton
	require.NotNil(t, ui{}.Routes(d))
	assert.Same(t, first, regSingleton, "a second Routes() call must reuse the same Registry, not rebuild one")
}

func TestShutdown_NilSafeWhenNeverEnabled(t *testing.T) {
	t.Cleanup(ResetForTest)
	assert.NotPanics(t, func() { Shutdown(context.Background()) })
}

func TestShutdown_TearsDownTheSingleton(t *testing.T) {
	t.Cleanup(ResetForTest)
	d := fullyConfiguredDeps(t)
	require.NotNil(t, ui{}.Routes(d))
	require.NotNil(t, regSingleton)

	Shutdown(context.Background())
	assert.Nil(t, regSingleton, "Shutdown must clear the singleton so a later Routes() call rebuilds it")
}
