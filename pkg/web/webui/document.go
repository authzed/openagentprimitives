package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/webui/cspassets"
)

// devConfig controls dev-mode script wiring (Vite dev server). Zero value =
// production (serve embedded bundles).
//
// INVARIANT: url is already SanitizeOrigin'd. newDevConfig is the only
// constructor and sanitizes on the way in, so no consumer can forget to. The
// operator-supplied value (--web-dev-url / WEBD_WEB_DEV_URL) fans out to
// buildCSP's script-src/connect-src (plus its ws:// derivation)/style-src and to
// registerDevScripts' <script src> attributes and INLINE module body — and that
// body is why per-sink sanitizing is not enough: cspassets.RenderWith escapes
// Type/ID/Src but emits Body verbatim, and the body is a JS string literal, so
// `http://x";alert(1);//` closes the literal and runs under the page nonce.
type devConfig struct {
	enabled bool
	url     string // Vite dev server base, e.g. http://localhost:5173 (sanitized)
}

// newDevConfig builds the enabled devConfig, establishing its url invariant.
// SanitizeOrigin reduces the value to a bare scheme://host, which is what every
// sink wants: the CSP directives take an origin, and registerDevScripts appends
// its own "/assets" path.
func newDevConfig(url string) devConfig {
	return devConfig{enabled: true, url: SanitizeOrigin(strings.TrimRight(url, "/"))}
}

// docInput is everything renderDocument needs to emit one HTML page.
type docInput struct {
	App      string
	Title    string
	Props    any
	Scripts  []string // production bundle URLs (entry); ignored in dev mode
	CSS      []string // production CSS URLs; ignored in dev mode
	Nonce    string
	CSP      string
	Fallback string    // server-rendered styled markup placed inside #root
	Dev      devConfig // when enabled, scripts come from the Vite dev server
	// Status is the HTTP status to write (0 ⇒ implicit 200). renderDocument owns
	// the header→status→body ordering so the CSP/Content-Type headers are always
	// sent before WriteHeader (callers must NOT call WriteHeader themselves).
	Status int
	// EmbeddableSameOrigin mirrors Page.EmbeddableSameOrigin: when true,
	// X-Frame-Options is relaxed from DENY to SAMEORIGIN to match the CSP's
	// frame-ancestors 'self' (buildCSP owns the CSP header itself; this only
	// keeps the legacy-browser backstop in agreement with it).
	EmbeddableSameOrigin bool
}

// criticalCSS is inlined so the server-rendered fallback is correctly themed
// before the design stylesheet loads (and even if it never does). Hardcoded
// design hexes — not the CSS vars, which arrive with the async stylesheet.
const criticalCSS = `html,body{margin:0;height:100%}` +
	`body{background:#0c050f;color:#e9e7e9;font-family:Inter Variable,system-ui,sans-serif}` +
	`.ap-fallback{min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px}` +
	`.ap-fallback .card{max-width:28rem;width:100%;background:#1e1424;border:1px solid #3f3644;border-radius:.5rem;padding:20px}` +
	`.ap-fallback h1{font-size:1rem;margin:0 0 .5rem}` +
	`.ap-fallback p{font-size:.875rem;color:#a99fb0;margin:.25rem 0}`

// FaviconHref is the OAP mark as an SVG data URI, shared with every page that
// is not a framework document (identityd's sign-in page renders its own HTML). A data: favicon needs no
// route and no asset entry, and the CSP already allows `img-src data:`. The
// embedded stylesheet flips the ink with the browser's colour scheme, because
// a favicon sits on the browser's tab strip, not on this page's theme — a
// near-white mark on a light tab strip would vanish.
var FaviconHref = "data:image/svg+xml," + url.PathEscape(
	`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 345 305">`+
		`<style>path{fill:#1D1423}@media(prefers-color-scheme:dark){path{fill:#F1F0F2}}</style>`+
		`<path d="M159.186 7.59677C164.993 -2.53226 179.604 -2.53226 185.411 7.59677L342.572 281.702C348.349 291.779 341.075 304.335 329.459 304.335H15.138C3.52262 304.335 -3.75273 291.779 2.02468 281.702L159.186 7.59677ZM178.855 133.226C175.951 128.161 168.646 128.161 165.742 133.226L117.254 217.794C114.366 222.832 118.003 229.11 123.811 229.11H220.786C226.594 229.11 230.232 222.832 227.343 217.794L178.855 133.226Z"/></svg>`)

// themeBootScript resolves the viewer's theme before first paint. Same rule as
// design/theme.ts: localStorage "oap.theme" ∈ {dark, light, system}, default
// system, system = prefers-color-scheme. Deliberately tiny and dependency-free.
const themeBootScript = `(function(){try{var c=localStorage.getItem("oap.theme");if(c!=="light"&&c!=="dark")c=window.matchMedia("(prefers-color-scheme: light)").matches?"light":"dark";document.documentElement.dataset.theme=c}catch(e){document.documentElement.dataset.theme="dark"}})();`

