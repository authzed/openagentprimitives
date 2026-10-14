package slack

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func firstSendSession() channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Namespace: "default", Name: "cron-session",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			// thread_ts intentionally absent: the cron/bot first-send case that
			// roots the thread.
			External: map[string]string{"channel_id": "C01ABCDEF"},
		},
	}
}

// TestSender_FirstSend_CapturesPermalink: on the send that roots the thread,
// the sender resolves the canonical permalink and returns it in
// SubChannelSendResult.External so the relay can persist it for the
// artifact-view origin link.
func TestSender_FirstSend_CapturesPermalink(t *testing.T) {
	c := &fakeSlackClient{postedTS: "1234.5678", getPermalinkResult: "https://slack.test/archives/C01ABCDEF/p12345678"}
	s := newSender(c)
	res, err := s.Send(context.Background(), firstSendSession(), userMessageEnvelope(t, "first cron post"))
	require.NoError(t, err, "Send")
	assert.Equal(t, "1234.5678", res.External["thread_ts"], "thread root still captured")
	assert.Equal(t, "https://slack.test/archives/C01ABCDEF/p12345678", res.External["permalink"], "canonical permalink captured")
	require.Len(t, c.getPermalinkCalls, 1, "getPermalink called once on first root")
	assert.Equal(t, "C01ABCDEF", c.getPermalinkCalls[0].Channel)
	assert.Equal(t, "1234.5678", c.getPermalinkCalls[0].Ts)
}

// TestSender_FirstSend_PermalinkFailureOmitsLink: a getPermalink failure must
// never block the send; the thread still roots and permalink is simply absent.
func TestSender_FirstSend_PermalinkFailureOmitsLink(t *testing.T) {
	c := &fakeSlackClient{postedTS: "1234.5678", getPermalinkErr: errors.New("channels:read missing")}
	s := newSender(c)
	res, err := s.Send(context.Background(), firstSendSession(), userMessageEnvelope(t, "first cron post"))
	require.NoError(t, err, "send must succeed despite getPermalink failure")
	assert.Equal(t, "1234.5678", res.External["thread_ts"], "thread root still captured")
	_, has := res.External["permalink"]
	assert.False(t, has, "permalink omitted when getPermalink fails")
}
