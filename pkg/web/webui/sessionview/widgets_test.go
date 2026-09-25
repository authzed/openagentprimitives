package sessionview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// widgetFakeDeps implements Deps for the /mcpui-content + /mcpui-host handler
// tests via per-behavior func fields, mirroring artifactview's fakeDeps.
type widgetFakeDeps struct {
	verifyWidgetToken func(token string) (string, string, string, error)
	fetchWidget       func(ctx context.Context, ns, sess, artifactID string) ([]byte, WidgetMeta, error)
	trustedOrigin     string
	sandboxBaseURL    string
	logger            logr.Logger
}

func (f *widgetFakeDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	return true, nil
}
func (f *widgetFakeDeps) OperatorURL() string    { return "" }
func (f *widgetFakeDeps) MemoryToken() string    { return "" }
func (f *widgetFakeDeps) NATS() *nats.Conn       { return nil }
func (f *widgetFakeDeps) TrustedOrigin() string  { return f.trustedOrigin }
func (f *widgetFakeDeps) Logger() logr.Logger    { return f.logger }
func (f *widgetFakeDeps) SandboxBaseURL() string { return f.sandboxBaseURL }
func (f *widgetFakeDeps) ActiveWidgets(ctx context.Context, ns, sess string) ([]WidgetRef, error) {
	return nil, nil
}
func (f *widgetFakeDeps) SignWidgetToken(ns, sess, artifactID string) (string, error) {
	return "tok-" + artifactID, nil
}
func (f *widgetFakeDeps) VerifyWidgetToken(token string) (string, string, string, error) {
	return f.verifyWidgetToken(token)
}
func (f *widgetFakeDeps) FetchWidget(ctx context.Context, ns, sess, artifactID string) ([]byte, WidgetMeta, error) {
	return f.fetchWidget(ctx, ns, sess, artifactID)
}

func widgetHappyDeps() *widgetFakeDeps {
	return &widgetFakeDeps{
		verifyWidgetToken: func(token string) (string, string, string, error) {
			if token != "goodtoken" {
				return "", "", "", assert.AnError
			}
			return "default", "sess1", "artifact-w1", nil
		},
		fetchWidget: func(ctx context.Context, ns, sess, artifactID string) ([]byte, WidgetMeta, error) {
			return []byte(`<html><body><script>doStuff()</script>hi</body></html>`), WidgetMeta{}, nil
		},
		trustedOrigin:  "https://shell.example",
		sandboxBaseURL: "https://sandbox.example",
		logger:         logr.Discard(),
	}
}

// TestMcpuiContent_HappyPath_ServesRawBytesVerbatim proves the core
// no-sanitize contract: a <script> tag in the fetched widget bytes survives
// byte-for-byte in the response — no ServeTransform, no rewrite. It also
// carries its own CSP: this is the document the widget's script actually
// executes in, so the connect-src/script-src/img-src restrictions must bind
// HERE, not only on /mcpui-host (see the package doc).
func TestMcpuiContent_HappyPath_ServesRawBytesVerbatim(t *testing.T) {
	d := widgetHappyDeps()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-content?ct=goodtoken", nil)
	mcpuiContentHandler(d).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Type"), "text/html")
	assert.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, `<html><body><script>doStuff()</script>hi</body></html>`, rr.Body.String(),
		"the widget's <script> must survive verbatim — no sanitize, no transform")
	csp := rr.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "connect-src 'none'", "the widget's own document must carry the CSP, not just the host")
	assert.Contains(t, csp, "frame-ancestors 'self' https://shell.example")
}

