package fake

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery/recorded"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

var _ channelkinds.DeliveryReceiverProvider = Kind{}

// NewDeliveryReceiver implements channelkinds.DeliveryReceiverProvider. The
// fake has no external transport, so its durable acceptance is the signed
// reply record in the session's memory — idempotent by delivery ID, the same
// contract a real group-hosting kind meets against its own transport. The
// live envelope the runner publishes afterwards still reaches the Driver.
func (Kind) NewDeliveryReceiver(mem memory.Memory) delivery.Receiver {
	return recorded.New(mem, "fake-recorded")
}