// renderDocument writes the shared HTML document. It assembles the page by hand
// (not html/template) for precise control of the bootstrap script: props are
// JSON-marshaled with HTML escaping (< > & -> &lt; ...) so the value cannot
// break out of the <script type="application/json"> element.
func renderDocument(w http.ResponseWriter, in docInput) error {
	bootstrap, err := MarshalBootstrap(in.Props)
	if err != nil {
		return err
	}
	// cspassets stamps the shared request nonce onto each tag, so it is never
	// threaded through by hand. The CSP is built separately (buildCSP) and set as
	// a header below; deriving style-src from these assets would add a 'nonce'
	// source, which makes the browser IGNORE 'unsafe-inline' and break the
	// runtime inline styles Radix/shadcn inject.
	var assets cspassets.Set
	assets.Style(criticalCSS)
	assets.AddScript(cspassets.ScriptSpec{Type: "application/json", ID: "ap-bootstrap", Body: bootstrap})
	// Runs before the app bundle: mirrors design/theme.ts readThemeChoice +
	// resolveTheme so the first paint already has the right theme. Keep the
	// storage key and the "system" default in step with that module.
	assets.AddScript(cspassets.ScriptSpec{Body: themeBootScript})
	if in.Dev.enabled {
		registerDevScripts(&assets, in.Dev.url)
	} else {
		for _, src := range in.Scripts {
			assets.AddScript(cspassets.ScriptSpec{Type: "module", Src: src})
		}
	}
	// This page registers no Policy directives (its CSP is buildCSP's), so the
	// error can only be a programming mistake in a future edit — which is what it
	// is there to catch, rather than shipping a page whose policy silently lost a
	// directive.
	tags, err := assets.RenderWith(in.Nonce)
	if err != nil {
		return err
	}

	var b strings.Builder
	// No theme attribute is stamped here: themeBootScript (below, nonced) sets
	// data-theme before first paint from the viewer's stored choice, defaulting
	// to the OS preference. Stamping "dark" here made every light-mode load
	// paint dark once; stamping nothing lets the script own it.
	b.WriteString("<!doctype html><html lang=\"en\"><head>")
	b.WriteString(`<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`)
	// No <meta http-equiv> CSP here: frame-ancestors/form-action are ignored in a
	// meta tag, so the policy is delivered as a header below.
	fmt.Fprintf(&b, "<title>%s</title>", html.EscapeString(in.Title))
	fmt.Fprintf(&b, `<link rel="icon" type="image/svg+xml" href="%s">`, html.EscapeString(FaviconHref))
	b.WriteString(tags.Styles) // critical CSS <style nonce>
	if !in.Dev.enabled {
		for _, href := range in.CSS {
			fmt.Fprintf(&b, `<link rel="stylesheet" href="%s">`, html.EscapeString(href))
		}
	}
	b.WriteString("</head><body>")
	fmt.Fprintf(&b, `<div id="root" data-app="%s">%s</div>`, html.EscapeString(in.App), in.Fallback)
	b.WriteString(tags.Scripts) // bootstrap JSON + module entry (or dev scripts)
	b.WriteString("</body></html>")

	// Headers MUST be set before WriteHeader/Write.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", in.CSP)
	// Defense-in-depth alongside the CSP: nosniff stops MIME confusion on these
	// HTML responses; Referrer-Policy keeps signed deep-link / OAuth URLs out of
	// the Referer header on outbound navigations; X-Frame-Options is the
	// legacy-browser backstop for frame-ancestors (DENY for the default 'none',
	// SAMEORIGIN for a page that opted into same-origin framing — every other
	// renderDocument page is a top-level document, never embedded).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	xfo := "DENY"
	if in.EmbeddableSameOrigin {
		xfo = "SAMEORIGIN"
	}
	w.Header().Set("X-Frame-Options", xfo)
	if in.Status != 0 {
		w.WriteHeader(in.Status)
	}
	_, werr := w.Write([]byte(b.String()))
	return werr
}

// registerDevScripts registers the Vite client, the react-refresh preamble, and
// the universal dev entry (mounts by #root[data-app]) onto the asset set. All
// paths sit under the /assets/ base because the Vite dev server mounts there
// (vite base applies to dev too).
//
// devURL must come from a devConfig — i.e. already sanitized. The preamble
// concatenates it into a JS string literal that cspassets emits verbatim, so an
// unsanitized value here executes arbitrary script under the page nonce.
func registerDevScripts(a *cspassets.Set, devURL string) {
	base := strings.TrimRight(devURL, "/") + "/assets"
	a.AddScript(cspassets.ScriptSpec{Type: "module", Src: base + "/@vite/client"})
	a.AddScript(cspassets.ScriptSpec{Type: "module", Body: `import R from "` + base + `/@react-refresh";R.injectIntoGlobalHook(window);` +
		`window.$RefreshReg$=()=>{};window.$RefreshSig$=()=>(t)=>t;` +
		`window.__vite_plugin_react_preamble_installed__=true;`})
	a.AddScript(cspassets.ScriptSpec{Type: "module", Src: base + "/dev/entry.ts"})
}

