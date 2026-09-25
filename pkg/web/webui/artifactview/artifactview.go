package artifactview

import (
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

type ui struct{}

// New returns the artifact live-view WebUI.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "artifact-live-view" }

func (ui) Routes(deps webui.Deps) []webui.Route {
	av, ok := deps.(Deps)
	if !ok {
		return nil // viewer not configured (nil/unknown deps) → no routes (fail-closed)
	}
	routes := []webui.Route{
		// The shell is the browser entry point: a cookie-less GET kicks off login
		// (302 → identityd /oidc/login) via AuthLoginIfNecessary, and Build runs
		// after auth. FramesSandbox lets the trusted shell frame the
		// sandbox-origin content iframe.
		{Origin: webui.OriginTrusted, Pattern: "/artifact-view", Methods: []string{http.MethodGet},
			Auth: webui.AuthLoginIfNecessary,
			Page: &webui.Page{App: "artifact-view", FramesSandbox: true, Build: shellPageBuild(av)}},
		{Origin: webui.OriginTrusted, Pattern: "/artifact-view/revision", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthenticated, Handler: revisionHandler(av)},
		{Origin: webui.OriginTrusted, Pattern: "/artifact-view/meta", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthenticated, Handler: metaHandler(av)},
		// Direct download: a cookie-less click logs in first, then the handler
		// serves the bytes with Content-Disposition: attachment. Trusted origin is
		// safe only because that disposition forces a download, so untrusted
		// artifact HTML never renders in this origin (see download.go).
		{Origin: webui.OriginTrusted, Pattern: "/artifact-download", Methods: []string{http.MethodGet},
			Auth: webui.AuthLoginIfNecessary, Handler: downloadHandler(av)},
		{Origin: webui.OriginTrusted, Pattern: "/artifact-view/ws", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthenticated, Handler: liveHandler(av)},
		{Origin: webui.OriginSandbox, Pattern: "/content", Methods: []string{http.MethodGet},
			Auth: webui.AuthNone, Handler: contentHandler(av)},
		// /artifacts/a/ serves a same-session SECONDARY artifact's bytes — the
		// target of an `artifact:HANDLE` reference contentHandler rewrote into the
		// primary's HTML. Same shape as /content (sandbox origin, cookie-less,
		// gated entirely by its own capability token), except the token is minted
		// server-side during the rewrite, never by the browser.
		{Origin: webui.OriginSandbox, Pattern: "/artifacts/a/", Methods: []string{http.MethodGet},
			Auth: webui.AuthNone, Handler: assetHandler(av)},
		{Origin: webui.OriginSandbox, Pattern: "/artifact-host", Methods: []string{http.MethodGet},
			Auth: webui.AuthNone, Handler: hostHandler(av)},
	}
	// The annotation renderer bundle needs its own sandbox-origin route (/assets
	// is trusted-origin only — see webui.BundleHandler). A missing bundle must
	// not panic the server: the read-only viewer still works without it, so log
	// loudly and omit the route. The host page's <script src> then 404s and the
	// annotator never mounts — visible and diagnosable rather than silent.
	if hb, err := webui.BundleHandler("artifact-host"); err != nil {
		av.Logger().Error(err, "artifactview: host bundle unavailable; annotator will not load")
	} else {
		routes = append(routes, webui.Route{Origin: webui.OriginSandbox, Pattern: "/artifact-host.js",
			Methods: []string{http.MethodGet}, Auth: webui.AuthNone, Handler: hb})
	}
	return routes
}

func init() { registry.Register(ui{}) }
