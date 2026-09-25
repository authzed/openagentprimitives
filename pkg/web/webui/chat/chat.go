// Package chat is the transcript view's data plane: a webui.WebUI plugin
// serving the session-scoped read/write/websocket routes a conversation needs.
// Same "ephemeral Channel + channel-attached AgentSession + outbound relay"
// shape cmd/oap/internal/chatcmd uses for the single-session `oap agent chat`
// TUI, generalized to N concurrent conversations with a websocket sink.
//
// It mounts wherever its three prerequisites are met — NATS, SpiceDB (Authz)
// and the operator memory URL — and nowhere else. ensureRegistry is the ONLY
// gate, it fails closed, and each refusal names the missing prerequisite: a
// plugin that silently does not mount is a 404 with nothing to diagnose from.
// CanHostBrowserSessions answers the same question before Routes runs.
//
// Authorization is per request, not per deployment: every session-scoped route
// is gated on agentsession#interact (the authorize closure in Routes), so a
// viewer reaches exactly the conversations they hold standing on, in whatever
// namespace those live in. Creation is not here at all — it lives behind the
// authorized start routes in pkg/web/webui/sessions and .../agentui.
package chat

import (
	"context"
	"net/http"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

type ui struct{}

// New returns the built-in web chat WebUI.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "chat" }

// regMu guards the process-wide Registry singleton, built lazily on first use
// and reused thereafter. Routes() runs once per real webd process, during
// webui.NewServer's UI-mount loop.
var (
	regMu        sync.Mutex
	regSingleton *Registry
)

func (ui) Routes(deps webui.Deps) []webui.Route {
	d, ok := deps.(Deps)
	if !ok {
		return nil // deps don't carry the chat surface at all: fail closed, and there is nothing here to log through
	}
	reg, ok := ensureRegistry(d)
	if !ok {
		return nil // prerequisites unmet; ensureRegistry already logged why
	}

	// authorize is the shared route-level gate for every session-scoped route
	// below: agentsession#interact on the URL's {ns}/{name}, built once and
	// reused across all six. A SpiceDB ERROR must never read as a denial —
	// hence the typed 503 *PageError rather than a bare error, which
	// webui.Server.renderAuthorizeFailure would collapse into a 403. It runs
	// BEFORE any handler body, so an unauthorized or indeterminate caller
	// never reaches a Kubernetes read.
	authorize := func(ctx context.Context, subject string, r *http.Request) error {
		ns, name := r.PathValue("ns"), r.PathValue("name")
		ok, err := d.CheckInteract(ctx, ns, name, subject)
		if err != nil {
			d.Logger().Info("chat: CheckInteract errored", "ns", ns, "name", name, "subject", subject, "err", err.Error())
			return &webui.PageError{Status: http.StatusServiceUnavailable, Kind: "error",
				Title: "Authorization unavailable", Message: "Could not verify access to this session. Try again."}
		}
		if !ok {
			return &webui.PageError{Status: http.StatusForbidden, Kind: "forbidden",
				Title: "Access denied", Message: "You do not have access to this session."}
		}
		return nil
	}

	return []webui.Route{
		{Origin: webui.OriginTrusted, Pattern: "/sessions/api/{ns}/{name}/detail", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize, Handler: sessionDetailHandler(d, reg)},
		{Origin: webui.OriginTrusted, Pattern: "/sessions/api/{ns}/{name}/messages", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize, Handler: sessionMessagesHandler(d, reg)},
		{Origin: webui.OriginTrusted, Pattern: "/sessions/api/{ns}/{name}/message", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthorized, Authorize: authorize, Handler: messageHandler(reg)},
		{Origin: webui.OriginTrusted, Pattern: "/sessions/api/{ns}/{name}/interrupt", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthorized, Authorize: authorize, Handler: interruptHandler(reg)},
		{Origin: webui.OriginTrusted, Pattern: "/sessions/api/{ns}/{name}/decision", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthorized, Authorize: authorize, Handler: decisionHandler(reg)},
		{Origin: webui.OriginTrusted, Pattern: "/sessions/api/{ns}/{name}/ws", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize, Handler: wsHandler(d, reg)},
	}
}

// CanHostBrowserSessions reports whether this webd can carry a browser
// session's outbound traffic: a browser Channel is not relayed by channelsd,
// so its Sender and StreamDeltaSink come from browser.NewHost, built here.
// Exported so internal/cmd/webd can decide whether to offer a start control at all
// without re-deriving the prerequisite list — a second copy is how a webd
// offers a start whose replies have nowhere to go.
func CanHostBrowserSessions(d Deps) bool { return unmetPrerequisite(d) == "" }

// unmetPrerequisite names the first prerequisite this Deps does not meet, as
// the log line an operator needs, or "" when all three are met. ONE list, two
// consumers (CanHostBrowserSessions and ensureRegistry), so the predicate and
// the diagnosis can never disagree.
func unmetPrerequisite(d Deps) string {
	switch {
	case d.NATS() == nil:
		return "chat: NATS is not configured; the transcript data plane is disabled"
	case d.Authz() == nil:
		return "chat: SpiceDB (Authz) is not configured; the transcript data plane is disabled"
	case d.OperatorURL() == "":
		return "chat: operator memory URL is not configured; the transcript data plane is disabled"
	}
	return ""
}

// ensureRegistry builds the process-wide Registry on first call, or returns
// the existing one. Every failure reason is logged via d.Logger() — no silent
// breakage: this is the ONLY gate on the plugin, so an operator whose chat
// routes did not mount has nothing else to read.
func ensureRegistry(d Deps) (*Registry, bool) {
	regMu.Lock()
	defer regMu.Unlock()
	if regSingleton != nil {
		return regSingleton, true
	}
	if unmet := unmetPrerequisite(d); unmet != "" {
		d.Logger().Info(unmet)
		return nil, false
	}
	reg := newRegistry(d)
	reg.startReaper()
	// The relay must be up before any session's outbound envelopes have
	// anywhere to go, so a failure here (the shared NATS conn already
	// draining, say) fails the whole plugin closed rather than silently
	// accepting sessions with no relay.
	if err := reg.startRelay(); err != nil {
		d.Logger().Info("chat: start outbound relay failed; the transcript data plane is disabled", "err", err.Error())
		reg.shutdown(context.Background())
		return nil, false
	}
	regSingleton = reg
	return reg, true
}

// CurrentRegistry returns this process's live-session table, or nil when the
// chat plugin never mounted (a webd whose NATS / SpiceDB / operator-URL
// prerequisites are unconfigured — see ensureRegistry).
//
// Read LAZILY, per request, by the surfaces that hand a just-created session
// to the table: the singleton is built during the webui server's UI-mount
// loop, which runs AFTER webd assembles its deps umbrella, so a value captured
// at construction time is always nil. Returns a nil *Registry, never a
// nil-wrapping interface, so the caller's nil check stays honest.
func CurrentRegistry() *Registry {
	regMu.Lock()
	defer regMu.Unlock()
	return regSingleton
}

// Shutdown tears down every live chat session's IN-PROCESS state (relay,
// watchers, sinks); the AgentSession and Channel CRs survive, as everywhere
// else in this package. A nil-safe no-op when chat never mounted. internal/cmd/webd
// calls it once during graceful shutdown, before draining the shared NATS
// connection the relay depends on.
func Shutdown(ctx context.Context) {
	regMu.Lock()
	reg := regSingleton
	regSingleton = nil
	regMu.Unlock()
	if reg != nil {
		reg.shutdown(ctx)
	}
}

// ResetForTest tears down (if present) and clears the Registry singleton so
// each test gets an isolated chat plugin. Test-only.
func ResetForTest() {
	regMu.Lock()
	reg := regSingleton
	regSingleton = nil
	regMu.Unlock()
	if reg != nil {
		reg.shutdown(context.Background())
	}
}

func init() { registry.Register(ui{}) }