// MarshalBootstrap JSON-encodes props with HTML escaping on, then escapes the JS
// line terminators U+2028/U+2029. Nil props -> "{}". Exported so a raw
// http.Handler that hand-rolls its own document (outside the Page/renderDocument
// path — e.g. sessionview's mcpui-host, which needs its OWN CSP rather than
// buildCSP's nonce-based one) embeds its <script type="application/json"
// id="ap-bootstrap"> element with exactly the escaping renderDocument relies on,
// not a second hand-rolled copy.
func MarshalBootstrap(props any) (string, error) {
	if props == nil {
		return "{}", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(props); err != nil {
		return "", err
	}
	s := strings.TrimRight(buf.String(), "\n")
	// Defensive: escape the JS line terminators U+2028/U+2029 (harmless inside the
	// application/json script we emit, but keeps the value safe if ever inlined).
	s = strings.ReplaceAll(s, "\u2028", `\u2028`)
	s = strings.ReplaceAll(s, "\u2029", `\u2029`)
	return s, nil
}

// newNonce returns a fresh base64url CSP nonce (the shared minter for webui pages).
func newNonce() (string, error) { return cspassets.NewNonce() }

// buildCSP assembles the per-request CSP. Production is strict; framesSandbox
// adds frame-src; dev mode allows the Vite origin + eval + HMR websocket.
// embeddableSameOrigin relaxes frame-ancestors to 'self' (the session-view
// page, embedded by the session shell's ap:session_view); framesSameOrigin
// adds 'self' to frame-src (the session shell itself).
func buildCSP(nonce string, framesSandbox bool, sandboxOrigin string, dev devConfig, embeddableSameOrigin, framesSameOrigin bool) string {
	scriptSrc := "'nonce-" + nonce + "'"
	connectSrc := "'self'"
	styleSrc := "'self' 'unsafe-inline'"
	if dev.enabled {
		// dev.url is already sanitized (see devConfig) and is a bare origin, so
		// the ws:// derivation below still matches. A value that sanitized to
		// empty grants the dev origin nothing, failing visibly in dev rather than
		// widening the policy.
		u := dev.url
		ws := strings.Replace(strings.Replace(u, "http://", "ws://", 1), "https://", "wss://", 1)
		scriptSrc = scriptSrc + " 'unsafe-inline' 'unsafe-eval' " + u
		connectSrc = connectSrc + " " + u + " " + ws
		// Vite dev may serve the design CSS as a <link> from the dev origin.
		styleSrc = styleSrc + " " + u
	}
	// frame-ancestors does not fall back to default-src; framework pages are
	// top-level documents that must never be embedded (clickjacking) — unless
	// the page opts into same-origin framing. This governs who may frame THESE
	// pages, not the frame-src below, which governs what this page may frame.
	frameAncestors := "frame-ancestors 'none'"
	if embeddableSameOrigin {
		// Same-origin framing only (the session-view page, embedded by the shell's
		// ap:session_view). Cross-origin framing stays blocked; the framed page
		// still runs CheckInteract on every open.
		frameAncestors = "frame-ancestors 'self'"
	}
	parts := []string{
		"default-src 'none'",
		"base-uri 'none'",
		// form-action does not fall back to default-src; restrict native form
		// submissions (POST to /link/submit, /my/accounts/*/revoke, etc.) to
		// same-origin. OAuth "Connect" links are navigations, not form posts,
		// so this does not affect them.
		"form-action 'self'",
		frameAncestors,
		"script-src " + scriptSrc,
		// BOTH sources are required: 'self' permits the external /assets bundle
		// stylesheet (<link>), which 'unsafe-inline' alone does NOT allow, and
		// 'unsafe-inline' covers the runtime inline styles shadcn/Radix inject.
		"style-src " + styleSrc,
		"img-src 'self' data:",
		// 'data:' permits the design bundle's inlined JetBrains Mono variable font
		// (a data:font/woff2 URI; Inter is served from /assets); without it the
		// shell falls back to a system monospace. data: fonts are inert glyph data
		// with no script, so this is a safe relaxation.
		"font-src 'self' data:",
		"connect-src " + connectSrc,
	}
	// ONE frame-src directive only: a browser honours the first and ignores any
	// duplicate, so the sandbox origin and 'self' must be merged into a single
	// source list rather than emitted as two directives.
	var frameSrc []string
	if framesSandbox {
		// The sandbox origin is operator-controlled AT RUNTIME (webd re-reads it
		// from the external-url ConfigMap and stores it verbatim), so it goes
		// through the same SanitizeOrigin as the other sinks that embed it:
		// without it a space widens frame-src to a second host and a ";" splices
		// in a literal extra directive. A value that sanitizes to empty drops the
		// directive rather than emitting a valueless one.
		if origin := SanitizeOrigin(sandboxOrigin); origin != "" {
			frameSrc = append(frameSrc, origin)
		}
	}
	if framesSameOrigin {
		frameSrc = append(frameSrc, "'self'")
	}
	if len(frameSrc) > 0 {
		parts = append(parts, "frame-src "+strings.Join(frameSrc, " "))
	}
	return strings.Join(parts, "; ")
}
