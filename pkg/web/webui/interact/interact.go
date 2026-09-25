// Package interact is the webui.WebUI plugin serving the browser's authorized
// path to act on a session it can view: POST /session/{ns}/{name}/interact
// dispatches a decoded interaction kind (pkg/channels/interact) through
// layered, fail-closed gates, and GET /session/{ns}/{name}/interactions reports
// which kinds this class permits (gate order in handlers.go).
//
// POST /session/{ns}/{name}/app-tool-call is a sibling route relaying an MCP-UI
// widget's tool call to the runner's synchronous app_tool_call responder
// through the same auth/CSRF/CheckInteract gates (apptoolcall.go).
package interact

import (
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

type ui struct{}

// New returns the /interact WebUI plugin.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "interact" }

func (ui) Routes(deps webui.Deps) []webui.Route {
	d, ok := deps.(Deps)
	if !ok {
		return nil // deps don't carry the interact surface: fail closed, no routes
	}
	return []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/session/{ns}/{name}/interactions", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthenticated, Handler: interactionsHandler(d)},
		{Origin: webui.OriginTrusted, Pattern: "/session/{ns}/{name}/interact", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthenticated, Handler: interactHandler(d)},
		{Origin: webui.OriginTrusted, Pattern: "/session/{ns}/{name}/app-tool-call", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthenticated, Handler: appToolCallHandler(d)},
	}
}

func init() { registry.Register(ui{}) }
