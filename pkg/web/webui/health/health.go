// Package health is a trivial built-in WebUI: a public GET /healthz on the
// trusted origin. The k8s probes actually hit the host-agnostic /healthz
// short-circuit in Server.ServeHTTP, which runs before Host dispatch and shadows
// this route; the registration keeps a real UI mounted in webd and so proves the
// registry → server wiring accepts one.
package health

import (
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

type ui struct{}

// New returns the health WebUI.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "health" }

func (ui) Routes(webui.Deps) []webui.Route {
	return []webui.Route{{
		Origin:  webui.OriginTrusted,
		Pattern: "/healthz",
		Methods: []string{http.MethodGet},
		Auth:    webui.AuthNone,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }),
	}}
}

func init() { registry.Register(ui{}) }
