// Package sessionview is the webui.WebUI plugin serving a SESSION-scoped (not
// artifact-scoped) shared live view: a read-only mirror of a session's
// conversation for a subject who can interact with it but was not the
// session's originating channel. It reuses pkg/web/webui/livemirror for both
// the initial history replay and the live NATS stream — the session-agnostic
// building blocks the built-in web chat shares — but gates on CheckInteract
// (agentsession#interact) rather than CheckArtifactView/CheckView, a strict
// superset that would admit platform admins as if they were session
// participants (see deps.go).
//
// The session is named entirely in the path (GET /session-view/{ns}/{name}),
// so there is no signed link parameter to verify: this page needs only the
// authenticated cookie subject (webui.SubjectFromContext) plus CheckInteract.
package sessionview

import (
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

type ui struct{}

// New returns the session-view WebUI.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "session-view" }

func (ui) Routes(deps webui.Deps) []webui.Route {
	d, ok := deps.(Deps)
	if !ok {
		return nil // deps don't carry the session-view surface: fail closed, no routes
	}
	routes := []webui.Route{
		// FramesSandbox lets the trusted shell frame the sandbox-origin
		// /mcpui-host content (see document.go's buildCSP: frame-src <sandbox>).
		// EmbeddableSameOrigin lets THIS page itself be framed, same-origin only,
		// by the session shell's ap:session_view component — CheckInteract still
		// runs on every open, so an embed carries no wider access than a direct
		// visit would.
		{Origin: webui.OriginTrusted, Pattern: "/session-view/{ns}/{name}", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthenticated,
			Page: &webui.Page{App: "session-view", FramesSandbox: true, EmbeddableSameOrigin: true, Build: shellBuild(d)}},
		{Origin: webui.OriginTrusted, Pattern: "/session-view/{ns}/{name}/live", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthenticated, Handler: liveHandler(d)},
		// /mcpui-content + /mcpui-host: sandbox-origin serving for MCP-UI
		// interactive widgets (widgets.go). Both AuthNone: the
		// content-capability token (ct) IS the authorization, mirroring
		// artifactview's /content + /artifact-host.
		{Origin: webui.OriginSandbox, Pattern: "/mcpui-content", Methods: []string{http.MethodGet},
			Auth: webui.AuthNone, Handler: mcpuiContentHandler(d)},
		{Origin: webui.OriginSandbox, Pattern: "/mcpui-host", Methods: []string{http.MethodGet},
			Auth: webui.AuthNone, Handler: mcpuiHostHandler(d)},
	}
	// The mcpui-host renderer bundle (@mcp-ui/client) needs its own
	// sandbox-origin route, since /assets is trusted-origin only. A missing
	// bundle (UI not built) must not panic the server: log loudly and omit the
	// route, so the host page's script tag 404s and the widget never mounts —
	// a visible, diagnosable failure rather than a silent one.
	if hb, err := webui.BundleHandler("mcpui-host"); err != nil {
		d.Logger().Error(err, "sessionview: mcpui-host bundle unavailable; widgets will not render")
	} else {
		routes = append(routes, webui.Route{Origin: webui.OriginSandbox, Pattern: "/mcpui-host.js",
			Methods: []string{http.MethodGet}, Auth: webui.AuthNone, Handler: hb})
	}
	return routes
}

func init() { registry.Register(ui{}) }
