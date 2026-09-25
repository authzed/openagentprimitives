package chatembed

import (
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

type ui struct{}

// New returns the chat-embed WebUI.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "chat-embed" }

func (ui) Routes(deps webui.Deps) []webui.Route {
	d, ok := deps.(Deps)
	if !ok {
		return nil // deps do not carry the interact check: fail closed, no routes
	}
	return []webui.Route{
		// EmbeddableSameOrigin: this page exists to be framed by another
		// same-origin page (ap:chat); cross-origin framing stays blocked.
		{Origin: webui.OriginTrusted, Pattern: "/chat-embed/{ns}/{name}", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthenticated,
			Page: &webui.Page{App: "chat-embed", EmbeddableSameOrigin: true, Build: pageBuild(d)}},
	}
}

func init() { registry.Register(ui{}) }
