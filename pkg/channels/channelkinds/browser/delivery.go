package browser

import (
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery/recorded"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// NewDeliveryReceiver accepts into the durable private browser transcript,
// independently of live tabs. The caller supplies component signing memory.
func (*Kind) NewDeliveryReceiver(mem memory.Memory) delivery.Receiver {
	return recorded.New(mem, "browser-transcript")
}

// replyScope is where an intent's acceptance lives in the transcript.
func replyScope(i delivery.Intent) memory.Scope { return recorded.Scope(i) }
