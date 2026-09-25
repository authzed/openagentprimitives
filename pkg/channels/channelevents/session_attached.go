// pkg/channels/channelevents/session_attached.go
//
// SessionAttached is a control-plane NATS event published by channelsd's
// outbound relay after it patches an AgentSession's outputChannel metadata on
// first send. Channel listeners that maintain per-session in-process state
// (today: the slack listener's threadIndex) subscribe to learn the new anchor
// in real time, without waiting for the next channelsd restart's startup walk.
//
// This subject is deliberately outside the per-session
// "ap.session.<ns>.<name>.{in,out}.<kind>" taxonomy in kinds.go: it is a
// one-way, kind-agnostic, *channel-scoped* fanout — every event flows from
// "the relay" (single publisher) to "listeners that own the OutputChannel
// name" (one subscriber-side dispatcher). It is not an Envelope, so the
// per-session validation rules (closed Kind enum, version gate,
// payload-required check) do not apply to a fanout with no session correlate.
package channelevents

import "github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"

// SessionAttachedSubject is the NATS subject channelsd's outbound relay
// publishes on after a successful OutputChannel patch. Channel listeners that
// need real-time notification of "a session attached me as its OutputChannel"
// subscribe here.
//
// It goes over the bus even though publisher and subscriber live in the same
// channelsd process today: channelsd already signals its control plane over
// NATS, and a split-process channelsd keeps working without rewiring.
const SessionAttachedSubject = subjects.SessionAttached

// SessionAttached is the payload of a SessionAttachedSubject message, JSON
// encoded for cross-process compatibility. It carries enough context for a
// listener-side dispatcher to find the active listener without reading extra
// state, but the dispatcher still does a fresh k8s Get for the full
// AgentSession before invoking the listener's SessionUpdated hook, so the
// handler sees current state rather than this snapshot.
type SessionAttached struct {
	// Namespace is the AgentSession's namespace; SessionName/SessionUID
	// identify the instance (a fork/recreate reuses the name, not the UID).
	Namespace   string `json:"namespace"`
	SessionUID  string `json:"sessionUID"`
	SessionName string `json:"sessionName"`
	// OutputChannelName is the Channel CR the session just attached to;
	// OutputChannelKind is its channelkinds key ("slack", "browser", …).
	OutputChannelName string `json:"outputChannelName"`
	OutputChannelKind string `json:"outputChannelKind"`
	// OutputChannelKey is the kind-opaque conversation anchor (Slack: the
	// channel+thread key) the listener indexes this session under.
	OutputChannelKey string `json:"outputChannelKey"`
	// External carries kind-specific anchor details a listener needs beyond
	// the key. Empty for kinds that need none.
	External map[string]string `json:"external,omitempty"`
}
