package local

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/clienthosted"
)

// localListener is the host-side Listener the TUI drives. The inbound paths are
// NATS request/publish only and are shared verbatim with the `browser` web-chat
// kind via clienthosted.Listener; the two methods below are call-shape adapters,
// not behaviour.
type localListener struct {
	clienthosted.Listener
}

// SubmitUserMessage drops the idempotency key: the TUI has no Retry
// affordance, so there is no second delivery for channelsd's dedup cache to
// collapse, and an empty requestID skips that cache. It also drops the identity
// argument, passing its own configured Ext — the TUI is single-user for its
// whole lifetime.
//
// This narrower shape exists only because cmd/oap calls it this way; it should
// collapse into the shared method the next time that call site is edited.
func (l *localListener) SubmitUserMessage(ctx context.Context, text string) (channelkinds.InboundDecision, error) {
	return l.Listener.SubmitUserMessage(ctx, l.Listener.Ext, text, "")
}

// SubmitInteractionDecision swaps requestRef and category into the shared
// method's order, and — like SubmitUserMessage above — supplies the TUI's own
// configured identity rather than asking cmd/oap to pass one.
//
// The swap matters: requestRef and category are both strings, so a mis-binding
// here is silent. The adapter is what lets the shared method keep one order
// without touching cmd/oap's call site; it should go the next time that call
// site is edited.
func (l *localListener) SubmitInteractionDecision(ctx context.Context, ns, name, requestRef, category, actionID string) error {
	return l.Listener.SubmitInteractionDecision(ctx, l.Listener.Ext, ns, name, category, requestRef, actionID)
}

// compile-time interface check.
var _ channelkinds.Listener = (*localListener)(nil)
