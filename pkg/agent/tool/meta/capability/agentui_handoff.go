package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func init() { Register(&agentUIHandoffCapability{}) }

// agentUIHandoffCapability offers show_agent_ui: the agent's way to hand a
// user a button into this session's dashboard, from a channel that cannot
// render the page itself.
//
// Infrastructural — always-on, no off switch. The gate is the UI's EXISTENCE,
// not a grant: an agent whose class references an AgentUI can inherently point
// at it. update_view and read_view beside it are opt-in because mutating and
// reading a view are different powers from naming its address. Listing this
// capability in spec.capabilities validates and changes nothing.
type agentUIHandoffCapability struct{}

func (agentUIHandoffCapability) Name() string                                { return "agent_ui_handoff" }
func (agentUIHandoffCapability) DefaultOn() bool                             { return true }
func (agentUIHandoffCapability) Infrastructural() bool                       { return true }
func (agentUIHandoffCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

// Offer decides injection ONCE, here; show_agent_ui re-derives none of it at
// call time, because a tool the model can see but cannot use costs a turn to
// discover.
//
// What is settled here is the CHANNEL's ability to carry the offer, not the
// recipient's permission to open the page — that is live per-turn state and
// stays a call-time check inside the tool.
//
// The declines below return (nil, nil) rather than a SkipReason because each is
// a normal, common state (no UI, no channel, viewer already in a browser), and a
// skip line in most sessions in the cluster is how a log stops being read. The
// one decline that IS logged is the wiring gap, since nothing else would tell an
// operator why a UI-bearing Slack session never got the tool.
func (agentUIHandoffCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	// OutboundBinding is where the button is POSTED (OutputChannel, falling
	// back to InputChannel) — the same binding the outbound relay resolves a
	// sender from, so a split-channel session is gated on the channel the user
	// actually reads. It is nil-safe on a nil session and nil when the session
	// has no channel at all, so it answers "channel-attached?" too.
	out := spiceboxv1alpha1.OutboundBinding(o.Session)
	if o.Env.UIView == nil || out == nil {
		return nil, nil
	}
	// Ask the registry, never a kind name: a kind declares itself a browser
	// surface by implementing channelkinds.BrowserSurface. An unregistered or
	// empty name answers false — the fail-safe direction, since a redundant
	// link offer is cosmetic while withholding one leaves a Slack or terminal
	// viewer unable to reach the page at all.
	if chregistry.IsBrowserSurface(out.Kind) {
		return nil, nil
	}
	if o.Env.NATSPublish == nil || o.Env.ViewerCanInteract == nil {
		return nil, &SkipReason{
			Capability: "agent_ui_handoff",
			Reason:     "this session has an agent UI on a non-browser channel, but the runner wiring to offer it is incomplete",
		}
	}
	return []tool.Tool{meta.NewShowAgentUI(meta.ShowAgentUIConfig{
		NATSPublish:       o.Env.NATSPublish,
		EnvelopeSigner:    o.Env.EnvelopeSigner,
		ViewerCanInteract: o.Env.ViewerCanInteract,
	})}, nil
}
