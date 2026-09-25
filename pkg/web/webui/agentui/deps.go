package agentui

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
)

// Deps is this plugin's dependency interface. internal/cmd/webd's concrete deps value
// implements it; the WebUI casts webui.Deps to this in Routes(). A cast
// failure fails the agent-ui routes CLOSED, and Routes logs loudly through
// whatever the failed value can still supply — a silent nil here is a 404 with
// nothing in the logs, which is exactly how it once shipped.
//
// The realistic failure is internal/cmd/webd's degraded, identity-only umbrella (used
// when SpiceDB / memory token / operator URL are unconfigured): it carries K8s
// but not CheckInteract, so the cast fails even though K8s alone satisfies
// part of this interface.
//
// ONE cast covers every route, so an umbrella missing ANY route's collaborator
// fails ALL of them closed together — there is no partial-Deps state where one
// route serves while its siblings silently 404. The only route needing nothing
// from this interface is GET /agent-ui/{ns}/{name}, a redirect that reads
// nothing (redirect.go).
type Deps interface {
	// CheckInteract is the ONLY authorization gate this plugin's session-scoped
	// routes use — the same agentsession#interact check pkg/web/webui/sessionview
	// and .../interact gate a session-scoped view/send on. NEVER
	// CheckArtifactView/CheckView: those are parent->interact +
	// platform->view_audit, a strict superset that would admit platform admins
	// into a session-scoped agent UI as if they were participants in it.
	CheckInteract(ctx context.Context, ns, name, subject string) (bool, error)
	// K8s Gets the AgentSession/AgentClass/AgentUI CRs the resolution ladder
	// (session.go, viewmodel.go) walks. Same method name and return type as
	// pkg/web/webui/chat.Deps.K8s, so internal/cmd/webd satisfies both with one
	// accessor. Not sufficient on its own — the degraded umbrella carries K8s
	// and still fails the cast on CheckInteract.
	K8s() client.Client
	// Logger backs the fail-closed cast-failure log in Routes below, and every
	// other fail-closed log line this package's handlers write.
	Logger() logr.Logger

	// --- POST /agent-ui/{ns}/{name}/bindings (bindings.go) -----------------
	//
	// These five let bindings.go pin the POST's Origin header and satisfy
	// pkg/web/uibindings.Deps through the resolverDeps adapter, so a registered
	// Resolver can reach a tool call, a memory query, or an artifact's rendered
	// bytes under the viewer's own subject.

	// TrustedOrigin is the live trusted-origin base URL, used to pin the POST's
	// Origin header (CSRF).
	TrustedOrigin() string
	// NATSRequest is the request/reply transport the "tool" resolver uses to
	// reach the runner's KindUIDataBinding responder; nil means unconfigured.
	NATSRequest() channelevents.RequestFunc

	// Memory, Artifacts and ArtifactRenderBytes back the "memory" and
	// "artifact" resolvers, in the three nil-check shapes uibindings.Deps
	// documents. Memory returns an INTERFACE: assigning a typed-nil pointer
	// into it makes the resolver's `mem == nil` fail-closed check lie (see
	// AGENTS.md's typed-nil rule), so declare the backing field as the
	// interface and assign only once a real client exists. Artifacts returns a
	// concrete pointer and ArtifactRenderBytes a func type, where a nil
	// comparison is honest by construction.
	Memory() memory.Memory
	Artifacts() *artifacts.Service
	ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc

	// --- GET /agent-ui/{ns}/{name}/live (live.go) --------------------------

	// NATS is the connection livemirror.WatchOutbound subscribes on to relay
	// ui_action_update envelopes to an attached browser. Same name and type as
	// sessionview.Deps.NATS, so internal/cmd/webd satisfies both with one accessor.
	//
	// nil degrades this route to snapshot-only: the memory read still answers,
	// and a browser learns the terminal state on its next reconnect. A real
	// degradation, and a LOGGED one.
	NATS() *nats.Conn

	// --- POST /agent-ui/{ns}/{name}/start (start.go) ----------------------

	// StartBrowserSession starts a NEW browser-channel AgentSession. A FUNC
	// type, not an interface, so a nil comparison is honest.
	//
	// nil means this webd cannot host a browser session — its replies would
	// have no sink, since a browser Channel is not relayed by channelsd and its
	// real Sender comes from browser.NewHost. Routes then omits the start route
	// and logs that it did; the terminal ladder branch still answers 410.
	StartBrowserSession() browsersession.StartFunc

	// LiveSessions is this process's live-session table, or nil when it hosts
	// none. It applies the two capacity limits: browserstart.Start reserves a
	// slot through it BEFORE any cluster object is created, so a refusal leaves
	// nothing behind. Without it, a viewer pressing the ended-session control
	// repeatedly is bounded by nothing.
	//
	// Read PER REQUEST, not captured: the table is built during the webui
	// server's UI-mount loop, after webd assembles its deps umbrella, so a
	// captured value is always nil. An INTERFACE — the nil check is honest only
	// if the implementation returns a genuine nil interface, never a typed-nil
	// pointer wrapped in one. Shared verbatim with sessions.Deps.
	LiveSessions() browserstart.LiveSessions

	// StartableNamespaces names the namespaces this webd can actually CREATE a
	// session in. browserstart.Start refuses anything outside it BEFORE
	// reserving a slot, so the control answers "this server cannot start one
	// here" rather than a bare 500 out of an apiserver Forbidden. Shared
	// verbatim with sessions.Deps.
	StartableNamespaces() []string
	// WorkshopNamespacesFor is the start gate's dynamic arm — see
	// browserstart.Deps for what it means and why an error fails it closed
	// without touching the static arm above. Shared verbatim with
	// sessions.Deps.
	WorkshopNamespacesFor(ctx context.Context, subject string) ([]string, error)
}