// TestMcpuiContent_BadToken_Forbidden proves a bad/expired/wrong-kind token
// is rejected with 403, never served.
func TestMcpuiContent_BadToken_Forbidden(t *testing.T) {
	d := widgetHappyDeps()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-content?ct=badtoken", nil)
	mcpuiContentHandler(d).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

// TestMcpuiContent_MetaCSP_BuildsPerWidgetPolicy proves the mapping the
// brief specifies (connectDomains -> connect-src, resourceDomains ->
// img/style/script-src, frameDomains -> frame-src) actually reaches the
// document where it matters, once FetchWidget returns a real WidgetMeta.
func TestMcpuiContent_MetaCSP_BuildsPerWidgetPolicy(t *testing.T) {
	d := widgetHappyDeps()
	d.fetchWidget = func(ctx context.Context, ns, sess, artifactID string) ([]byte, WidgetMeta, error) {
		return []byte(`<html></html>`), WidgetMeta{CSP: &WidgetCSPMeta{
			ConnectDomains:  []string{"https://api.example.com"},
			ResourceDomains: []string{"https://cdn.example.com"},
			FrameDomains:    []string{"https://embed.example.com"},
		}}, nil
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-content?ct=goodtoken", nil)
	mcpuiContentHandler(d).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	csp := rr.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "connect-src https://api.example.com")
	assert.Contains(t, csp, "script-src 'self' 'unsafe-inline' https://cdn.example.com")
	assert.Contains(t, csp, "img-src 'self' data: https://cdn.example.com")
	assert.Contains(t, csp, "style-src 'self' 'unsafe-inline' https://cdn.example.com")
	assert.Contains(t, csp, "frame-src 'self' https://embed.example.com")
}

// TestMcpuiContent_FetchWidgetError_BadGateway proves a fetch failure (after
// a valid token) surfaces as a 502, never a silent empty body.
func TestMcpuiContent_FetchWidgetError_BadGateway(t *testing.T) {
	d := widgetHappyDeps()
	d.fetchWidget = func(ctx context.Context, ns, sess, artifactID string) ([]byte, WidgetMeta, error) {
		return nil, WidgetMeta{}, assert.AnError
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-content?ct=goodtoken", nil)
	mcpuiContentHandler(d).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadGateway, rr.Code)
}

// TestMcpuiHost_HappyPath_MountsReactBundleWithEmbeddedWidget proves the host
// page (Task 3) mounts the mcpui-host React bundle — which renders the
// widget via @mcp-ui/client into an iframe sandbox="allow-scripts" WITHOUT
// allow-same-origin; see ui/mcpuihost/McpUiHost.test.tsx for the DOM-level
// proof of that sandbox posture, since the iframe itself is created
// client-side, not present in this server-rendered body. This Go-side test
// proves the SERVER half of the contract: the fetched widget bytes are
// embedded (safely) in the bootstrap JSON, and the bundle script is
// referenced.
func TestMcpuiHost_HappyPath_MountsReactBundleWithEmbeddedWidget(t *testing.T) {
	d := widgetHappyDeps()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-host?ct=goodtoken", nil)
	mcpuiHostHandler(d).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Type"), "text/html")
	assert.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"))
	body := rr.Body.String()
	assert.Contains(t, body, `<div id="root">`, "the bundle mounts into #root")
	assert.Contains(t, body, `<script type="module" src="/mcpui-host.js">`, "the host must reference the mcpui-host bundle")
	assert.Contains(t, body, `id="ap-bootstrap"`, "props travel via the standard bootstrap element")
	assert.Contains(t, body, `"uri":"ui://widget/artifact-w1"`)
	// The widget's script survives (HTML-escaped for safe embedding in the
	// bootstrap JSON — see TestMcpuiHost_WidgetHTMLWithScriptClose_EscapesSafely
	// for the adversarial case): the bundle re-parses it as a plain JSON string
	// and hands it to @mcp-ui/client as srcDoc, never as live markup on THIS
	// document.
	assert.Contains(t, body, `doStuff`)
	assert.NotEmpty(t, rr.Header().Get("Content-Security-Policy"))
}

// TestMcpuiHost_Bootstrap_IncludesSanitizedTrustedOrigin proves the sandbox->
// trusted bridge (Plan 5 Task 2) gets its postMessage target origin from the
// server, never a client-supplied or wildcard value: mcpuiHostHandler must
// stamp d.TrustedOrigin() (sanitized, same as buildWidgetCSP's shellOrigin)
// into the bootstrap JSON's trustedOrigin field, which McpUiHost.tsx's
// logUIAction later uses as postMessage's second argument.
func TestMcpuiHost_Bootstrap_IncludesSanitizedTrustedOrigin(t *testing.T) {
	d := widgetHappyDeps()
	d.trustedOrigin = "https://shell.example; evil-directive"
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-host?ct=goodtoken", nil)
	mcpuiHostHandler(d).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, `"trustedOrigin":"https://shell.example"`,
		"the bootstrap must carry the sanitized trusted origin, truncated at the injection attempt")
	assert.NotContains(t, body, "evil-directive")
}

