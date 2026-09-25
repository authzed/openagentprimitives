package slack

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// liveViewOfferEnvelope builds a channelevents.Envelope carrying a
// LiveViewOfferPayload. Mirrors the pattern in portalAccessEnvelope.
func liveViewOfferEnvelope(t *testing.T, artifactID, rendererKind string) channelevents.Envelope {
	t.Helper()
	b, err := json.Marshal(channelevents.LiveViewOfferPayload{
		ArtifactID:   artifactID,
		RendererKind: rendererKind,
	})
	require.NoError(t, err, "marshal LiveViewOfferPayload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindLiveViewOffer,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "sess-1"},
		PublishedAt: time.Now().UTC(),
		Payload:     b,
	}
}

// stubArtifactViewMinter implements channelkinds.ArtifactViewMinter for tests.
type stubArtifactViewMinter struct {
	url string
	err error
}

func (m *stubArtifactViewMinter) MintArtifactViewLink(_, _ string, _ identity.Principal, _ string) (string, error) {
	return m.url, m.err
}

// TestLiveViewOfferSender_PostsInteractionButton verifies the happy path: when a
// minter is configured and the client is set up, Send posts a message with
// a button to the channel/thread.
func TestLiveViewOfferSender_PostsInteractionButton(t *testing.T) {
	const (
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
		viewURL  = "https://webd.example.com/artifact-view?d=abc&sig=def"
	)
	c := &fakeSlackClient{}
	s := &liveViewOfferSender{
		client: c,
		minter: &stubArtifactViewMinter{url: viewURL},
	}

	env := liveViewOfferEnvelope(t, "art-abc123", "html")
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send should succeed")
	require.Len(t, c.postMessageCalls, 1, "expected 1 PostMessage call")
	assert.Equal(t, chanID, c.postMessageCalls[0].channelID, "channelID")
}

// TestLiveViewOfferSender_DMThreadsUnderLastInbound: a plain DM channel binding
// has channel_id but no thread_ts (handleDM stamps none), so reading
// External["thread_ts"] posts the live-view offer top-level. The sender must
// consult effectiveOutboundThreadTS, which prefers the per-turn
// LastInboundTSAnnotationKey annotation the listener stamps on every inbound.
func TestLiveViewOfferSender_DMThreadsUnderLastInbound(t *testing.T) {
	const (
		chanID        = "C01CHAN"
		lastInboundTS = "1700000000.000700"
		viewURL       = "https://webd.example.com/artifact-view?d=abc&sig=def"
	)
	c := &fakeSlackClient{}
	s := &liveViewOfferSender{
		client: c,
		minter: &stubArtifactViewMinter{url: viewURL},
	}

	env := liveViewOfferEnvelope(t, "art-abc123", "html")
	sess := sessionWithChannel(chanID, "") // DM binding: no thread_ts ever written
	sess.Annotations = map[string]string{LastInboundTSAnnotationKey: lastInboundTS}

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send should succeed")
	require.Len(t, c.postMessageCalls, 1, "expected 1 PostMessage call")
	assert.Equal(t, chanID, c.postMessageCalls[0].channelID, "channelID")

	_, vals, err := slackapi.UnsafeApplyMsgOptions("test-token", chanID,
		"http://test.invalid/", c.postMessageCalls[0].options...)
	require.NoError(t, err)
	assert.Equal(t, lastInboundTS, vals.Get("thread_ts"),
		"live-view offer must thread under the per-turn LastInboundTS annotation even though External[thread_ts] is empty")
}

// TestLiveViewOfferSender_NilMinter_SurfacesInsteadOfSkipping covers the
// misconfiguration this sender used to swallow: no ArtifactViewMinter means
// webd was never wired, so a "View live" button could never resolve and none is
// posted.
//
// Posting nothing is right; doing it QUIETLY is not. The agent explicitly
// offered the user a live view and the user is waiting for it, so the failure
// has to leave a trace an operator can grep for — the same answer `local` and
// `browser` already give from their own live-view senders.
func TestLiveViewOfferSender_NilMinter_SurfacesInsteadOfSkipping(t *testing.T) {
	c := &fakeSlackClient{}
	s := &liveViewOfferSender{
		client: c,
		minter: nil, // webd not configured
	}

	env := liveViewOfferEnvelope(t, "art-abc123", "html")
	sess := sessionWithChannel("C01", "1700000000.000001")

	_, err := s.Send(context.Background(), sess, env)
	require.Error(t, err, "a nil minter is a misconfiguration, not a feature toggle — it must not return success")
	assert.Contains(t, err.Error(), "minter", "the error has to name what is missing")
	assert.Empty(t, c.postMessageCalls, "and it must still post nothing: an unresolvable button is worse than none")
}

