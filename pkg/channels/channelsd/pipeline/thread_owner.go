package pipeline

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// threadOwnerLabels are the two labels under which a session's thread anchor
// can be indexed. A human-initiated thread carries it on the INPUT binding
// (LabelChannelKey); a cron-spawned session's inbound anchor is per-firing and
// never repeats, so its thread lives on the OUTPUT binding
// (LabelOutputChannelKey), stamped by the outbound relay after the first send.
var threadOwnerLabels = []string{
	spiceboxv1alpha1.LabelChannelKey,
	spiceboxv1alpha1.LabelOutputChannelKey,
}

// threadAnchor returns the coordinates of the shared conversation an inbound
// event or a session binding names — "<channel_id>:<thread_ts>" — or "" when
// the surface is not a shared thread.
//
// Both halves are required, and the channel key is deliberately NOT used. A
// key is only globally meaningful for a thread: a DM key ("dm:<user>") is
// scoped to ONE Channel, so two bots DM'd by the same person hold the same key
// string for two entirely unrelated conversations — and Slack gives each bot
// its own DM channel_id. Keying ownership off the key would refuse the first
// message anyone sends to a second bot.
func threadAnchor(ext map[string]string) string {
	channelID, threadTS := ext["channel_id"], ext["thread_ts"]
	if channelID == "" || threadTS == "" {
		return ""
	}
	return channelID + ":" + threadTS
}

// bindsAnchor reports whether either of a session's bindings names the shared
// thread at anchor.
func bindsAnchor(sess *spiceboxv1alpha1.AgentSession, anchor string) bool {
	for _, b := range []*spiceboxv1alpha1.ChannelBinding{
		sess.Spec.InputChannel, sess.Spec.OutputChannel,
	} {
		if b != nil && threadAnchor(b.External) == anchor {
			return true
		}
	}
	return false
}

// ThreadAnchorForTest and BindsAnchorForTest expose threadAnchor/bindsAnchor
// to another package's tests, mirroring pkg/agent/tool/mcp's
// SetTimeoutForTest/UseTokenGateForTest "…ForTest" convention for a test-only
// accessor that must cross a package boundary. workshopthreadsrv duplicates
// this pair rather than importing this package (see that package's own
// bindingAnchor/bindsAnchor doc for why), and these two accessors are what let
// its tests assert the duplicate never drifts from this, the original, without
// giving production code a dependency on channelsd's pipeline package.
func ThreadAnchorForTest(ext map[string]string) string { return threadAnchor(ext) }

// BindsAnchorForTest is ThreadAnchorForTest's sibling for bindsAnchor. See
// ThreadAnchorForTest's doc.
func BindsAnchorForTest(sess *spiceboxv1alpha1.AgentSession, anchor string) bool {
	return bindsAnchor(sess, anchor)
}

// threadOwnedByAnotherAgent returns the name of a session already bound to
// this inbound's thread on behalf of a DIFFERENT AgentClass, or "" when none
// is — including whenever the inbound is not on a shared thread at all.
//
// A thread belongs 1:1 to one agent. Several agents' transports can be members
// of the same conversation — several Slack apps in one Slack channel — and
// each delivers its own copy of every event in it. Binding a second agent to a
// thread another agent owns is therefore not a rare race but the default
// outcome of that layout, and it shows up as one human message handled N times
// and as agents overwriting each other's thread status.
//
// The caller exempts adoption, where a second binding is exactly what the
// human asked for.
//
// A session with an empty spec.class never matches: absence is not evidence of
// a conflicting owner, and refusing on it would wedge threads whose owner is
// half-written.
func (p *Pipeline) threadOwnedByAnotherAgent(ctx context.Context, ev channelkinds.InboundEvent, keyHash string) (string, error) {
	anchor := threadAnchor(ev.External)
	if anchor == "" {
		return "", nil
	}
	for _, label := range threadOwnerLabels {
		var sessions spiceboxv1alpha1.AgentSessionList
		if err := p.K8s.List(ctx, &sessions,
			client.InNamespace(ev.Channel.Namespace),
			client.MatchingLabels{label: keyHash},
		); err != nil {
			return "", err
		}
		for i := range sessions.Items {
			s := &sessions.Items[i]
			if s.Spec.Class == "" || s.Spec.Class == ev.Channel.Spec.AgentClass {
				continue
			}
			if bindsAnchor(s, anchor) {
				return s.Name, nil
			}
		}
	}
	return "", nil
}
