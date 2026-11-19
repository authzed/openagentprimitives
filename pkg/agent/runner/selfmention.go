package runner

import (
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// SelfMentionOf returns the token that mentions the agent itself on the
// session's input channel, or "" when there is none to name: the session is
// not channel-attached, the kind did not resolve, the kind has no mentionable
// identity of its own (it is not a channelkinds.SelfMentioner), or the binding
// does not carry what the kind derives the token from.
//
// It consults no capability. A summoning message opens with this token whether
// or not the class offers lookup_user_for_mention, so the warning it feeds
// (WithSelfMention) must not depend on that tool being offered.
func SelfMentionOf(kind channelkinds.Kind, binding *spiceboxv1alpha1.ChannelBinding) string {
	if binding == nil {
		return ""
	}
	sm, ok := kind.(channelkinds.SelfMentioner)
	if !ok {
		return ""
	}
	return sm.SelfMention(binding.External)
}

// selfMentionSectionTitle heads the section WithSelfMention renders.
const selfMentionSectionTitle = "Your mention token"

// selfMentionSection is the system prompt's account of the agent's own token.
// Its wording lives beside senderMentionText, which renders the other half:
// the sender's token on each message.
func selfMentionSection(token string) capability.PromptSection {
	return capability.PromptSection{
		Title: selfMentionSectionTitle,
		Body: "On this channel you yourself are " + token + ". A message containing " + token +
			" is addressing YOU — that token is not the person you are talking to, and you must never put it in a reply. " +
			"To @-mention the person who sent a message, use the sender mention token shown alongside that message.",
	}
}

// WithSelfMention appends a section naming the agent's own mention token and
// saying it must never appear in a reply. Callers pass SelfMentionOf's result;
// "" renders nothing. The section precedes the capability-contributed ones and
// is deliberately not one of them: a capability section rides with a tool, and
// this text describes what inbound messages contain, which no tool gates.
func WithSelfMention(token string) ComposeOption {
	return func(c *composeConfig) { c.selfMention = token }
}
