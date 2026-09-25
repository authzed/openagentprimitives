package chat

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
)

// Deps is the transcript data plane's dependency interface. webd's concrete
// deps value implements it; the WebUI casts webui.Deps to this in Routes(). A
// cast failure (nil, or a webd umbrella missing one of these collaborators)
// means the chat routes fail closed — see chat.go's Routes and ensureRegistry.
type Deps interface {
	// K8s reads and writes the Channel + AgentSession CRs a chat conversation
	// owns, the AgentClass the info panel falls back to before
	// status.effectiveSettings is stamped, and the operator-minted per-session
	// memory-token Secret.
	K8s() client.Client
	// NATS is the shared connection used for the session's outbound relay
	// subscription and inbound wakeups. nil means NATS is not configured —
	// chat.Routes fails closed in that case (a Relay cannot subscribe on a
	// nil connection).
	NATS() *nats.Conn
	// Authz satisfies the channelsd pipeline's Authz interface (SpiceDB
	// started_by / interact / approval checks). nil means SpiceDB is not
	// configured — chat.Routes fails closed.
	Authz() pipeline.Authz
	// OperatorURL is the operator's memory/artifact HTTP base URL, used to
	// build the per-session memory client (the same operator memory API the
	// runner and channelsd read/write).
	OperatorURL() string
	// MemoryToken is webd's own read-only operator-memory bearer token. The
	// operator maps it to the "system:webd" caller and refuses every mutating
	// route, so it can read any session's transcript turns but never write.
	// Empty when webd started without one — the transcript endpoint then fails
	// closed with a clear message rather than returning an empty conversation.
	MemoryToken() string
	// TrustedOrigin returns the live trusted-origin base URL (e.g.
	// "https://127.0.0.1:8443"), used to gate the websocket upgrade's
	// Origin header (mirrors artifactview's liveHandler).
	TrustedOrigin() string
	// ArtifactViewMinter mints signed "<webd-base>/artifact-view?d=…&sig=…"
	// deep-links for the "View live" offer the agent makes via
	// artifact_offer_view. nil when webd has no passthroughlink signing key;
	// the sender then says so loudly rather than dropping the offer.
	ArtifactViewMinter() channelkinds.ArtifactViewMinter
	// SessionViewMinter composes durable
	// "<webd-trusted-origin>/session-view/<ns>/<name>" escalation links. Needs
	// no signing key — the session-view page runs its own CheckInteract at open
	// time — so a TrustedOrigin-backed implementation is always non-nil; nil is
	// still handled loudly, for a Deps with no trusted origin yet.
	SessionViewMinter() channelkinds.SessionViewMinter
	// CheckInteract is the authorization gate for every session-scoped route
	// this package serves: the same agentsession#interact check
	// pkg/web/webui/sessionview, .../interact and .../agentui use, with an
	// identical signature so internal/cmd/webd's adapter satisfies it with no second
	// accessor. A returned error is never a denial (see
	// Registry.checkInteract) — surface it as unavailable, not forbidden.
	CheckInteract(ctx context.Context, ns, name, subject string) (bool, error)
	Logger() logr.Logger
}
