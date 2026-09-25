package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func init() { Register(&threadHistoryCapability{}) }

// threadHistoryCapability is default-on but active only for adopted threads
// (RoutingMode == "mention_only"). A mention_only binding can only be
// produced by a kind that implements ConversationReader, so this gate
// transitively satisfies "only conversation-capable channels inject the
// tool". Bot-originated threads have their whole transcript in memory
// already, so the tool would be pointless there. Ports main.go:548-560.
type threadHistoryCapability struct{}

func (threadHistoryCapability) Name() string                                { return "thread_history" }
func (threadHistoryCapability) DefaultOn() bool                             { return true }
func (threadHistoryCapability) Infrastructural() bool                       { return false }
func (threadHistoryCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (threadHistoryCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Binding == nil || o.Binding.RoutingMode != "mention_only" {
		return nil, nil // not an adopted thread → not a skip, just inactive
	}
	return []tool.Tool{
		meta.NewReadHistory(meta.ReadHistoryConfig{
			RequestSubject: channelevents.HistoryRequestSubject(o.Env.SubjectPrefix),
			NATSRequest:    o.Env.NATSRequest,
		}),
	}, nil
}
