// Package channelhistorygate contains the shared "offer decision" helper
// internal/cmd/runner and the e2e in-process runner factory both use to decide
// whether to inject the read_channel_history tool for a session. One
// source of truth means the e2e tests exercise the production wiring
// path — if the offer decision diverges between binaries, the tests
// catch it.
package channelhistorygate

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Offer reports whether read_channel_history should be injected for this
// session, and returns the kind's reader when so. Mirrors the gating
// matrix enforced server-side by pkg/channels/channelsd/historyresp.ChannelResponder.gate
// — the responder is the authoritative enforcement point (it re-derives
// the channel from the session and checks SpiceDB on every read); this
// gate only decides whether it's worth offering the tool to the LLM at
// all, so the two must agree or a gap opens where the tool is offered but
// every call is withheld (or, worse, withheld here but would have been
// allowed there, silently hiding a capability).
//
// The tool is offered when: the Channel opted in; the kind implements
// ChannelHistoryReader; and EITHER info-leakage gating is active (the
// responder then enforces a per-read SpiceDB view check) OR the session's
// output channel equals its input channel (no cross-channel exposure).
// When info-leakage is off and output != input, the tool is withheld
// entirely.
func Offer(
	ch *spiceboxv1alpha1.Channel, k channelkinds.Kind,
	sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass,
) (channelkinds.ChannelHistoryReader, bool) {
	if ch.Spec.ChannelHistory == nil || !ch.Spec.ChannelHistory.Enabled {
		return nil, false
	}
	reader, ok := k.(channelkinds.ChannelHistoryReader)
	if !ok {
		return nil, false
	}
	leakageOn := class != nil && class.Spec.GetAuthz().InformationLeakage.ResolvedMode() != "disabled"
	if leakageOn {
		return reader, true
	}
	if sess.Spec.InputChannel != nil && sess.Spec.InputChannel.SameChannelAs(sess.Spec.OutputChannel) {
		return reader, true
	}
	return nil, false
}
