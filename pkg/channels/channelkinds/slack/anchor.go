package slack

import (
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Compile-time check: *Kind satisfies the optional outbound-anchor interface.
var _ channelkinds.OutboundAnchorProvider = (*Kind)(nil)

// OutboundAnchor derives the initial outbound routing metadata for a session
// whose reply lands in this Channel but whose inbound origin was elsewhere
// (a github webhook or a bento cron Channel). Returns an error when the Channel
// carries no usable destination.
//
// The Channel controller reports that at apply time, in two places and neither
// of them unconditional: on the role=input Channel bound to the same
// AgentClass, which resolves this Channel and asks the same question of it; and
// on THIS Channel, when the class it is bound to has no user-attributable input
// — a webhook payload or a cron tick supplies no destination, so a role=output
// Channel serving one must carry its own. Outside those two, the refusal
// surfaces later, when channelsd's inbound pipeline declines to create a
// session it could not deliver.
//
// Each error names the spec field to fill in, because the Channel controller
// puts it verbatim in the Valid condition and it is what whoever applied the
// Channel has to act on. Two different fields can be the missing one, and only
// this kind knows which.
//
// Pure function of ch: no wall-clock, no randomness. For the thread-per-session
// and direct strategies the key is a bare channel anchor; the outbound relay's
// patchOutputChannel replaces it with thread:<channel_id>:<thread_ts> once the
// first send returns a real thread root.
//
// So the thread root is whatever is sent first. For a session a human started
// that is their own message. For one no human started, the relay sends the
// trigger's own opening line ahead of the agent's first output and roots the
// thread on that (AnnotationSessionOpening, outputbind.SessionOpening) — the
// alternative is a thread whose first message is a bare plan or status caption,
// with nothing saying what it is about.
func (k *Kind) OutboundAnchor(ch *spiceboxv1alpha1.Channel) (string, map[string]string, error) {
	if ch == nil || ch.Spec.Slack == nil || ch.Spec.Slack.OutputDefaults == nil ||
		ch.Spec.Slack.OutputDefaults.ChannelID == "" {
		return "", nil, fmt.Errorf("spec.slack.outputDefaults.channelId is required to post into a Slack channel")
	}
	od := ch.Spec.Slack.OutputDefaults
	if od.ThreadStrategy == "static-thread" {
		if od.StaticThreadTS == "" {
			// staticThreadTs is required for this strategy; without it there
			// is no anchor to seed and the session would post untethered.
			return "", nil, fmt.Errorf(`spec.slack.outputDefaults.staticThreadTs is required when threadStrategy="static-thread"`)
		}
		return "thread:" + od.ChannelID + ":" + od.StaticThreadTS,
			map[string]string{"channel_id": od.ChannelID, "thread_ts": od.StaticThreadTS},
			nil
	}
	return "channel:" + od.ChannelID,
		map[string]string{"channel_id": od.ChannelID},
		nil
}
