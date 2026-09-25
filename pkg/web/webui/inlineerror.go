package webui

import (
	"fmt"
	"html"
	"net/http"
)

// RenderInlineError writes a small, fully self-contained styled error page with
// NO external assets — for the cookieless sandbox origin (which does not serve
// /assets) and for any context where a JS-mounted system page is inappropriate
// (e.g. inside the artifact content iframe). The design colors are inlined via
// criticalCSS so it is themed without the design stylesheet. Sets the status.
func RenderInlineError(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// This page is inline <style> + HTML-escaped text only (no scripts, no
	// external assets), so lock everything else down. NO frame-ancestors:
	// unlike renderDocument's pages, this one is served inside the artifact
	// content iframe and must remain embeddable by the shell.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">`+
		`<meta name="viewport" content="width=device-width, initial-scale=1"><title>%s</title>`+
		`<style>%s</style></head><body>`+
		`<div class="ap-fallback"><div class="card"><h1>%s</h1><p>%s</p>`+
		`<p style="opacity:.6;font-size:.75rem">Status %d</p></div></div>`+
		`</body></html>`,
		html.EscapeString(title), criticalCSS,
		html.EscapeString(title), html.EscapeString(message), status)
}
