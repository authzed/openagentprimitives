// Package agentui backs the agent-defined view of a session: the resolution
// ladder that turns an AgentSession into a validated declaration, and the
// three session-scoped API routes a browser drives that view through
// (bindings, actions, live) plus the replacement-session start route.
//
// It serves no page of its own. The view is rendered inside the session
// shell's content region (pkg/web/webui/sessions), which owns chrome; the only
// GET here is /agent-ui/{ns}/{name}, a redirect into that shell kept so every
// address that ever worked still resolves (redirect.go).
//
// This file wires the plugin skeleton, its Deps cast, and its routes; the
// resolution ladder lives in session.go and page.go, and the merged view a
// route serves in viewmodel.go.
package agentui

import (
	"errors"
	"net/http"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

type ui struct{}

// New returns the agent-UI WebUI plugin.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "agent-ui" }

// errDepsCastFailed gives logr.Error a real error for what is actually a type
// mismatch, keeping the log line's shape consistent with the rest of the
// codebase.
var errDepsCastFailed = errors.New("agentui: deps does not implement agentui.Deps")

// hasLogger lets a FAILED deps.(Deps) cast still be logged. The degraded,
// identity-only umbrella internal/cmd/webd builds when SpiceDB / memory token /
// operator URL are unconfigured carries K8s but not CheckInteract, so the cast
// fails in exactly that case. A logged nil is a diagnosable misconfiguration;
// a silent one is the defect.
type hasLogger interface{ Logger() logr.Logger }

func (ui) Routes(deps webui.Deps) []webui.Route {
	d, ok := deps.(Deps)
	if !ok {
		if lg, ok2 := deps.(hasLogger); ok2 {
			lg.Logger().Error(errDepsCastFailed,
				"agentui: deps cast failed; routes not registered (fail closed) — GET /agent-ui/{ns}/{name} will 404")
		}
		return nil // deps don't carry the agent-ui surface: fail closed, no routes
	}
	routes := []webui.Route{
		// GET /agent-ui/{ns}/{name} REDIRECTS into the session shell — one page
		// per session, and this address resolves to it. See redirectHandler
		// for why it holds no authorization and performs no lookup.
		//
		// {name} is an AgentSession name, NEVER an AgentClass name:
		// CheckInteract's SpiceDB resource type is hardcoded to "agentsession"
		// and a UI is always session-scoped.
		{Origin: webui.OriginTrusted, Pattern: "/agent-ui/{ns}/{name}", Methods: []string{http.MethodGet},
			Auth: webui.AuthNone, Handler: redirectHandler()},
		// POST .../bindings resolves every data binding in the SAME session's
		// declaration server-side, under the viewer's own subject. See
		// gateAgentUIPost (bindings.go) for the gate order every POST here
		// shares; the GET above contributes nothing — a redirect gates
		// nothing.
		{Origin: webui.OriginTrusted, Pattern: "/agent-ui/{ns}/{name}/bindings", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthenticated, Handler: bindingsHandler(d)},
		// POST .../actions INVOKES one declared action. Same gateAgentUIPost
		// preamble as .../bindings, verbatim; the difference is that this one
		// mutates, so its outcome is recorded and pushed rather than returned
		// once. See actions.go for the gate order.
		{Origin: webui.OriginTrusted, Pattern: "/agent-ui/{ns}/{name}/actions", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthenticated, Handler: actionsHandler(d)},
		// GET .../live is the lifecycle's delivery path: a snapshot read from
		// the ui_action memory records at open, then ui_action_update
		// envelopes relayed as they arrive. Memory is authoritative and NATS
		// is the low-latency copy, so a reconnecting browser and a live one
		// converge — see live.go's runActionMirror.
		{Origin: webui.OriginTrusted, Pattern: "/agent-ui/{ns}/{name}/live", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthenticated, Handler: liveHandler(d)},
	}
	// POST .../start starts a REPLACEMENT session for the one the URL names,
	// once that session has ended — an explicit action, never a side effect of
	// opening the view. Mounted only when StartBrowserSession is non-nil:
	// creating a session this webd cannot host produces one whose replies have
	// nowhere to go. Gated on the collaborator's presence rather than a
	// cluster-shape flag, keeping the cluster-kind branch in internal/cmd/webd where
	// the fact is actually known.
	if d.StartBrowserSession() != nil {
		routes = append(routes,
			webui.Route{Origin: webui.OriginTrusted, Pattern: "/agent-ui/{ns}/{name}/start", Methods: []string{http.MethodPost},
				Auth: webui.AuthAuthenticated, Handler: startHandler(d)})
	} else {
		d.Logger().Info("agentui: StartBrowserSession is nil; POST /agent-ui/{ns}/{name}/start not mounted (this webd cannot host a browser session's live output)")
	}
	return routes
}

func init() { registry.Register(ui{}) }
