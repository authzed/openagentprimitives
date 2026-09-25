// Package sessionview: sandbox-origin serving for MCP-UI interactive widgets.
// Two routes, mirroring pkg/web/webui/artifactview's /content + /artifact-host
// pattern:
//
//   - GET /mcpui-content serves a persisted widget's raw HTML VERBATIM — no
//     sanitize, no ServeTransform, because the widget IS supposed to execute
//     its own script (pkg/channels/channelassets/mcpui). Independently tokened
//     and independently CSP'd, for direct/standalone loads; the primary render
//     path does not navigate an iframe to it (see mcpuiHostPage).
//   - GET /mcpui-host mounts the mcpui-host React bundle, which renders the
//     widget via @mcp-ui/client's UIResourceRenderer into an iframe
//     sandbox="allow-scripts" WITHOUT allow-same-origin — the OPPOSITE
//     isolation posture from artifactview's /artifact-host, whose inner frame
//     is allow-same-origin WITHOUT allow-scripts because that renderer's
//     output is always script-inert. Here the widget's author JS runs, but the
//     frame has no origin identity to escalate with: a unique opaque origin,
//     distinct from both the sandbox host and the trusted shell. See
//     ui/mcpuihost/McpUiHost.tsx for the library-mode proof (srcDoc mode ->
//     sandbox="allow-scripts"; its "src" mode unconditionally adds
//     allow-same-origin and is never used here).
//
// Both routes are gated by the SAME content-capability token kind
// (SignWidgetToken/VerifyWidgetToken, Kind=widget), verified independently.
//
// Both also carry the identical Content-Security-Policy header
// (buildWidgetCSP), because a CSP header only governs the document that
// returned it — never one it frames. The exception is what makes /mcpui-host's
// header load-bearing: a `srcDoc` child with no CSP of its own inherits its
// creator's (CSP3), so that header becomes the widget's EFFECTIVE policy.
// /mcpui-content needs the same restrictions on its own response for a direct
// load. The pair also solves the frame-ancestors chain: two levels deep
// (trusted shell -> /mcpui-host -> /mcpui-content), CSP3 requires EVERY
// ancestor to match, so both declare "'self' <shellOrigin>" (buildWidgetCSP).
package sessionview

