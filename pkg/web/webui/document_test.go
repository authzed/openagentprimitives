package webui

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderDocumentInjectsAppScriptsAndBootstrap(t *testing.T) {
	rec := httptest.NewRecorder()
	err := renderDocument(rec, docInput{
		App:      "system",
		Title:    "Hi",
		Props:    map[string]any{"k": "v"},
		Scripts:  []string{"/assets/system.abc.js"},
		CSS:      []string{"/assets/vendor.def.css"},
		Nonce:    "NONCE123",
		CSP:      "default-src 'none'",
		Fallback: "<p>loading</p>",
	})
	require.NoError(t, err)
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="system"`)
	assert.Contains(t, body, `<script type="module" src="/assets/system.abc.js" nonce="NONCE123">`)
	assert.Contains(t, body, `<link rel="stylesheet" href="/assets/vendor.def.css">`)
	assert.Contains(t, body, `<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,`, "every framework page carries the OAP favicon")
	assert.Contains(t, body, `id="ap-bootstrap"`)
	assert.Contains(t, body, `{"k":"v"}`)
	assert.Contains(t, body, "<p>loading</p>")
	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	// CSP is delivered as a header (so frame-ancestors/form-action work), NOT a meta.
	assert.Equal(t, "default-src 'none'", rec.Header().Get("Content-Security-Policy"))
	assert.NotContains(t, body, `http-equiv="Content-Security-Policy"`, "CSP must not be in a <meta>")
	// Defense-in-depth headers on every framework page.
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, 200, rec.Code, "Status 0 ⇒ implicit 200")
}

func TestRenderDocumentWritesExplicitStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	require.NoError(t, renderDocument(rec, docInput{
		App: "system", Nonce: "N", CSP: "default-src 'none'", Status: 404,
	}))
	assert.Equal(t, 404, rec.Code)
	assert.Equal(t, "default-src 'none'", rec.Header().Get("Content-Security-Policy"),
		"CSP header set even on a non-200 (header before WriteHeader)")
}

func TestRenderDocumentEscapesPropsAgainstScriptBreakout(t *testing.T) {
	rec := httptest.NewRecorder()
	require.NoError(t, renderDocument(rec, docInput{
		App: "system", Title: "<x>", Props: map[string]any{"x": "</script><script>alert(1)"},
		Nonce: "N", CSP: "default-src 'none'",
	}))
	body := rec.Body.String()
	assert.NotContains(t, body, "</script><script>alert(1)", "raw breakout must not appear")
	assert.Contains(t, body, `</script>`, "legit closing script tags still present")
	assert.NotContains(t, body, "<title><x></title>", "title must be HTML-escaped")
}

func TestBuildCSPAddsFrameSrcWhenFramesSandbox(t *testing.T) {
	base := buildCSP("NONCE", false, "https://sandbox.example", devConfig{}, false, false)
	assert.NotContains(t, base, "frame-src")
	withFrame := buildCSP("NONCE", true, "https://sandbox.example", devConfig{}, false, false)
	assert.Contains(t, withFrame, "frame-src https://sandbox.example")
	assert.Contains(t, withFrame, "script-src 'nonce-NONCE'")
}

// The sandbox origin buildCSP stamps into frame-src is operator-controlled at
// runtime (webd re-reads it from a ConfigMap every 10s and stores it verbatim),
// so it must go through SanitizeOrigin like the other two sinks that embed it
// (artifactview/host.go, sessionview/widgets.go) — otherwise a space widens
// frame-src to a second host and a ";" splices in a literal extra directive.
func TestBuildCSPSanitizesSandboxOriginIntoFrameSrc(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		want   string // the exact frame-src directive expected
		absent []string
	}{
		{
			name:   "space-separated payload: host-widening is truncated away",
			origin: "https://sandbox.example evil.example",
			want:   "frame-src https://sandbox.example",
			absent: []string{"evil.example"},
		},
		{
			name:   "semicolon payload: the spliced directive is truncated away",
			origin: "https://sandbox.example; base-uri *",
			want:   "frame-src https://sandbox.example",
			absent: []string{"base-uri *"},
		},
		{
			name:   "path/query are dropped, leaving a bare origin",
			origin: "https://sandbox.example/x?y=1",
			want:   "frame-src https://sandbox.example",
			absent: []string{"?y=1"},
		},
		{
			name:   "clean origin with a trailing slash is unchanged",
			origin: "https://sandbox.example/",
			want:   "frame-src https://sandbox.example",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			csp := buildCSP("N", true, tc.origin, devConfig{}, false, false)
			assert.Contains(t, csp, tc.want)
			for _, a := range tc.absent {
				assert.NotContains(t, csp, a)
			}
		})
	}
}

// A sandbox origin that sanitizes to nothing (a payload with no clean prefix)
// must drop the directive rather than emit a valueless "frame-src", which would
// be a malformed directive rather than a deliberate policy.
func TestBuildCSPOmitsFrameSrcWhenOriginSanitizesEmpty(t *testing.T) {
	csp := buildCSP("N", true, " evil.example", devConfig{}, false, false)
	assert.NotContains(t, csp, "frame-src")
	assert.NotContains(t, csp, "evil.example")
}

func TestBuildCSPDevModeLoosens(t *testing.T) {
	csp := buildCSP("N", false, "", devConfig{enabled: true, url: "http://localhost:5173"}, false, false)
	assert.Contains(t, csp, "http://localhost:5173")
	assert.Contains(t, csp, "'unsafe-eval'")
}

// TestSetWebDevSanitizesForEveryDevSink drives the dev origin through its ONE
// choke point — Server.SetWebDev, the only place a non-zero devConfig is built
// — and asserts every sink downstream of it, because the value fans out to two
// separate consumers that had disagreed:
//
//   - buildCSP puts it in script-src, connect-src (plus the ws:// derivation)
//     and style-src. script-src is the sharpest: a widened value there grants
//     script execution to an extra host.
//   - registerDevScripts puts it in two <script src> attributes AND in an
//     inline module body, which cspassets emits VERBATIM (RenderWith escapes
//     Type/ID/Src but never Body). That body is a JS string literal, so a
//     configured `http://x";alert(1);//` closed the literal and executed under
//     the page nonce.
//
// Sanitizing per sink is what let the second consumer keep a raw value after
// the first was fixed, so the test is written against the constructor: a value
// that is clean when it is stored is clean at every sink, present and future.
func TestSetWebDevSanitizesForEveryDevSink(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "space host-widening: truncated at the constructor", url: "http://localhost:5173 https://evil.example"},
		{name: "semicolon directive splice: truncated at the constructor", url: "http://localhost:5173; base-uri *"},
		{name: "newline host-widening: truncated at the constructor", url: "http://localhost:5173\nhttps://evil.example"},
		{name: "tab host-widening: truncated at the constructor", url: "http://localhost:5173\thttps://evil.example"},
		{name: "JS string breakout: cannot reach the inline module body", url: `http://localhost:5173";alert(1);//`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s Server
			s.SetWebDev(tc.url)

			csp := buildCSP("N", false, "", s.dev, false, false)
			assert.Contains(t, csp, "http://localhost:5173", "the clean dev origin prefix still grants dev mode")
			assert.NotContains(t, csp, "evil.example", "no smuggled host may reach the policy")
			assert.NotContains(t, csp, "base-uri *", "no spliced directive may reach the policy")
			assert.Equal(t, -1, strings.IndexFunc(csp, func(r rune) bool { return r < 0x20 || r == 0x7f }),
				"the CSP header value must contain no control byte")

			rec := httptest.NewRecorder()
			require.NoError(t, renderDocument(rec, docInput{App: "system", Nonce: "N", CSP: csp, Dev: s.dev}))
			body := rec.Body.String()
			assert.Contains(t, body, `import R from "http://localhost:5173/assets/@react-refresh"`,
				"the clean dev origin still wires the react-refresh preamble")
			assert.NotContains(t, body, "alert(1)", "the dev origin must not break out of the inline module's JS string literal")
			assert.NotContains(t, body, "evil.example", "no smuggled host may reach a <script src>")
		})
	}
}

