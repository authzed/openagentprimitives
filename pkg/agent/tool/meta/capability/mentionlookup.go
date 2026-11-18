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

func (c mentionLookupCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	tools, _, skip := c.OfferWithSections(o)
	return tools, skip
}

// OfferWithSections adds, beside the tool, a section naming the agent's OWN
// mention token when the bound kind can derive one (channelkinds.SelfMentioner):
// a summoning message opens with that token, and unexplained it reads as the
// person to address. Like every section, it rides only with the tool.
func (mentionLookupCapability) OfferWithSections(o OfferContext) ([]tool.Tool, []PromptSection, *SkipReason) {
	if o.Binding == nil {
		return nil, nil, nil // not channel-attached → no channel tools, not a skip
	}
	if o.Env.ResolveErr != nil {
		return nil, nil, &SkipReason{Capability: "mention_lookup", Reason: "channel resolve failed"}
	}
	t := meta.NewLookupUserForMention(meta.LookupUserForMentionConfig{
		Kind:        o.Env.ResolvedKind,
		LookupDeps:  channelkinds.LookupDeps{Secret: o.Env.ResolvedSecret},
		ChannelKind: o.Env.ResolvedChannel.Spec.Kind,
	})
	if t == nil {
		// Bound kind advertises no lookup kinds — a normal inactive state,
		// not a skip (mirrors NewLookupUserForMention's own nil-return gate).
		return nil, nil, nil
	}
	var sections []PromptSection
	if sm, ok := o.Env.ResolvedKind.(channelkinds.SelfMentioner); ok {
		if token := sm.SelfMention(o.Binding.External); token != "" {
			sections = append(sections, selfMentionSection(token))
		}
	}
	return []tool.Tool{t}, sections, nil
}

// selfMentionSectionTitle heads the section selfMentionSection renders, kept
// as a constant so a test can assert on it without restating the body.
const selfMentionSectionTitle = "Your mention token"

func selfMentionSection(token string) PromptSection {
	return PromptSection{
		Title: selfMentionSectionTitle,
		Body: "on this channel you yourself are " + token + ". A message containing " + token +
			" is addressing YOU — that token is not the person you are talking to, and you must never put it in a reply. " +
			"To @-mention the person who sent a message, use the sender mention token shown alongside that message.",
	}
}

var _ SectionOfferer = mentionLookupCapability{}
