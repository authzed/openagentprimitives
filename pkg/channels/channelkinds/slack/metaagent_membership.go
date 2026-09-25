// pkg/channels/channelkinds/slack/metaagent_membership.go
//
// Slack's answer to channelkinds.MetaagentMembershipReporter: whether the
// metaagent bot is configured for this workspace, and therefore whether a
// scope-approval prompt can be rendered in the channel at all.
package slack

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
)

// Compile-time assertion: Slack reports metaagent channel membership.
var _ channelkinds.MetaagentMembershipReporter = (*Kind)(nil)

// MetaagentMembership implements channelkinds.MetaagentMembershipReporter.
//
// Slack is the transport where the metaagent runs as its own bot user with its
// own app install, so "is the bot in this room" is a real question here. Today
// the answer is derived from configuration only — a bot user-id resolved by
// metaagentBotUser means the metaagent app is installed and can be addressed.
// The actual conversations.invite round-trip is a follow-on: it needs the
// primary bot's Slack token plus the channels:write scope at call time, which
// this method (called from a reconcile, with no channel credentials in hand)
// does not have. ChannelReasonMetaagentInsufficientScopes and
// ChannelReasonMetaagentDMNotSupported exist for that later, per-conversation
// answer; ch is threaded through so it can be given without changing the seam.
func (k *Kind) MetaagentMembership(ch *spiceboxv1alpha1.Channel) (channelkinds.MetaagentMembership, bool) {
	if k.metaagentBotUser() == "" {
		return channelkinds.MetaagentMembership{
			Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ChannelReasonMetaagentAppNotInstalled,
			Message: "metaagent bot user-id is not configured; create the " +
				"agentprimitives-system-metaagent-config Secret with a " +
				"slack_bot_user_id key and restart the operator (it reads " +
				clikit.EnvMetaagentSlackBotUserID + " at start-up)",
		}, true
	}
	return channelkinds.MetaagentMembership{
		Status:  metav1.ConditionTrue,
		Reason:  spiceboxv1alpha1.ChannelReasonMetaagentInvited,
		Message: "metaagent bot configured for this channel",
	}, true
}
