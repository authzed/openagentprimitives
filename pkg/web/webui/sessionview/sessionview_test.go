package sessionview_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/sessionview"
)

const trustedHost = "trusted.example"
const sandboxHost = "sandbox.example"

// fakeDeps implements sessionview.Deps via per-behavior func fields. Only
// CheckInteract + the widget-bootstrap methods are exercised by the shell
// page — the rest are the fields the live handler needs (see live_test.go
// for those tests) and stay at their zero values here.
type fakeDeps struct {
	checkInteract   func(ctx context.Context, ns, name, subject string) (bool, error)
	operatorURL     string
	memoryToken     string
	nc              *nats.Conn
	trustedOrigin   string
	sandboxBaseURL  string
	activeWidgets   func(ctx context.Context, ns, sess string) ([]sessionview.WidgetRef, error)
	signWidgetToken func(ns, sess, artifactID string) (string, error)
	logger          logr.Logger
}

func (f *fakeDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	return f.checkInteract(ctx, ns, name, subject)
}
func (f *fakeDeps) OperatorURL() string   { return f.operatorURL }
func (f *fakeDeps) MemoryToken() string   { return f.memoryToken }
func (f *fakeDeps) NATS() *nats.Conn      { return f.nc }
func (f *fakeDeps) TrustedOrigin() string { return f.trustedOrigin }
func (f *fakeDeps) Logger() logr.Logger   { return f.logger }

func (f *fakeDeps) SandboxBaseURL() string { return f.sandboxBaseURL }

// ActiveWidgets defaults to "no widgets" (nil, nil) — the happy-path default
// for shell tests that don't exercise widget bootstrapping.
func (f *fakeDeps) ActiveWidgets(ctx context.Context, ns, sess string) ([]sessionview.WidgetRef, error) {
	if f.activeWidgets == nil {
		return nil, nil
	}
	return f.activeWidgets(ctx, ns, sess)
}
func (f *fakeDeps) SignWidgetToken(ns, sess, artifactID string) (string, error) {
	if f.signWidgetToken == nil {
		return "tok-" + artifactID, nil
	}
	return f.signWidgetToken(ns, sess, artifactID)
}
func (f *fakeDeps) VerifyWidgetToken(token string) (string, string, string, error) {
	return "", "", "", assert.AnError
}
func (f *fakeDeps) FetchWidget(ctx context.Context, ns, sess, artifactID string) ([]byte, sessionview.WidgetMeta, error) {
	return nil, sessionview.WidgetMeta{}, assert.AnError
}

// fakeSV returns a fully-populated fakeDeps whose happy-path CheckInteract
// lets the shell flow succeed. Each test overrides only what it exercises.
func fakeSV() *fakeDeps {
	return &fakeDeps{
		checkInteract:  func(ctx context.Context, ns, name, subject string) (bool, error) { return true, nil },
		trustedOrigin:  "https://" + trustedHost,
		sandboxBaseURL: "https://" + sandboxHost,
		logger:         logr.Discard(),
	}
}

// newServer wires the sessionview WebUI into a real webui.Server whose
// authenticate func always authenticates the given subject, mirroring
// artifactview_test.go's newServer helper.
func newServer(t *testing.T, subject string, d sessionview.Deps) *webui.Server {
	t.Helper()
	authenticate := func(r *http.Request) (string, bool) { return subject, true }
	s, err := webui.NewServer(authenticate, nil,
		func() string { return trustedHost }, func() string { return sandboxHost },
		nil, d, []webui.WebUI{sessionview.New()})
	require.NoError(t, err)
	return s
}

func req(host, path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	r.Host = host
	return r
}

func TestShell_CheckInteractFalse_Forbidden(t *testing.T) {
	d := fakeSV()
	d.checkInteract = func(ctx context.Context, ns, name, subject string) (bool, error) { return false, nil }
	s := newServer(t, "user:abc", d)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/session-view/ns1/sess1"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `data-app="system"`, "a denial renders the styled system page")
}

func TestShell_CheckInteractError_InternalServerError(t *testing.T) {
	d := fakeSV()
	d.checkInteract = func(ctx context.Context, ns, name, subject string) (bool, error) {
		return false, assert.AnError
	}
	s := newServer(t, "user:abc", d)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/session-view/ns1/sess1"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), `data-app="system"`, "an authz error renders the styled system page")
}

