package webui

import (
	"context"
	"net/http"
)

// AppRenderer lets a raw http.Handler render a React document (or a styled
// system error) imperatively — for handlers whose own control flow (custom
// auth, OIDC redirects, etc.) means they can't be declarative Pages. The Server
// implements it and injects itself into every request's context; retrieve it
// with RendererFromContext(r.Context()).
type AppRenderer interface {
	// RenderApp renders the shared document mounting the React app `app` with
	// `props`. framesSandbox adds frame-src <sandbox origin> to the CSP.
	RenderApp(w http.ResponseWriter, app string, props any, meta PageMeta, framesSandbox bool) error
	// RenderError renders a styled system page with the PageError's status.
	RenderError(w http.ResponseWriter, pe *PageError)
}

// RenderApp implements AppRenderer. It mirrors the Page-route render path
// (serveRoute) but is callable from any handler that already has its props.
func (s *Server) RenderApp(w http.ResponseWriter, app string, props any, meta PageMeta, framesSandbox bool) error {
	nonce, err := newNonce()
	if err != nil {
		return err
	}
	entry := s.manifest[app]
	return renderDocument(w, docInput{
		App:     app,
		Title:   meta.Title,
		Props:   props,
		Scripts: entry.Scripts,
		CSS:     entry.CSS,
		Nonce:   nonce,
		CSP:     buildCSP(nonce, framesSandbox, s.sandboxHost(), s.dev, false, false),
		Dev:     s.dev,
	})
}

// RenderError implements AppRenderer.
func (s *Server) RenderError(w http.ResponseWriter, pe *PageError) { s.renderSystem(w, pe) }

type rendererKeyT struct{}

var rendererKey rendererKeyT

// withRenderer carries the AppRenderer on ctx (Server.ServeHTTP injects it).
func withRenderer(ctx context.Context, r AppRenderer) context.Context {
	return context.WithValue(ctx, rendererKey, r)
}

// RendererFromContext returns the AppRenderer the framework injected, or nil if
// the request didn't pass through Server.ServeHTTP.
func RendererFromContext(ctx context.Context) AppRenderer {
	r, _ := ctx.Value(rendererKey).(AppRenderer)
	return r
}
