package webui

import (
	"fmt"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/web/webui/webassets"
)

// RenderStandaloneApp serves the shared HTML document for a built webassets
// app entry from a host that is not a webui Server — today the desktop
// settings server. It reuses renderDocument/buildCSP verbatim (the same
// document shell serveRoute uses) rather than duplicating them, so a host
// that only needs one page doesn't have to construct a full Server and pull
// in webd's dependency graph to get it.
//
// Production assets only (no dev mode), a strict per-request nonce CSP, and
// no framing (frame-ancestors 'none', X-Frame-Options DENY) — the same
// defaults serveRoute applies to a Page that opts into neither FramesSandbox
// nor EmbeddableSameOrigin/FramesSameOrigin.
func RenderStandaloneApp(w http.ResponseWriter, app, title string, props any) error {
	m, err := webassets.Manifest()
	if err != nil {
		return fmt.Errorf("webui: load web asset manifest: %w", err)
	}
	entry, ok := m[app]
	if !ok {
		return fmt.Errorf("webui: app %q is not a built webassets entry", app)
	}
	nonce, err := newNonce()
	if err != nil {
		return fmt.Errorf("webui: mint CSP nonce: %w", err)
	}
	return renderDocument(w, docInput{
		App:     app,
		Title:   title,
		Props:   props,
		Scripts: entry.Scripts,
		CSS:     entry.CSS,
		Nonce:   nonce,
		CSP:     buildCSP(nonce, false, "", devConfig{}, false, false),
	})
}