func TestBuildCSPHasFormActionAndFrameAncestors(t *testing.T) {
	csp := buildCSP("N", false, "", devConfig{}, false, false)
	assert.Contains(t, csp, "form-action 'self'", "restrict native form posts to same-origin")
	assert.Contains(t, csp, "frame-ancestors 'none'", "framework pages are top-level (clickjacking protection)")
}

func TestBuildCSPStillStrictBaseline(t *testing.T) {
	csp := buildCSP("N", false, "", devConfig{}, false, false)
	assert.Contains(t, csp, "default-src 'none'")
	assert.Contains(t, csp, "script-src 'nonce-N'")
	assert.NotContains(t, csp, "'unsafe-eval'") // prod: no eval
}

func TestBuildCSPStyleSrcAllowsExternalStylesheet(t *testing.T) {
	csp := buildCSP("N", false, "", devConfig{}, false, false)
	// 'self' permits the external /assets bundle stylesheet; 'unsafe-inline' alone
	// would block it (it only covers inline styles).
	assert.Contains(t, csp, "style-src 'self' 'unsafe-inline'")
}

func TestBuildCSPFontSrcAllowsDataURIs(t *testing.T) {
	csp := buildCSP("N", false, "", devConfig{}, false, false)
	// The @ap/design bundle inlines the JetBrains Mono variable font as a
	// data:font/woff2 URI (Inter is served from /assets, but the mono face is a
	// data: URI). 'self' alone blocks it, so the shell falls back to a system
	// monospace. 'data:' restores it — and matches the artifact renderer's own
	// font-src ('self' data:), the other document that hosts embedded fonts.
	assert.Contains(t, csp, "font-src 'self' data:")
}

