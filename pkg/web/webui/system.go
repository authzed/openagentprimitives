package webui

import (
	"fmt"
	"html"
	"net/http"
)

// renderSystemPage renders a styled error/info page: the built-in `system` React
// app mounted with the PageError as props, plus a server-rendered styled fallback
// inside #root so the page is correctly themed even before/without JS. Sets the
// PageError's HTTP status. CSP is the strict base (no sandbox framing).
func renderSystemPage(w http.ResponseWriter, mf manifest, nonce string, dev devConfig, pe *PageError) {
	entry := mf["system"]
	props := systemProps{Status: pe.Status, Kind: pe.Kind, Title: pe.Title, Message: pe.Message}
	// renderDocument owns header→status→body ordering (it sets Content-Type + CSP
	// headers before WriteHeader(Status)); do NOT set them here.
	_ = renderDocument(w, docInput{
		App:      "system",
		Title:    pe.Title,
		Props:    props,
		Scripts:  entry.Scripts,
		CSS:      entry.CSS,
		Nonce:    nonce,
		CSP:      buildCSP(nonce, false, "", dev, false, false),
		Fallback: systemFallback(pe),
		Dev:      dev,
		Status:   pe.Status,
	})
}

// systemFallback is the no-JS styled markup for a system page (matches the
// criticalCSS classes). React replaces it on mount with the shadcn version.
func systemFallback(pe *PageError) string {
	return fmt.Sprintf(
		`<div class="ap-fallback"><div class="card"><h1>%s</h1><p>%s</p><p style="opacity:.6;font-size:.75rem">Status %d</p></div></div>`,
		html.EscapeString(pe.Title), html.EscapeString(pe.Message), pe.Status)
}
