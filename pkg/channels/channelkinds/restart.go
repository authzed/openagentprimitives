package channelkinds

import (
	"context"

	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// RestartCapable is implemented by Kinds that surface a "restart from here"
// affordance. channelsd's start-up type-asserts across the registry: kinds
// that implement it get their handler wired, the rest are absent from the
// feature. The kind owns whatever channel-specific UI ends up calling submit
// with a populated RestartTrigger.
type RestartCapable interface {
	RegisterRestartTrigger(submit RestartSubmitFunc)
}

// RestartSubmitFunc is what a RestartCapable kind calls when a user
// has completed a restart action and the kind has resolved the cut
// turn and new text. channelsd's implementation publishes a
// channelevents.KindRestartTrigger envelope on the session's NATS
// subject.
type RestartSubmitFunc func(ctx context.Context, t RestartTrigger) error

// RestartTrigger is the channel-kind-agnostic envelope carried from
// the channel kind to channelsd's publisher.
type RestartTrigger struct {
	// SessionRef identifies the (parent) AgentSession the restart
	// targets. The kind resolves this from its channel surface
	// (Slack: thread_ts → channelKey → AgentSession lookup).
	SessionRef types.NamespacedName

	// CutTurnIndex is the turn the user clicked Restart on
	// (inclusive — turns 0..CutTurnIndex are copied to the child).
	CutTurnIndex int32

	// NewUserText is the message text the user submitted.
	NewUserText string

	// TriggeredBy is the channel-kind-scoped external identity that
	// initiated the restart. channelsd maps this to a canonical
	// subject before populating the NATS envelope.
	TriggeredBy channelevents.ExternalIdentity

	// KindRequestRef is an opaque round-trip handle the kind can
	// use to ack the continuation back to the user.
	KindRequestRef string
}
