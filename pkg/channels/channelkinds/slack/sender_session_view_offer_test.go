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

// sessionViewOfferEnvelope builds a channelevents.Envelope carrying a
// SessionViewOfferPayload. Mirrors liveViewOfferEnvelope.
func sessionViewOfferEnvelope(t *testing.T, sessionRef string) channelevents.Envelope {
	t.Helper()
	b, err := json.Marshal(channelevents.SessionViewOfferPayload{SessionRef: sessionRef})
	require.NoError(t, err, "marshal SessionViewOfferPayload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindSessionViewOffer,
		Session:     channelevents.SessionRef{Namespace: "ns1", Name: "sess1"},
		PublishedAt: time.Now().UTC(),
		Payload:     b,
	}
}

// stubSessionViewMinter implements channelkinds.SessionViewMinter for tests.
type stubSessionViewMinter struct {
	url string
	err error
}

func (m *stubSessionViewMinter) MintSessionViewLink(_ string, _ identity.Principal, _ string) (string, error) {
	return m.url, m.err
}

// TestSessionViewOfferSender_PostsAnchor verifies the happy path: when a
// minter is configured and the client is set up, Send posts a message
// containing the minted URL.
func TestSessionViewOfferSender_PostsAnchor(t *testing.T) {
	const (
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
		viewURL  = "https://webd.example/session-view/ns1/sess1"
	)
	c := &fakeSlackClient{}
	s := &sessionViewOfferSender{
		client: c,
		minter: &stubSessionViewMinter{url: viewURL},
	}

	env := sessionViewOfferEnvelope(t, "ns1/sess1")
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send should succeed")
	require.Len(t, c.postMessageCalls, 1, "expected 1 PostMessage call")
	assert.Equal(t, chanID, c.postMessageCalls[0].channelID, "channelID")

	// UnsafeApplyMsgOptions doesn't surface Blocks (see the same note on
	// other senders' tests in this package), so verify the rendered blocks
	// directly, independent of the MsgOption wire encoding.
	blocks := buildSessionViewOfferBlocks(viewURL)
	actions, ok := blocks[1].(*slackapi.ActionBlock)
	require.True(t, ok, "block[1] must be *ActionBlock")
	require.Len(t, actions.Elements.ElementSet, 1, "actions block must have exactly one button")
	btn, ok := actions.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	require.True(t, ok, "actions element must be *ButtonBlockElement")
	assert.Equal(t, viewURL, btn.URL, "button URL must be the minted session-view link")
}

// TestSessionViewOfferSender_NilMinter_SkipsWithNoError verifies that when
// the SessionViewMinter is nil (webd not configured), Send returns nil
// without posting anything to Slack — a graceful no-op.
func TestSessionViewOfferSender_NilMinter_SkipsWithNoError(t *testing.T) {
	c := &fakeSlackClient{}
	s := &sessionViewOfferSender{
		client: c,
		minter: nil, // webd not configured
	}

	env := sessionViewOfferEnvelope(t, "ns1/sess1")
	sess := sessionWithChannel("C01", "1700000000.000001")

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "nil minter must not return an error")
	assert.Empty(t, c.postMessageCalls, "PostMessage must not be called when minter is nil")
}

// TestSessionViewOfferSender_NilClient_ReturnsError verifies that a nil
// Slack client surfaces an error rather than panicking.
func TestSessionViewOfferSender_NilClient_ReturnsError(t *testing.T) {
	s := &sessionViewOfferSender{
		client: nil,
		minter: &stubSessionViewMinter{url: "https://webd.example/session-view/ns1/sess1"},
	}
	env := sessionViewOfferEnvelope(t, "ns1/sess1")
	_, err := s.Send(context.Background(), sessionWithChannel("C01", "ts"), env)
	require.Error(t, err, "nil client must return an error")
	assert.Contains(t, err.Error(), "unconfigured", "error mentions unconfigured client")
}

// TestSessionViewOfferSender_MissingSessionRef_ReturnsError verifies validation.
func TestSessionViewOfferSender_MissingSessionRef_ReturnsError(t *testing.T) {
	c := &fakeSlackClient{}
	s := &sessionViewOfferSender{
		client: c,
		minter: &stubSessionViewMinter{url: "https://webd.example/session-view/ns1/sess1"},
	}
	env := sessionViewOfferEnvelope(t, "")
	_, err := s.Send(context.Background(), sessionWithChannel("C01", "ts"), env)
	require.Error(t, err, "missing sessionRef must return an error")
	assert.Contains(t, err.Error(), "sessionRef", "error mentions sessionRef")
}

// TestSessionViewOfferSender_NoChannelID_ReturnsError verifies that a
// session with no channel_id surfaces an error instead of panicking.
func TestSessionViewOfferSender_NoChannelID_ReturnsError(t *testing.T) {
	c := &fakeSlackClient{}
	s := &sessionViewOfferSender{
		client: c,
		minter: &stubSessionViewMinter{url: "https://webd.example/session-view/ns1/sess1"},
	}
	env := sessionViewOfferEnvelope(t, "ns1/sess1")
	sess := channelkinds.SessionInfo{
		Namespace: "ns1", Name: "sess1",
		Channel: nil, // no channel binding
	}
	_, err := s.Send(context.Background(), sess, env)
	require.Error(t, err, "missing channel_id must return an error")
	assert.Contains(t, err.Error(), "channel_id", "error mentions channel_id")
}

// TestSessionViewOfferSender_MintError_ReturnsError verifies a minter error
// surfaces rather than being swallowed.
func TestSessionViewOfferSender_MintError_ReturnsError(t *testing.T) {
	c := &fakeSlackClient{}
	s := &sessionViewOfferSender{
		client: c,
		minter: &stubSessionViewMinter{err: assert.AnError},
	}
	env := sessionViewOfferEnvelope(t, "ns1/sess1")
	_, err := s.Send(context.Background(), sessionWithChannel("C01", "ts"), env)
	require.Error(t, err, "mint error must surface")
	assert.Empty(t, c.postMessageCalls, "PostMessage must not be called when mint fails")
}

// TestBuildSessionViewOfferBlocks verifies the offer renders a URL button —
// no interaction, action_id == sessionViewOfferActionID, URL == the minted
// link.
func TestBuildSessionViewOfferBlocks(t *testing.T) {
	const viewURL = "https://webd.example/session-view/ns1/sess1"
	blocks := buildSessionViewOfferBlocks(viewURL)
	require.Len(t, blocks, 2, "expected 2 blocks: section + actions")

	actions, ok := blocks[1].(*slackapi.ActionBlock)
	require.True(t, ok, "block[1] must be *ActionBlock")
	require.Len(t, actions.Elements.ElementSet, 1, "actions block must have exactly one element")

	btn, ok := actions.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	require.True(t, ok, "actions element must be *ButtonBlockElement")
	assert.Equal(t, viewURL, btn.URL, "button must carry the minted URL directly (durable plain-path link)")
	assert.Equal(t, sessionViewOfferActionID, btn.ActionID, "action_id must be the session-view action id")
	assert.Equal(t, slackapi.StylePrimary, btn.Style, "button style must be primary")
}

// TestSubChannelSender_SessionViewOfferReturnsNonNil verifies that
// Kind.SubChannelSender("session_view_offer", ...) returns a non-nil
// Sender — i.e. the wire-up in kind.go is correct.
func TestSubChannelSender_SessionViewOfferReturnsNonNil(t *testing.T) {
	k := &Kind{}
	sender := k.SubChannelSender("session_view_offer", channelkinds.Deps{})
	require.NotNil(t, sender, "SubChannelSender(session_view_offer) must return a non-nil Sender")
}