// TestMcpuiHost_WidgetHTMLWithScriptClose_EscapesSafely proves the core
// safety claim behind embedding the widget's raw (unsanitized) bytes directly
// in this document's own <script type="application/json" id="ap-bootstrap">
// element: a literal "</script>" inside the widget's HTML must never be able
// to close that element early and inject sibling markup into the TRUSTED
// mcpui-host document itself (as opposed to the widget's own later srcDoc
// iframe, which is a separate, isolated document — this test is about THIS
// response's own parse safety).
func TestMcpuiHost_WidgetHTMLWithScriptClose_EscapesSafely(t *testing.T) {
	d := widgetHappyDeps()
	d.fetchWidget = func(ctx context.Context, ns, sess, artifactID string) ([]byte, WidgetMeta, error) {
		return []byte(`</script><img src=x onerror="alert(1)">`), WidgetMeta{}, nil
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-host?ct=goodtoken", nil)
	mcpuiHostHandler(d).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.NotContains(t, body, "</script><img", "a literal </script> in widget HTML must never close the bootstrap element early")
	// Go's json.Encoder.SetEscapeHTML(true) rewrites the raw '<' and '>' bytes
	// to the 6-character sequence backslash+u+003c (and 003e) inside the JSON
	// string value — that's what actually neutralizes the closing tag. Assert
	// THAT escaped form is genuinely present, rather than relying on the
	// absence check above alone (which would also pass if the handler had
	// simply written nothing at all). Built from rune literals instead of a
	// hand-typed escape sequence so the test source is unambiguous about
	// exactly which bytes it expects.
	backslash := string(rune(0x5c))
	escapedOpen := backslash + "u003c"  // what '<' becomes
	escapedClose := backslash + "u003e" // what '>' becomes
	assert.Contains(t, body, escapedOpen+"/script"+escapedClose+escapedOpen+"img",
		"the malicious sequence must survive ONLY in its HTML-escaped JSON-string form")
}

// TestMcpuiHost_FetchWidgetError_BadGateway mirrors
// TestMcpuiContent_FetchWidgetError_BadGateway: the host page ALSO depends on
// FetchWidget succeeding now that it embeds the widget's bytes server-side,
// so a fetch failure must surface as a 502, never a blank/silent page.
func TestMcpuiHost_FetchWidgetError_BadGateway(t *testing.T) {
	d := widgetHappyDeps()
	d.fetchWidget = func(ctx context.Context, ns, sess, artifactID string) ([]byte, WidgetMeta, error) {
		return nil, WidgetMeta{}, assert.AnError
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-host?ct=goodtoken", nil)
	mcpuiHostHandler(d).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadGateway, rr.Code)
}

// TestMcpuiHost_InvalidToken_403 mirrors artifactview's
// TestHostHandler_InvalidToken_403.
func TestMcpuiHost_InvalidToken_403(t *testing.T) {
	d := widgetHappyDeps()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-host?ct=badtoken", nil)
	mcpuiHostHandler(d).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

// TestMcpuiHost_NoMeta_UsesRestrictiveDefaultCSP proves the safe-fallback
// contract: when FetchWidget returns a nil WidgetMeta.CSP (widgetHappyDeps'
// fake never sets one), the host page's CSP falls back to the restrictive
// default rather than an error or an over-permissive policy. The mapping
// FROM a real meta is proven separately by TestBuildWidgetCSP_FromMeta below,
// and the producer side (webd's FetchWidget threading a persisted CSP
// through) by internal/cmd/webd's own FetchWidget tests.
func TestMcpuiHost_NoMeta_UsesRestrictiveDefaultCSP(t *testing.T) {
	d := widgetHappyDeps()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-host?ct=goodtoken", nil)
	mcpuiHostHandler(d).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	csp := rr.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'none'")
	assert.Contains(t, csp, "connect-src 'none'")
	assert.Contains(t, csp, "script-src 'self' 'unsafe-inline'")
	assert.Contains(t, csp, "img-src 'self' data:")
}

// TestMcpuiHost_CSP_FrameAncestorsIncludesShellOrigin proves the two-level
// nesting math: frame-ancestors must include BOTH 'self' (the immediate
// parent for /mcpui-content's own load) and the trusted shell origin (the
// top-level ancestor for /mcpui-host's own load) — see buildWidgetCSP's doc.
func TestMcpuiHost_CSP_FrameAncestorsIncludesShellOrigin(t *testing.T) {
	d := widgetHappyDeps()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcpui-host?ct=goodtoken", nil)
	mcpuiHostHandler(d).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	csp := rr.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "frame-ancestors 'self' https://shell.example")
}

// TestBuildWidgetCSP_DefaultWhenMetaAbsent + TestBuildWidgetCSP_FromMeta are
// direct unit tests of the pure CSP builder, independent of the HTTP layer.
func TestBuildWidgetCSP_DefaultWhenMetaAbsent(t *testing.T) {
	csp := buildWidgetCSP(nil, "https://shell.example")
	assert.Contains(t, csp, "default-src 'none'")
	assert.Contains(t, csp, "connect-src 'none'")
	assert.Contains(t, csp, "script-src 'self' 'unsafe-inline'")
	assert.Contains(t, csp, "img-src 'self' data:")
	assert.NotContains(t, csp, "style-src", "no style-src grant without a declared resourceDomains need")
}

func TestBuildWidgetCSP_FromMeta(t *testing.T) {
	csp := buildWidgetCSP(&WidgetCSPMeta{
		ConnectDomains:  []string{"https://api.example.com", "https://api2.example.com"},
		ResourceDomains: []string{"https://cdn.example.com"},
	}, "https://shell.example")
	assert.Contains(t, csp, "connect-src https://api.example.com https://api2.example.com")
	assert.Contains(t, csp, "script-src 'self' 'unsafe-inline' https://cdn.example.com")
	assert.Contains(t, csp, "img-src 'self' data: https://cdn.example.com")
	assert.Contains(t, csp, "style-src 'self' 'unsafe-inline' https://cdn.example.com")
	// No frameDomains declared: frame-src stays 'self' only (still required so
	// the host can embed its own /mcpui-content).
	assert.Contains(t, csp, "frame-src 'self'")
	assert.NotContains(t, csp, "frame-src 'self' https://", "frame-src must not gain a domain nobody declared")
}

// TestBuildWidgetCSP_SanitizesUntrustedDomains proves the security guard
// this task adds: a widget's `_meta.ui.csp` domain lists are MCP-server-
// authored and therefore untrusted — a token carrying a `;` could splice an
// extra directive into the CSP header (e.g. "https://evil.test; script-src
// * 'unsafe-eval'"). buildWidgetCSP must filter every declared domain
// through webui.SanitizeCSPSourceToken before joining, dropping any token
// that fails the check, while a legitimate wildcard source (a valid CSP
// source-list token, not a parseable origin — see SanitizeCSPSourceToken's
// doc) survives unchanged.
func TestBuildWidgetCSP_SanitizesUntrustedDomains(t *testing.T) {
	csp := buildWidgetCSP(&WidgetCSPMeta{
		ConnectDomains:  []string{"https://api.example.test", "https://evil.test; script-src * 'unsafe-eval'"},
		ResourceDomains: []string{"https://*.cdn.example.test"},
		FrameDomains:    []string{"https://embed.example.test", "bad\"onload"},
	}, "https://shell.example")

	assert.Contains(t, csp, "connect-src https://api.example.test", "the clean domain must survive")
	assert.NotContains(t, csp, "evil.test", "the injection-carrying domain must be dropped entirely")

	// Extract the connect-src directive value in isolation and prove it
	// contains no stray semicolon — the character that would let an injected
	// token splice in a whole new directive.
	directives := strings.Split(csp, "; ")
	var connectDirective string
	for _, d := range directives {
		if strings.HasPrefix(d, "connect-src ") {
			connectDirective = d
			break
		}
	}
	require.NotEmpty(t, connectDirective, "connect-src directive must be present")
	assert.NotContains(t, connectDirective, ";", "connect-src value must not contain a semicolon from a dropped injection token")

	assert.Contains(t, csp, "img-src 'self' data: https://*.cdn.example.test", "a legitimate wildcard resource domain must survive")
	assert.Contains(t, csp, "frame-src 'self' https://embed.example.test", "the clean frame domain must survive")
	assert.NotContains(t, csp, "onload", "the injection-carrying frame domain must be dropped entirely")
}

// TestLive_ForwardsWidgetOfferEnvelope_WithMintedHostURL covers the brief's
// "the live path forwards KindWidgetOffer" TDD point: a widget_offer envelope
// published on NATS arrives client-side as an "event" frame whose kind is
// still widget_offer, but whose payload has been rewritten to carry a
// server-minted /mcpui-host URL (the browser holds no signing key — see
// live.go's mintWidgetOfferHostURL).
func TestLive_ForwardsWidgetOfferEnvelope_WithMintedHostURL(t *testing.T) {
	natsSrv := natstest.RunServer(&natsserver.Options{Port: -1})
	defer natsSrv.Shutdown()
	subConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	defer subConn.Close()
	pubConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	defer pubConn.Close()

	const ns, name = "default", "sess1"
	var trusted string
	d := liveTestDeps(&trusted)
	d.nc = subConn
	d.sandboxBaseURL = "https://sandbox.example"
	d.signWidgetToken = func(ns, sess, artifactID string) (string, error) {
		return "minted-" + artifactID, nil
	}

	conn, resp := dialLive(t, d, &trusted, ns, name)
	require.NotNil(t, conn)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	_ = readLive(t, conn) // initial snapshot

	evt := publishUntilDelivered(t, conn, pubConn, ns, name,
		channelevents.KindWidgetOffer, channelevents.WidgetOfferPayload{ArtifactID: "artifact-w1", Tool: "render_chart", RendererKind: "mcpui"})
	require.Equal(t, "event", evt.Type)
	require.NotNil(t, evt.Event)
	assert.Equal(t, channelevents.KindWidgetOffer, evt.Event.Kind)

	var pl widgetOfferClientPayload
	require.NoError(t, json.Unmarshal(evt.Event.Payload, &pl))
	assert.Equal(t, "artifact-w1", pl.ArtifactID)
	assert.Equal(t, "render_chart", pl.Tool)
	assert.Equal(t, "https://sandbox.example/mcpui-host?ct=minted-artifact-w1", pl.HostURL,
		"the live path must mint a ready-to-frame host URL, not forward a bare artifact id")
}

// TestLive_WidgetOffer_MintFailure_ForwardsWithoutHostURL proves the
// best-effort degrade: a SignWidgetToken failure is logged and the envelope
// still reaches the client (with an empty hostUrl the frontend skips
// rendering), rather than killing the socket or dropping the event silently.
func TestLive_WidgetOffer_MintFailure_ForwardsWithoutHostURL(t *testing.T) {
	natsSrv := natstest.RunServer(&natsserver.Options{Port: -1})
	defer natsSrv.Shutdown()
	subConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	defer subConn.Close()
	pubConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	defer pubConn.Close()

	const ns, name = "default", "sess1"
	var trusted string
	d := liveTestDeps(&trusted)
	d.nc = subConn
	d.signWidgetToken = func(ns, sess, artifactID string) (string, error) {
		return "", assert.AnError
	}

	conn, resp := dialLive(t, d, &trusted, ns, name)
	require.NotNil(t, conn)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	_ = readLive(t, conn)

	evt := publishUntilDelivered(t, conn, pubConn, ns, name,
		channelevents.KindWidgetOffer, channelevents.WidgetOfferPayload{ArtifactID: "artifact-w1"})
	require.Equal(t, "event", evt.Type)
	require.NotNil(t, evt.Event)
	assert.Equal(t, channelevents.KindWidgetOffer, evt.Event.Kind)

	// The original (unrewritten) payload is forwarded on a mint failure — no
	// hostUrl field at all, which the frontend's handleFrame treats as
	// "nothing to frame yet" rather than crashing on a malformed payload.
	var pl channelevents.WidgetOfferPayload
	require.NoError(t, json.Unmarshal(evt.Event.Payload, &pl))
	assert.Equal(t, "artifact-w1", pl.ArtifactID)
}
