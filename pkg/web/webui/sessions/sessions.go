// Package sessions is the webui.WebUI plugin serving the per-subject session
// dashboard: a single page, GET /sessions, listing every AgentSession the
// viewer holds interact on and offering the session-scoped chat/agent-UI views
// plus a start control for classes the viewer already interacts with. It is
// the only page that enumerates sessions across the viewer's own standing —
// pkg/web/webui/chat has no list route (it serves this page's transcript data
// plane, one already-named session per request), and pkg/web/webui/agentui and
// pkg/web/webui/sessionview each serve one already-named session too.
//
// This file wires the plugin skeleton, its Deps cast, and its routes; deps.go
// declares the dependency surface and page.go builds the page's props.
// Modeled on pkg/web/webui/agentui: same New/Name/Routes shape, same
// fail-closed deps.(Deps) cast, same registry.Register in init().
package sessions

import (
	"errors"
	"net/http"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

type ui struct{}

// New returns the session-dashboard WebUI plugin.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "sessions" }

// errDepsCastFailed is the error value passed to Logger().Error for a cast
// failure below. logr's Error signature wants a real error even though the
// failure is a type mismatch, not an I/O fault; a sentinel keeps the log
// line's shape consistent with every other Logger().Error in this codebase.
var errDepsCastFailed = errors.New("sessions: deps does not implement sessions.Deps")

// hasLogger is checked when the deps.(Deps) cast below fails, so the failure
// can still be logged. It fires for a reachable case: with the artifact
// viewer's prerequisites (SpiceDB, memory token, operator URL) unconfigured,
// internal/cmd/webd's buildArtifactViewDeps degrades to the identity-only *webdDeps
// umbrella, which carries K8s and Logger() but not
// LookupInteractableSessions/CheckInteract. A logged nil is a diagnosable
// misconfiguration; a silent one is the defect.
type hasLogger interface{ Logger() logr.Logger }

func (ui) Routes(deps webui.Deps) []webui.Route {
	d, ok := deps.(Deps)
	if !ok {
		if lg, ok2 := deps.(hasLogger); ok2 {
			lg.Logger().Error(errDepsCastFailed,
				"sessions: deps cast failed; routes not registered (fail closed) — GET /sessions will 401")
		}
		return nil // deps don't carry the sessions surface: fail closed, no routes
	}
	routes := []webui.Route{
		// The selection is a query parameter (?session=<ns>/<name>&view=), not a
		// path segment: a /sessions/{ns}/{name} shape would collide with the API
		// routes below, where ServeMux prefers the literal segment — so a
		// namespace actually named "api" would become unaddressable.
		//
		// Authorize is nil: every authenticated subject may open this page and
		// see their own — possibly empty — list. The list itself is the
		// authorization boundary (LookupInteractableSessions' per-subject
		// filter); there is no narrower permission to gate the page behind.
		{Origin: webui.OriginTrusted, Pattern: "/sessions", Methods: []string{http.MethodGet},
			Auth: webui.AuthLoginIfNecessary,
			// FramesSameOrigin lets this shell frame the same-origin
			// /session-view/{ns}/{name} page (ap:session_view).
			Page: &webui.Page{App: "sessions", FramesSameOrigin: true, Build: shellPageBuild(d)}},
	}
	// More specific than chat's /sessions/api/{ns}/{name}/… wildcards, so
	// ServeMux dispatches here.
	//
	// AuthAuthenticated rather than AuthAuthorized: no object in the path for a
	// route-level Authorize to gate, so the authorization is buildSessionList's
	// per-subject LookupInteractableSessions filter (list.go) — the same
	// boundary the page route relies on, from the same call shellPageBuild's
	// first paint makes, so render and poll cannot disagree about "the list".
	routes = append(routes, webui.Route{
		Origin: webui.OriginTrusted, Pattern: "/sessions/api/sessions", Methods: []string{http.MethodGet},
		Auth: webui.AuthAuthenticated, Handler: sessionsAPIHandler(d),
	})
	// POST /sessions/api/start starts a session for a class the viewer already
	// interacts with. It mounts only when d.StartBrowserSession() is non-nil —
	// the same collaborator-presence gate pkg/web/webui/agentui's own start
	// route uses — so a webd that cannot host a browser session serves no route
	// that would create one it could not deliver to.
	//
	// AuthAuthenticated, not AuthAuthorized: like the list endpoint, it carries
	// no object in its path to gate on. Its authorization is the
	// fully-consistent (namespace, class) recomputation the handler runs
	// against the BODY's named pair (start.go), which a path-shaped Authorize
	// could not express.
	if d.StartBrowserSession() != nil {
		routes = append(routes, webui.Route{
			Origin: webui.OriginTrusted, Pattern: "/sessions/api/start", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthenticated, Handler: startHandler(d),
		})
	}
	return routes
}

func init() { registry.Register(ui{}) }
