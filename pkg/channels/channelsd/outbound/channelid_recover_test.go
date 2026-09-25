package outbound

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// slackOutputChannelCR returns a role=output slack Channel CR named "slack-out"
// whose outputDefaults name channelID as the configured destination — the
// source of truth the relay recovers from when a session's outbound binding
// never got (or lost) its channel_id.
func slackOutputChannelCR(channelID string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-out", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack",
			Role: spiceboxv1alpha1.ChannelRoleOutput,
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{
					ChannelID:      channelID,
					ThreadStrategy: "new-thread-per-session",
				},
			},
		},
	}
}

// failFirstSessionInfoSender fails the first Send (the reviewbot's
// respond_to_user reply) and records the SessionInfo of every Send it was asked
// to make — so a test can prove the degraded delivery-failure notice is
// attempted on a binding that actually carries a channel_id, rather than dying
// silently on the same unusable binding the original send failed on.
type failFirstSessionInfoSender struct {
	mu  sync.Mutex
	got []channelkinds.SessionInfo
}

func (s *failFirstSessionInfoSender) Send(_ context.Context, info channelkinds.SessionInfo, _ channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, info)
	if len(s.got) == 1 {
		return channelkinds.SubChannelSendResult{}, errSendFailed
	}
	return channelkinds.SubChannelSendResult{}, nil
}

func (s *failFirstSessionInfoSender) infos() []channelkinds.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channelkinds.SessionInfo(nil), s.got...)
}

// TestRelay_RecoversMissingChannelIDFromOutputChannelCR is the reviewbot
// incident: a session whose slack output binding never carried a channel_id
// (so the slack sender fails "external.channel_id missing on
// session.spec.channel") must not be delivered onto an empty destination. The
// relay recovers the channel_id from the output Channel CR's
// outputDefaults.channelId before dispatch, so the reply starts on the
// configured slack channel instead of failing into monitoring silence.
func TestRelay_RecoversMissingChannelIDFromOutputChannelCR(t *testing.T) {
	nc := connectNATS(t)
	// nil External ⇒ the outbound binding has no channel_id — the exact shape
	// the slack sender refuses.
	sess := cronSession("rb", nil)
	cli := fakeClientWith(t, sess, slackOutputChannelCR("C0RECOVERED"))

	sndr := &captureSessionInfoSender{}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	env := buildEnv(t, "rb", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "review is ready"})
	publishOut(t, nc, "default", "rb", "user_message", env)

	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		_, ok := sndr.first()
		return ok
	}), "send never reached the Sender")

	info, _ := sndr.first()
	require.NotNil(t, info.Channel, "SessionInfo must carry the outbound binding")
	assert.Equal(t, "C0RECOVERED", info.Channel.External["channel_id"],
		"the relay must recover channel_id from the output Channel CR's outputDefaults when the binding lacked one")
}

// TestRelay_ChannelIDRecovery_FallbackNoticeReachesRecoveredBinding locks the
// no-silent-errors half: when the recovered channel is genuinely unreachable
// (the send still fails), the degraded delivery-failure notice must be
// attempted on a binding that carries the recovered channel_id — not silently
// dropped onto the same channel_id-less binding, which is why the original
// incident showed only in monitoring and never in the thread.
func TestRelay_ChannelIDRecovery_FallbackNoticeReachesRecoveredBinding(t *testing.T) {
	nc := connectNATS(t)
	sess := cronSession("rb", nil) // no channel_id on the outbound binding
	// Stand-in for the slack listener's last-inbound-ts anchor: the thread the
	// user is actually reading. The relay must carry it onto the fallback
	// SessionInfo so the delivery-failure notice threads into the original
	// conversation rather than posting untethered.
	sess.Annotations = map[string]string{"slack.agentprimitives.authzed.com/last-inbound-ts": "999.888"}
	cli := fakeClientWith(t, sess, slackOutputChannelCR("C0RECOVERED"))

	sndr := &failFirstSessionInfoSender{}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	env := buildEnv(t, "rb", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "the real reply"})
	publishOut(t, nc, "default", "rb", "user_message", env)

	// Two sends: the failing original + the degraded fallback notice.
	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		return len(sndr.infos()) >= 2
	}), "relay did not attempt a fallback notice after the user_message Send failed")

	infos := sndr.infos()
	require.GreaterOrEqual(t, len(infos), 2, "original + fallback")
	require.NotNil(t, infos[1].Channel, "fallback SessionInfo must carry the outbound binding")
	assert.Equal(t, "C0RECOVERED", infos[1].Channel.External["channel_id"],
		"the delivery-failure notice must be sent on a binding carrying the recovered channel_id, not silently dropped")
	assert.Equal(t, "999.888", infos[1].Annotations["slack.agentprimitives.authzed.com/last-inbound-ts"],
		"the fallback notice must carry the last-inbound thread anchor so the error lands in the original thread")
}

// TestRelay_ChannelIDRecovery_LeavesExistingChannelIDUntouched guards the
// hot-path idempotency: a binding that already carries channel_id (and a
// thread_ts) must be left exactly as-is — recovery must neither run nor clobber
// an established thread anchor with the Channel CR's channel-level default.
func TestRelay_ChannelIDRecovery_LeavesExistingChannelIDUntouched(t *testing.T) {
	nc := connectNATS(t)
	sess := cronSession("rb", map[string]string{"channel_id": "C0ESTABLISHED", "thread_ts": "111.222"})
	// The Channel CR names a DIFFERENT channel; if recovery wrongly ran, the
	// binding's channel_id would flip to this value.
	cli := fakeClientWith(t, sess, slackOutputChannelCR("C0DIFFERENT"))

	sndr := &captureSessionInfoSender{}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	env := buildEnv(t, "rb", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "routine reply"})
	publishOut(t, nc, "default", "rb", "user_message", env)

	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		_, ok := sndr.first()
		return ok
	}), "send never reached the Sender")

	info, _ := sndr.first()
	require.NotNil(t, info.Channel)
	assert.Equal(t, "C0ESTABLISHED", info.Channel.External["channel_id"],
		"an established channel_id must never be overwritten by the Channel CR default")
	assert.Equal(t, "111.222", info.Channel.External["thread_ts"],
		"an established thread anchor must survive untouched")
}