func TestShell_HappyPath_RendersSessionViewAppWithNsNameProps(t *testing.T) {
	d := fakeSV()
	var gotNs, gotName, gotSubject string
	d.checkInteract = func(ctx context.Context, ns, name, subject string) (bool, error) {
		gotNs, gotName, gotSubject = ns, name, subject
		return true, nil
	}
	s := newServer(t, "user:abc", d)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/session-view/ns1/sess1"))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="session-view"`, "shell must mount the session-view React app")
	assert.Contains(t, body, `"ns":"ns1"`, "bootstrap props must carry ns")
	assert.Contains(t, body, `"name":"sess1"`, "bootstrap props must carry name")
	assert.Contains(t, body, "<title>Session view — ns1/sess1</title>")
	assert.Equal(t, "ns1", gotNs, "CheckInteract must run with the path namespace")
	assert.Equal(t, "sess1", gotName, "CheckInteract must run with the path session name")
	assert.Equal(t, "user:abc", gotSubject, "CheckInteract must run with the authenticated subject")
}

// TestRoutes_DepsCastFails_NoRoutes proves the fail-closed contract: an
// unrelated or nil deps value yields no routes (never a panic). This is also
// exactly how pkg/web/webui's TestRegisteredPageAppsResolveInManifest calls every
// registered WebUI's Routes(nil).
func TestRoutes_DepsCastFails_NoRoutes(t *testing.T) {
	ui := sessionview.New()
	assert.Nil(t, ui.Routes(nil))
}

// TestRoutes_SessionViewPage_EmbeddableSameOrigin proves the shell route's
// EmbeddableSameOrigin flag is wired (Task 1 of the ap:session_view plan): the
// session shell's ap:session_view component embeds this page same-origin, so
// its anti-framing headers must relax to permit that. buildCSP/renderDocument
// turn this flag into the actual CSP frame-ancestors + X-Frame-Options
// response headers (see document_test.go); this pins the compile-time wiring
// that feeds them. FramesSandbox must stay set too — session-view still
// frames its own sandbox-origin /mcpui-host content.
func TestRoutes_SessionViewPage_EmbeddableSameOrigin(t *testing.T) {
	d := fakeSV()
	ui := sessionview.New()
	routes := ui.Routes(d)

	var sessionViewPage *webui.Page
	for _, rt := range routes {
		if rt.Pattern == "/session-view/{ns}/{name}" {
			sessionViewPage = rt.Page
		}
	}
	require.NotNil(t, sessionViewPage, "the /session-view/{ns}/{name} route must be a declarative Page")
	assert.True(t, sessionViewPage.EmbeddableSameOrigin, "session-view opts into same-origin framing")
	assert.True(t, sessionViewPage.FramesSandbox, "session-view still frames its own sandbox-origin mcpui-host content")
}

// TestShell_CSPGainsSandboxFrameSrc proves the trusted page's FramesSandbox:
// true wiring (sessionview.go's Routes) actually reaches the CSP header — the
// framework's buildCSP adds frame-src <sandbox> only when a Page sets it, so
// this is the compile-time wiring's runtime proof. Mirrors artifactview's
// TestShell_HappyPath_RendersReactAppWithNonceCSPFramingSandbox.
//
// frame-ancestors is 'self', not 'none': this page also sets
// EmbeddableSameOrigin (Task 1 of the ap:session_view plan), so the session
// shell may frame it same-origin — cross-origin framing stays refused.
func TestShell_CSPGainsSandboxFrameSrc(t *testing.T) {
	d := fakeSV()
	s := newServer(t, "user:abc", d)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/session-view/ns1/sess1"))

	require.Equal(t, http.StatusOK, rec.Code)
	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "frame-src "+sandboxHost, "CSP must frame the sandbox content origin so /mcpui-host can be embedded")
	assert.Contains(t, csp, "frame-ancestors 'self'", "the page opts into same-origin framing by the session shell, but stays refused cross-origin")
	assert.Equal(t, "SAMEORIGIN", rec.Header().Get("X-Frame-Options"), "legacy-browser backstop must agree with frame-ancestors 'self'")
}

