package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func init() { Register(&openingSummaryCapability{}) }

// openingSummaryCapability offers update_opening_summary, which lets an agent
// enrich the pinned opening message of a TRIGGERED session. It is gated on the
// INPUT kind posting opening lines (channelkinds.TriggerDescriber, resolved
// through chregistry.TriggerDescriberFor) — so a human who starts an agent by
// typing in Slack, whose input kind is not a describer, never sees it. This is
// a hard constraint, not a default: the tool must not exist for a
// human-started session at all, not merely go unused.
//
// Mirrors triggerStatusCapability's gated Offer shape (same package, same
// "kind either has the surface or it does not" reasoning), gated on the
// sibling registry lookup instead.
type openingSummaryCapability struct{}

func (openingSummaryCapability) Name() string          { return "opening_summary" }
func (openingSummaryCapability) DefaultOn() bool       { return true }
func (openingSummaryCapability) Infrastructural() bool { return false }
func (openingSummaryCapability) ParseConfig(json.RawMessage) (Config, error) {
	return nil, nil
}

func (openingSummaryCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Binding == nil {
		return nil, nil // not channel-attached → no opening message to enrich, not a skip
	}
	if _, ok := chregistry.TriggerDescriberFor(o.Binding.Kind); !ok {
		return nil, nil // input does not post opening lines → nothing to enrich
	}
	return []tool.Tool{
		meta.NewUpdateOpeningSummary(meta.UpdateOpeningSummaryConfig{LeakageGate: o.Env.LeakageGate}),
	}, nil
}