// TestLiveViewOfferSender_NilClient_ReturnsError verifies that a nil Slack
// client surfaces an error rather than panicking.
func TestLiveViewOfferSender_NilClient_ReturnsError(t *testing.T) {
	s := &liveViewOfferSender{
		client: nil,
		minter: &stubArtifactViewMinter{url: "https://webd.example.com/artifact-view?d=x&sig=y"},
	}
	env := liveViewOfferEnvelope(t, "art-abc123", "html")
	_, err := s.Send(context.Background(), sessionWithChannel("C01", "ts"), env)
	require.Error(t, err, "nil client must return an error")
	assert.Contains(t, err.Error(), "unconfigured", "error mentions unconfigured client")
}

// TestLiveViewOfferSender_MissingArtifactID_ReturnsError verifies validation.
func TestLiveViewOfferSender_MissingArtifactID_ReturnsError(t *testing.T) {
	c := &fakeSlackClient{}
	s := &liveViewOfferSender{
		client: c,
		minter: &stubArtifactViewMinter{url: "https://webd.example.com/artifact-view?d=x&sig=y"},
	}
	// Envelope with empty artifactId.
	env := liveViewOfferEnvelope(t, "", "html")
	_, err := s.Send(context.Background(), sessionWithChannel("C01", "ts"), env)
	require.Error(t, err, "missing artifactId must return an error")
	assert.Contains(t, err.Error(), "artifactId", "error mentions artifactId")
}

// TestLiveViewOfferSender_NoChannelID_ReturnsError verifies that a session
// with no channel_id surfaces an error instead of panicking.
func TestLiveViewOfferSender_NoChannelID_ReturnsError(t *testing.T) {
	c := &fakeSlackClient{}
	s := &liveViewOfferSender{
		client: c,
		minter: &stubArtifactViewMinter{url: "https://webd.example.com/artifact-view?d=x&sig=y"},
	}
	env := liveViewOfferEnvelope(t, "art-1", "html")
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "sess-1",
		Channel: nil, // no channel binding
	}
	_, err := s.Send(context.Background(), sess, env)
	require.Error(t, err, "missing channel_id must return an error")
	assert.Contains(t, err.Error(), "channel_id", "error mentions channel_id")
}

// TestBuildLiveViewOfferBlocks verifies the offer renders an INTERACTION
// button — no URL, action_id == liveViewActionID, and a value JSON that
// carries the artifact identity + channel/thread for the click handler.
func TestBuildLiveViewOfferBlocks(t *testing.T) {
	blocks, err := buildLiveViewOfferBlocks("art-abc123", "default/sess-1", "C01CHAN", "1700000000.000001")
	require.NoError(t, err, "buildLiveViewOfferBlocks")
	require.Len(t, blocks, 2, "expected 2 blocks: section + actions")

	actions, ok := blocks[1].(*slackapi.ActionBlock)
	require.True(t, ok, "block[1] must be *ActionBlock")
	require.Len(t, actions.Elements.ElementSet, 1, "actions block must have exactly one element")

	btn, ok := actions.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	require.True(t, ok, "actions element must be *ButtonBlockElement")
	assert.Empty(t, btn.URL, "interaction button must NOT carry a URL (URL buttons never fire interactions)")
	assert.Equal(t, liveViewActionID, btn.ActionID, "action_id must be the live-view action id")
	assert.Equal(t, slackapi.StylePrimary, btn.Style, "button style must be primary")

	var v liveViewButtonValue
	require.NoError(t, json.Unmarshal([]byte(btn.Value), &v), "button value must be JSON")
	assert.Equal(t, "live_view", v.V, "value discriminator")
	assert.Equal(t, "art-abc123", v.A, "value artifactID")
	assert.Equal(t, "default/sess-1", v.S, "value sessionRef")
	assert.Equal(t, "C01CHAN", v.C, "value channel_id")
	assert.Equal(t, "1700000000.000001", v.T, "value thread_ts")
}

// TestSubChannelSender_LiveViewOfferReturnsNonNil verifies that
// Kind.SubChannelSender("live_view_offer", ...) returns a non-nil
// Sender — i.e. the wire-up in kind.go is correct.
func TestSubChannelSender_LiveViewOfferReturnsNonNil(t *testing.T) {
	k := &Kind{}
	sender := k.SubChannelSender("live_view_offer", channelkinds.Deps{})
	require.NotNil(t, sender, "SubChannelSender(live_view_offer) must return a non-nil Sender")
}