func TestBuildCSPFramingFlags(t *testing.T) {
	cases := []struct {
		name                 string
		framesSandbox        bool
		sandbox              string
		embeddable           bool
		framesSelf           bool
		wantAncestors        string   // the frame-ancestors directive expected, verbatim
		wantFrameSrcContains []string // substrings the (single) frame-src directive must contain
		wantNoFrameSrc       bool     // true means no frame-src directive at all
	}{
		{name: "default: locked down (regression guard)",
			wantAncestors: "frame-ancestors 'none'", wantNoFrameSrc: true},
		{name: "embeddable adds frame-ancestors self",
			embeddable: true, wantAncestors: "frame-ancestors 'self'", wantNoFrameSrc: true},
		{name: "framesSameOrigin adds frame-src self",
			framesSelf: true, wantAncestors: "frame-ancestors 'none'",
			wantFrameSrcContains: []string{"'self'"}},
		{name: "sandbox plus self merge into ONE frame-src directive",
			framesSandbox: true, sandbox: "https://sandbox.example", framesSelf: true,
			wantAncestors:        "frame-ancestors 'none'",
			wantFrameSrcContains: []string{"https://sandbox.example", "'self'"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			csp := buildCSP("n0nce", tc.framesSandbox, tc.sandbox, devConfig{}, tc.embeddable, tc.framesSelf)
			assert.Contains(t, csp, tc.wantAncestors)
			// Exactly one frame-src directive ever (a duplicate is ignored by the browser).
			assert.LessOrEqual(t, strings.Count(csp, "frame-src "), 1, "at most one frame-src directive")
			if tc.wantNoFrameSrc {
				assert.NotContains(t, csp, "frame-src ")
			}
			for _, want := range tc.wantFrameSrcContains {
				assert.Contains(t, csp, want)
			}
		})
	}
}
