package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/runner/channelhistorygate"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func init() { Register(&channelHistoryCapability{}) }

// channelHistoryCapability is default-on, active only when channel-attached
// and channelhistorygate.Offer agrees the Channel opted in, the bound kind
// can read whole-channel history, and either info-leakage gating is active
// or output==input (see channelhistorygate.Offer's doc for the full
// matrix). Ports main.go:578-593.
type channelHistoryCapability struct{}

func (channelHistoryCapability) Name() string                                { return "channel_history" }
func (channelHistoryCapability) DefaultOn() bool                             { return true }
func (channelHistoryCapability) Infrastructural() bool                       { return false }
func (channelHistoryCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (channelHistoryCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Binding == nil {
		return nil, nil // not channel-attached → no channel tools, not a skip
	}
	if o.Env.ResolveErr != nil {
		// Quietly inactive, not a skip: mirrors main.go:584's `if err == nil`
		// gate. mention_lookup's Offer already turns the same ResolveErr into
		// a logged SkipReason for this session; a second report here would be
		// redundant.
		return nil, nil
	}
	r, ok := channelhistorygate.Offer(o.Env.ResolvedChannel, o.Env.ResolvedKind, o.Session, o.Class)
	if !ok {
		return nil, nil
	}
	return []tool.Tool{
		meta.NewReadChannelHistory(meta.ReadChannelHistoryConfig{
			RequestSubject:    channelevents.ChannelHistoryRequestSubject(o.Env.SubjectPrefix),
			NATSRequest:       o.Env.NATSRequest,
			SupportsDateRange: r.ChannelHistoryBounds().SupportsDateRange,
		}),
	}, nil
}
