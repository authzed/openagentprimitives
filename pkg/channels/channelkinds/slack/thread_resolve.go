package slack

import "github.com/authzed/openagentprimitives/pkg/channels/channelkinds"

// effectiveOutboundThreadTS resolves the thread_ts an outbound Slack message
// should post under. The per-turn anchor is stamped onto the AgentSession's
// LastInboundTSAnnotationKey annotation on every inbound (DM and channel), so it
// is the source of truth: a plain DM binding's External["thread_ts"] is never
// populated (handleDM stamps no thread_ts, and no relay write-back fires for a
// non-fork InputChannel-only session), so reading it instead posts DM replies
// top-level. Falls back to External["thread_ts"] when the annotation is absent
// (cron/fork first send, where the empty result correctly lets the sender's
// first-send capture run).
//
// Reading from SessionInfo (populated by the relay) rather than a fresh K8s Get
// guarantees the streaming bubble and the final reply resolve to the SAME value
// in a turn, so ConsumeFinalTarget can never miss and double-post.
//
// LastInboundTS is a single session-scoped annotation, so it is
// last-inbound-wins: within a turn, the most recent inbound's anchor is used
// (matches the channel path's existing semantics; the pipeline serializes
// turns on Idle).
func effectiveOutboundThreadTS(sess channelkinds.SessionInfo) string {
	if sess.Annotations != nil {
		if ts := sess.Annotations[LastInboundTSAnnotationKey]; ts != "" {
			return ts
		}
	}
	if sess.Channel != nil {
		return sess.Channel.External["thread_ts"]
	}
	return ""
}
