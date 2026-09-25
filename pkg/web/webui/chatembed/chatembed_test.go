package chatembed_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/chatembed"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
	"github.com/authzed/openagentprimitives/pkg/web/webui/webassets"
)

const trustedHost = "trusted.example"
const sandboxHost = "sandbox.example"

// fakeDeps implements chatembed.Deps via a scripted CheckInteract, mirroring
// sessionview_test.go's fakeDeps.
type fakeDeps struct {
	checkInteract func(ctx context.Context, ns, name, subject string) (bool, error)
	logger        logr.Logger
}

func (f *fakeDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	return f.checkInteract(ctx, ns, name, subject)
}
func (f *fakeDeps) Logger() logr.Logger { return f.logger }

// fakeCE returns a fakeDeps whose happy-path CheckInteract lets the page
// build succeed. Each test overrides checkInteract to exercise the other arms.
func fakeCE() *fakeDeps {
	return &fakeDeps{
		checkInteract: func(ctx context.Context, ns, name, subject string) (bool, error) { return true, nil },
		logger:        logr.Discard(),
	}
}

// newServer wires the chatembed WebUI into a real webui.Server, then seeds a
// manifest entry for "chat-embed" — the app key isn't in the committed
// pkg/web/webui/webassets/dist/manifest.json until `mage web:build` runs after
// this task lands, and SetManifestEntryForTest exists exactly so this page can
// be exercised end-to-end through the real Server.ServeHTTP before that.
func newServer(t *testing.T, subject string, d chatembed.Deps) *webui.Server {
	t.Helper()
	authenticate := func(r *http.Request) (string, bool) { return subject, true }
	s, err := webui.NewServer(authenticate, nil,
		func() string { return trustedHost }, func() string { return sandboxHost },
		nil, d, []webui.WebUI{chatembed.New()})
	require.NoError(t, err)
	s.SetManifestEntryForTest("chat-embed", webassets.Entry{Scripts: []string{"/assets/chat-embed.js"}})
	return s
}

func req(host, path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	r.Host = host
	return r
}

// TestPage_InteractGate proves the three facts about the page's authorization:
// it authorizes the viewer on interact (403 otherwise, 500 on an
// indeterminate check — never a grant), and on the happy path its props are
// exactly the session named in the path.
func TestPage_InteractGate(t *testing.T) {
	cases := []struct {
		name          string
		checkInteract func(ctx context.Context, ns, name, subject string) (bool, error)
		wantStatus    int
	}{
		{
			name:          "CheckInteract allows: 200 with the path session as props",
			checkInteract: func(ctx context.Context, ns, name, subject string) (bool, error) { return true, nil },
			wantStatus:    http.StatusOK,
		},
		{
			name:          "CheckInteract denies: 403, never a grant",
			checkInteract: func(ctx context.Context, ns, name, subject string) (bool, error) { return false, nil },
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "CheckInteract errors: 500, never a grant",
			checkInteract: func(ctx context.Context, ns, name, subject string) (bool, error) { return false, assert.AnError },
			wantStatus:    http.StatusInternalServerError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := fakeCE()
			var gotNs, gotName, gotSubject string
			d.checkInteract = func(ctx context.Context, ns, name, subject string) (bool, error) {
				gotNs, gotName, gotSubject = ns, name, subject
				return tc.checkInteract(ctx, ns, name, subject)
			}
			s := newServer(t, "user:abc", d)

			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req(trustedHost, "/chat-embed/ws-abc123456789/demo-haiku-1a2b3c4d"))

			require.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, "ws-abc123456789", gotNs, "CheckInteract must run with the path namespace")
			assert.Equal(t, "demo-haiku-1a2b3c4d", gotName, "CheckInteract must run with the path session name")
			assert.Equal(t, "user:abc", gotSubject, "CheckInteract must run with the authenticated subject")

			body := rec.Body.String()
			switch tc.wantStatus {
			case http.StatusOK:
				assert.Contains(t, body, `data-app="chat-embed"`, "the happy path must mount the chat-embed React app")
				assert.Contains(t, body, `"ns":"ws-abc123456789"`, "bootstrap props must carry ns")
				assert.Contains(t, body, `"name":"demo-haiku-1a2b3c4d"`, "bootstrap props must carry name")
			default:
				assert.Contains(t, body, `data-app="system"`, "a denial or error renders the styled system page")
			}
		})
	}
}

// TestPage_EmbeddableSameOriginOnly proves the route relaxes anti-framing
// headers to same-origin only (X-Frame-Options: SAMEORIGIN, CSP
// frame-ancestors 'self') — never wide open, and never fully blocked, since
// ap:chat must be able to frame it same-origin.
func TestPage_EmbeddableSameOriginOnly(t *testing.T) {
	d := fakeCE()
	s := newServer(t, "user:abc", d)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/chat-embed/ws-abc123456789/demo-haiku-1a2b3c4d"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "SAMEORIGIN", rec.Header().Get("X-Frame-Options"))
	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "frame-ancestors 'self'", "the page opts into same-origin framing, but stays refused cross-origin")

	// Compile-time wiring pin, mirroring sessionview's
	// TestRoutes_SessionViewPage_EmbeddableSameOrigin: the route itself must
	// carry EmbeddableSameOrigin, and must NOT set FramesSandbox — this page
	// frames nothing of its own.
	ui := chatembed.New()
	routes := ui.Routes(d)
	require.Len(t, routes, 1)
	require.NotNil(t, routes[0].Page)
	assert.True(t, routes[0].Page.EmbeddableSameOrigin, "chat-embed opts into same-origin framing")
	assert.False(t, routes[0].Page.FramesSandbox, "chat-embed frames no sandbox-origin content of its own")
}

// TestRoutes_DepsCastFails_NoRoutes proves the fail-closed contract: an
// unrelated or nil deps value yields no routes (never a panic) — the same
// property pkg/web/webui/manifest_contract_test.go relies on for every
// registered WebUI's Routes(nil).
func TestRoutes_DepsCastFails_NoRoutes(t *testing.T) {
	ui := chatembed.New()
	assert.Nil(t, ui.Routes(nil))
}

// TestPlugin_RegisteredAs proves the WebUI self-registers via init() once the
// package is imported, mirroring health_test.go's own registration test.
func TestPlugin_RegisteredAs(t *testing.T) {
	names := map[string]bool{}
	for _, u := range registry.All() {
		names[u.Name()] = true
	}
	assert.True(t, names["chat-embed"], "chat-embed UI must self-register via init()")
}
