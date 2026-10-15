package slack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
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

// TestSender_AttachmentFirstRoot_CapturesPermalink: a reviewbot-style first
// output that is an ATTACHMENT (github-in/slack-out, no inbound thread_ts) roots
// the thread via the files.uploadV2 path. That root must capture a permalink
// too — the output binding carries no team_id, so OriginLink's reconstruction
// fallback cannot rebuild a Slack link otherwise and the artifact view would
// show no origin.
func TestSender_AttachmentFirstRoot_CapturesPermalink(t *testing.T) {
	c := &fakeSlackClient{postedTS: "888.1", getPermalinkResult: "https://slack.test/archives/C01ABCDEF/p8881"}
	af := &stubAssetFetcher{bytes: []byte("<html>hi</html>"), mime: "text/html", filename: "report.html"}
	s := &slackSender{deps: channelkinds.Deps{AssetFetcher: af}, client: c}
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "reviewbot",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			// no thread_ts: the attachment upload must root the thread first.
			External: map[string]string{"channel_id": "C01ABCDEF"},
		},
	}
	pl := channelevents.OutboundUserMessagePayload{
		Text:        "Here is the review",
		Attachments: []channelevents.AttachmentRef{{RenderName: "ar-1", MIME: "text/html", Filename: "review.html"}},
	}
	plBytes, err := json.Marshal(pl)
	require.NoError(t, err, "marshal payload")
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindUserMessage,
		Session: channelevents.SessionRef{Namespace: "default", Name: "reviewbot"},
		Payload: plBytes,
	}
	res, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")
	assert.Equal(t, "888.1", res.External["thread_ts"], "attachment upload rooted the thread")
	assert.Equal(t, "https://slack.test/archives/C01ABCDEF/p8881", res.External["permalink"],
		"attachment root resolves a permalink for the artifact-view origin link")
	require.Len(t, c.getPermalinkCalls, 1, "getPermalink called once on the attachment root")
}
