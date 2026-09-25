package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func init() { Register(&mentionLookupCapability{}) }

// mentionLookupCapability is default-on, active only when channel-attached,
// and bound to the channel kind's LookupUser resolver via the runner's
// one-time resolve.ForSession result (RunnerEnv.Resolved*). Ports
// main.go:562-576.
type mentionLookupCapability struct{}

func (mentionLookupCapability) Name() string                                { return "mention_lookup" }
func (mentionLookupCapability) DefaultOn() bool                             { return true }
func (mentionLookupCapability) Infrastructural() bool                       { return false }
func (mentionLookupCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (mentionLookupCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Binding == nil {
		return nil, nil // not channel-attached → no channel tools, not a skip
	}
	if o.Env.ResolveErr != nil {
		return nil, &SkipReason{Capability: "mention_lookup", Reason: "channel resolve failed"}
	}
	t := meta.NewLookupUserForMention(meta.LookupUserForMentionConfig{
		Kind:        o.Env.ResolvedKind,
		LookupDeps:  channelkinds.LookupDeps{Secret: o.Env.ResolvedSecret},
		ChannelKind: o.Env.ResolvedChannel.Spec.Kind,
	})
	if t == nil {
		// Bound kind advertises no lookup kinds — a normal inactive state,
		// not a skip (mirrors NewLookupUserForMention's own nil-return gate).
		return nil, nil
	}
	return []tool.Tool{t}, nil
}