import (
	"net/http"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// buildWidgetCSP returns the Content-Security-Policy value shared by
// /mcpui-content (where the widget's untrusted script executes) and
// /mcpui-host (the framing document). meta is the widget's `_meta.ui.csp`, nil
// when absent, which falls back to the restrictive default: the widget's own
// inline/external script may run (we never sanitize or nonce its markup, so a
// nonce-based policy would simply block it) but it reaches no network and
// loads no resource beyond same-origin/data: URIs.
//
// shellOrigin is the trusted session-view page's origin, needed regardless of
// meta because both routes sit two levels deep (trusted shell -> /mcpui-host
// -> /mcpui-content) and CSP3 requires frame-ancestors to match EVERY ancestor
// in the chain, not just the immediate parent. 'self' alone would satisfy
// /mcpui-content's immediate parent (/mcpui-host, same origin) and reject the
// outer shell; listing both satisfies both routes with one shared value.
func buildWidgetCSP(meta *WidgetCSPMeta, shellOrigin string) string {
	shellOrigin = webui.SanitizeOrigin(shellOrigin)

	var parts []string
	if meta == nil {
		parts = []string{
			"default-src 'none'",
			"connect-src 'none'",
			"script-src 'self' 'unsafe-inline'",
			"img-src 'self' data:",
		}
	} else {
		// meta's domain lists are MCP-server-authored (untrusted): a token
		// carrying a `;` would splice an extra directive into this header if
		// joined verbatim. sanitizeDomains drops any token
		// SanitizeCSPSourceToken rejects, keeping legitimate entries including
		// wildcard sources, which it deliberately does not url.Parse away.
		connectDomains := sanitizeDomains(meta.ConnectDomains)
		resourceDomains := sanitizeDomains(meta.ResourceDomains)

		connect := "'none'"
		if len(connectDomains) > 0 {
			connect = strings.Join(connectDomains, " ")
		}
		// ResourceDomains maps onto img-src, style-src, AND script-src — a
		// widget declares the domains it loads scripts/styles/images from once,
		// under one name, rather than three separate lists.
		resourceSrc := func(base ...string) string {
			return strings.Join(append(append([]string{}, base...), resourceDomains...), " ")
		}
		parts = []string{
			"default-src 'none'",
			"connect-src " + connect,
			"script-src " + resourceSrc("'self'", "'unsafe-inline'"),
			"img-src " + resourceSrc("'self'", "data:"),
			"style-src " + resourceSrc("'self'", "'unsafe-inline'"),
		}
	}

	frameSrc := "'self'"
	if meta != nil && len(meta.FrameDomains) > 0 {
		if frameDomains := sanitizeDomains(meta.FrameDomains); len(frameDomains) > 0 {
			frameSrc = frameSrc + " " + strings.Join(frameDomains, " ")
		}
	}
	parts = append(parts,
		"frame-src "+frameSrc,
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'self' "+shellOrigin,
	)
	return strings.Join(parts, "; ")
}

// sanitizeDomains filters a widget-declared (MCP-server-authored, untrusted)
// CSP domain list through webui.SanitizeCSPSourceToken, dropping any token
// that fails the check rather than letting it reach strings.Join and splice
// into the directive value.
func sanitizeDomains(domains []string) []string {
	clean := make([]string, 0, len(domains))
	for _, d := range domains {
		if s, ok := webui.SanitizeCSPSourceToken(d); ok {
			clean = append(clean, s)
		}
	}
	return clean
}

// mcpUiHostBootstrap is the bootstrap JSON the mcpui-host React bundle reads
// from the oap-bootstrap script element — the same id and reading convention
// @ap/runtime's readBootstrap uses for trusted "Page" routes. This handler
// cannot use that helper: it needs buildWidgetCSP rather than the framework's
// nonce-based buildCSP, so it hand-rolls the document as artifactview's
// hostPage does.
type mcpUiHostBootstrap struct {
	// URI is a synthetic per-widget identifier (mcp-ui/MCP-Apps convention);
	// any stable unique string suffices, nothing dereferences it.
	URI string `json:"uri"`
	// HTML is the widget's raw, unsanitized bytes.
	HTML string `json:"html"`
	// TrustedOrigin is the sandbox->shell postMessage target origin, stamped as
	// postMessage's second argument (never "*") when relaying a widget action.
	TrustedOrigin string `json:"trustedOrigin"`
}

// mcpuiHostPage builds the sandbox-origin host document for one widget: a bare
// mount point plus the mcpui-host React bundle, which renders the widget via
// @mcp-ui/client's UIResourceRenderer (ui/mcpuihost/McpUiHost.tsx).
// bootstrapJSON is the ALREADY html-escaped output of webui.MarshalBootstrap,
// embedded as-is exactly as pkg/web/webui/document.go's renderDocument does —
// that escaping is what lets a widget's HTML, which may contain a literal
// closing script tag, travel inside the JSON script element without breaking
// out of it.
//
// The widget's bytes are embedded in THIS response's bootstrap JSON (fetched
// server-side, once, by mcpuiHostHandler) rather than fetched by a second
// browser-side navigation to /mcpui-content. That keeps this response's own
// CSP the ONLY policy governing the widget's execution context, since a
// `srcDoc` iframe with no CSP of its own inherits its creator's (CSP3).
// Fetching /mcpui-content client-side would instead need this document's
// connect-src to permit it — and the widget inherits that same connect-src, so
// the grant would reach the untrusted widget too, undoing buildWidgetCSP's
// restrictive default.
func mcpuiHostPage(bootstrapJSON string) []byte {
	doc := `<!doctype html><html><head><meta charset="utf-8">` +
		`<style>html,body{margin:0;height:100%}#root{width:100%;height:100%;display:block}</style>` +
		`</head><body>` +
		`<div id="root"></div>` +
		`<script type="application/json" id="ap-bootstrap">` + bootstrapJSON + `</script>` +
		`<script type="module" src="/mcpui-host.js"></script>` +
		`</body></html>`
	return []byte(doc)
}

// mcpuiContentHandler serves a persisted MCP-UI widget's raw HTML bytes
// verbatim, gated by a content-capability token. No ServeTransform, no
// artifact-ref rewriting: the bytes reach the browser byte-for-byte, and
// safety comes entirely from the sandbox framing plus this response's own CSP
// (see the package doc), never from transforming the bytes.
func mcpuiContentHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns, sess, artifactID, err := d.VerifyWidgetToken(r.URL.Query().Get("ct"))
		if err != nil {
			webui.RenderInlineError(w, http.StatusForbidden, "Link expired",
				"This widget link is invalid or has expired. Reopen the session view to continue.")
			return
		}
		out, meta, err := d.FetchWidget(r.Context(), ns, sess, artifactID)
		if err != nil {
			d.Logger().Error(err, "sessionview mcpui-content: FetchWidget failed",
				"ns", ns, "sess", sess, "artifactID", artifactID)
			webui.RenderInlineError(w, http.StatusBadGateway, "Could not load widget",
				"This widget is temporarily unavailable. Please retry.")
			return
		}
		// mcpui is a pass-through renderer whose only declared output MIME is
		// text/html, so the content type is fixed here rather than threaded
		// through FetchWidget.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// This document is where the widget's own untrusted script executes, so
		// the CSP belongs here and not only on /mcpui-host (package doc). A nil
		// meta.CSP falls back to buildWidgetCSP's restrictive default.
		w.Header().Set("Content-Security-Policy", buildWidgetCSP(meta.CSP, d.TrustedOrigin()))
		_, _ = w.Write(out)
	})
}