// TestShell_ActiveWidgets_IncludedInBootstrapProps proves shellBuild reads
// status.activeWidgets (via Deps.ActiveWidgets), mints a content-capability
// token per widget (via Deps.SignWidgetToken), and bootstraps an
// activeWidgets prop carrying each widget's artifactId + a ready-to-frame
// /mcpui-host URL — the shell never mints tokens client-side.
func TestShell_ActiveWidgets_IncludedInBootstrapProps(t *testing.T) {
	d := fakeSV()
	var gotNs, gotName string
	d.activeWidgets = func(ctx context.Context, ns, sess string) ([]sessionview.WidgetRef, error) {
		gotNs, gotName = ns, sess
		return []sessionview.WidgetRef{{ArtifactID: "artifact-w1", Tool: "render_chart", RendererKind: "mcpui"}}, nil
	}
	d.signWidgetToken = func(ns, sess, artifactID string) (string, error) {
		return "signed-" + artifactID, nil
	}
	s := newServer(t, "user:abc", d)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/session-view/ns1/sess1"))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"artifactId":"artifact-w1"`, "bootstrap props must carry the widget's artifact id")
	assert.Contains(t, body, `"hostUrl":"https://sandbox.example/mcpui-host?ct=signed-artifact-w1"`,
		"bootstrap props must carry a ready-to-frame /mcpui-host URL, not a bare artifact id")
	assert.Equal(t, "ns1", gotNs, "ActiveWidgets must run with the path namespace")
	assert.Equal(t, "sess1", gotName, "ActiveWidgets must run with the path session name")
}

// TestShell_ActiveWidgetsError_DegradesToNoWidgets proves a read failure
// bootstraps an empty widget list (logged, per no-silent-errors) rather than
// failing the whole page — a session-view with no widgets yet is still fully
// usable.
func TestShell_ActiveWidgetsError_DegradesToNoWidgets(t *testing.T) {
	d := fakeSV()
	d.activeWidgets = func(ctx context.Context, ns, sess string) ([]sessionview.WidgetRef, error) {
		return nil, assert.AnError
	}
	s := newServer(t, "user:abc", d)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(trustedHost, "/session-view/ns1/sess1"))

	require.Equal(t, http.StatusOK, rec.Code, "an ActiveWidgets error must not fail the whole page")
	assert.Contains(t, rec.Body.String(), `"activeWidgets":[]`, "degrades to an empty widget list, never leaked as a page error")
}

// TestShell_SandboxBaseNotAnHTTPOrigin_OmitsWidgetsAndTheOrigin is the
// sessionview half of the same fail-closed rule artifactview's urlsFor
// enforces. hostUrl lands on an iframe carrying
// sandbox="allow-scripts allow-same-origin" (SessionView.tsx's WidgetFrame),
// so the origin it resolves to gets script execution plus same-origin access
// to that origin.
//
// The empty row is the reachable one: `oap install` seeds the
// spicebox-webd-external-url ConfigMap with "" whenever it manages external
// access, and webd warns and keeps serving until the value lands.
// Concatenating "" yields the shell-RELATIVE "/mcpui-host?ct=…", which frames
// MCP-server-authored widget HTML in the TRUSTED origin. The page must still
// render — a session view with no widgets is fully usable — but with no widget
// and no sandbox origin, which the client already reads as "no bridge".
func TestShell_SandboxBaseNotAnHTTPOrigin_OmitsWidgetsAndTheOrigin(t *testing.T) {
	cases := []struct {
		name    string
		sandbox string
	}{
		{name: "javascript: base: widget omitted, page still renders", sandbox: "javascript:alert(1)"},
		{name: "empty base (ConfigMap not yet populated): widget omitted, no shell-relative URL", sandbox: ""},
		{name: "scheme-less host: widget omitted", sandbox: "sandbox.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := fakeSV()
			d.sandboxBaseURL = tc.sandbox
			d.activeWidgets = func(ctx context.Context, ns, sess string) ([]sessionview.WidgetRef, error) {
				return []sessionview.WidgetRef{{ArtifactID: "artifact-w1", RendererKind: "mcpui"}}, nil
			}
			s := newServer(t, "user:abc", d)

			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req(trustedHost, "/session-view/ns1/sess1"))

			require.Equal(t, http.StatusOK, rec.Code, "an unusable sandbox origin must not fail the whole page")
			body := rec.Body.String()
			assert.Contains(t, body, `"activeWidgets":[]`, "no widget may be framed without a usable sandbox origin")
			assert.NotContains(t, body, "/mcpui-host?ct=", "no host URL may be minted at all")
			assert.Contains(t, body, `"sandboxOrigin":""`, "the props must not carry a non-origin as the postMessage target")
		})
	}
}