// mcpuiHostHandler serves the sandbox-origin host page for a widget: verify
// the content-capability token, fetch the widget's bytes (the same call
// mcpuiContentHandler makes — mcpuiHostPage's doc says why they are embedded
// here rather than fetched client-side), and mount the mcpui-host React bundle
// with them as bootstrap props. AuthNone: the capability token IS the
// authorization.
func mcpuiHostHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.URL.Query().Get("ct")
		ns, sess, artifactID, err := d.VerifyWidgetToken(ct)
		if err != nil {
			webui.RenderInlineError(w, http.StatusForbidden, "Link expired",
				"This widget link is invalid or has expired. Reopen the session view to continue.")
			return
		}
		out, meta, err := d.FetchWidget(r.Context(), ns, sess, artifactID)
		if err != nil {
			d.Logger().Error(err, "sessionview mcpui-host: FetchWidget failed",
				"ns", ns, "sess", sess, "artifactID", artifactID)
			webui.RenderInlineError(w, http.StatusBadGateway, "Could not load widget",
				"This widget is temporarily unavailable. Please retry.")
			return
		}
		// meta.CSP carries the widget's declared `_meta.ui.csp` when the runner
		// parsed one at ingestion, nil otherwise; buildWidgetCSP implements the
		// connect/resource/frame domain mapping for both cases.
		bootstrap, err := webui.MarshalBootstrap(mcpUiHostBootstrap{
			URI:  "ui://widget/" + artifactID,
			HTML: string(out),
			// Sanitized the same way buildWidgetCSP's shellOrigin is — a bare
			// "scheme://host", never a raw value — because this travels to the
			// client as postMessage's targetOrigin.
			TrustedOrigin: webui.SanitizeOrigin(d.TrustedOrigin()),
		})
		if err != nil {
			d.Logger().Error(err, "sessionview mcpui-host: bootstrap marshal failed",
				"ns", ns, "sess", sess, "artifactID", artifactID)
			webui.RenderInlineError(w, http.StatusInternalServerError, "Could not load widget",
				"The widget could not be prepared for display. Please retry.")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", buildWidgetCSP(meta.CSP, d.TrustedOrigin()))
		_, _ = w.Write(mcpuiHostPage(bootstrap))
	})
}
