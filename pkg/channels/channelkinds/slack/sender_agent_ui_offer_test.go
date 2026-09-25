package slack

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// agentUIOfferEnvelope builds a channelevents.Envelope carrying an
// AgentUIOfferPayload. Mirrors sessionViewOfferEnvelope.
func agentUIOfferEnvelope(t *testing.T, sessionRef string) channelevents.Envelope {
	t.Helper()
	b, err := json.Marshal(channelevents.AgentUIOfferPayload{SessionRef: sessionRef})
	require.NoError(t, err, "marshal AgentUIOfferPayload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindAgentUIOffer,
		Session:     channelevents.SessionRef{Namespace: "demo-ns", Name: "demo-session"},
		PublishedAt: time.Now().UTC(),
		Payload:     b,
	}
}

// stubAgentUIMinter implements channelkinds.AgentUIMinter for tests.
type stubAgentUIMinter struct {
	url string
	err error
}

func (m *stubAgentUIMinter) MintAgentUILink(_ string) (string, error) {
	return m.url, m.err
}

// TestAgentUIOfferSender_Send covers every gate in agentUIOfferSender.Send,
// in the order Send evaluates them. The last three rows are the reason this
// sender exists as its own file rather than a copy-paste of
// sessionViewOfferSender: a nil minter or an empty minted URL must BOTH
// return an error AND post a user-visible notice, unlike session_view_offer's
// quiet skip — see the file doc comment on sender_agent_ui_offer.go.
func TestAgentUIOfferSender_Send(t *testing.T) {
	const (
		chanID    = "C01CHAN"
		threadTS  = "1700000000.000001"
		mintedURL = "https://webd.example.test/sessions?session=demo-ns%2Fdemo-session"
	)
	noChannelSession := channelkinds.SessionInfo{Namespace: "demo-ns", Name: "demo-session", Channel: nil}

	cases := []struct {
		name string
		// client == nil (the zero value of the table row) leaves
		// agentUIOfferSender.client as a true nil interface value, exercising
		// the "unconfigured client" gate rather than a typed-nil pointer.
		client        *fakeSlackClient
		minter        channelkinds.AgentUIMinter
		sessionRef    string
		session       channelkinds.SessionInfo
		wantErr       bool
		wantErrSubstr string
		wantPosts     int
		wantNotice    bool // the loud-failure contract: notice text + no button
	}{
		{
			name:       "happy path posts one message carrying the button",
			client:     &fakeSlackClient{},
			minter:     &stubAgentUIMinter{url: mintedURL},
			sessionRef: "demo-ns/demo-session",
			session:    sessionWithChannel(chanID, threadTS),
			wantPosts:  1,
		},
		{
			name:          "nil client is an error, not a panic",
			client:        nil,
			minter:        &stubAgentUIMinter{url: mintedURL},
			sessionRef:    "demo-ns/demo-session",
			session:       sessionWithChannel(chanID, threadTS),
			wantErr:       true,
			wantErrSubstr: "unconfigured",
			wantPosts:     0,
		},
		{
			name:          "a payload with no sessionRef is refused",
			client:        &fakeSlackClient{},
			minter:        &stubAgentUIMinter{url: mintedURL},
			sessionRef:    "",
			session:       sessionWithChannel(chanID, threadTS),
			wantErr:       true,
			wantErrSubstr: "sessionRef",
			wantPosts:     0,
		},
		{
			name:          "a session with no channel_id is an error, not a panic",
			client:        &fakeSlackClient{},
			minter:        &stubAgentUIMinter{url: mintedURL},
			sessionRef:    "demo-ns/demo-session",
			session:       noChannelSession,
			wantErr:       true,
			wantErrSubstr: "channel_id",
			wantPosts:     0,
		},
		{
			// minter left unset (nil field): a TRUE nil channelkinds.AgentUIMinter
			// interface value, per the standing requirement this sender must be
			// tested against — never a typed-nil concrete pointer, which would
			// pass != nil and panic on first call instead of exercising this gate.
			name:       "a nil minter posts a notice AND errors",
			client:     &fakeSlackClient{},
			minter:     nil,
			sessionRef: "demo-ns/demo-session",
			session:    sessionWithChannel(chanID, threadTS),
			wantErr:    true,
			wantPosts:  1,
			wantNotice: true,
		},
		{
			name:       "an empty minted URL posts a notice AND errors",
			client:     &fakeSlackClient{},
			minter:     &stubAgentUIMinter{url: ""},
			sessionRef: "demo-ns/demo-session",
			session:    sessionWithChannel(chanID, threadTS),
			wantErr:    true,
			wantPosts:  1,
			wantNotice: true,
		},
		{
			// A real minter error (as opposed to a nil minter or an empty-string
			// return) is a distinct gate from the two above: Send returns the
			// wrapped error but does NOT attempt the notice post. Proves the
			// boundary the brief draws between "webd not configured" (loud +
			// notice) and "mint itself failed" (loud only).
			name:       "a mint error surfaces without posting a notice",
			client:     &fakeSlackClient{},
			minter:     &stubAgentUIMinter{err: assert.AnError},
			sessionRef: "demo-ns/demo-session",
			session:    sessionWithChannel(chanID, threadTS),
			wantErr:    true,
			wantPosts:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &agentUIOfferSender{minter: tc.minter}
			if tc.client != nil {
				s.client = tc.client
			}

			_, err := s.Send(context.Background(), tc.session, agentUIOfferEnvelope(t, tc.sessionRef))

			if tc.wantErr {
				// assert, not require: the posts/notice checks below must still run
				// even if this fails, so a regression that drops BOTH halves of the
				// loud-failure contract (error + notice) is reported as two distinct
				// failures rather than the second one going unobserved.
				assert.Error(t, err, "expected Send to fail")
				if tc.wantErrSubstr != "" && err != nil {
					assert.Contains(t, err.Error(), tc.wantErrSubstr, "error names the failing gate")
				}
			} else {
				require.NoError(t, err, "expected Send to succeed")
			}

			var posts int
			if tc.client != nil {
				posts = len(tc.client.postMessageCalls)
			}
			assert.Equal(t, tc.wantPosts, posts, "PostMessage call count")

			if tc.wantNotice {
				require.Len(t, tc.client.postMessageCalls, 1, "the notice is the ONE post on this path")
				assert.Contains(t, tc.client.lastPostedText, "couldn't produce a link",
					"the notice text must be the user-visible copy, not a config key or Go identifier")
				blocks := msgOptionBlocksJSON(t, tc.client.postMessageCalls[0].options)
				assert.NotContains(t, blocks, `"type":"button"`,
					"the notice is text-only — no button when a link couldn't be produced")
			}
		})
	}
}

// TestAgentUIOfferSender_NilMinter_ErrorsAndNotifies pins the sender's
// loud-failure contract on its own: a nil channelkinds.AgentUIMinter must
// produce BOTH halves — the returned error AND the posted notice — asserted
// independently of TestAgentUIOfferSender_Send's table so a future edit
// that drops either half fails on its own dedicated test, not just a shared
// row. The field is left at its zero value (a true nil interface), not a
// typed-nil *stubAgentUIMinter, per the production incident AGENTS.md
// documents where a typed-nil pointer satisfied != nil and panicked on
// first call.
func TestAgentUIOfferSender_NilMinter_ErrorsAndNotifies(t *testing.T) {
	var nilMinter channelkinds.AgentUIMinter // true nil interface, not a typed-nil pointer
	c := &fakeSlackClient{}
	s := &agentUIOfferSender{client: c, minter: nilMinter}

	_, err := s.Send(context.Background(), sessionWithChannel("C01CHAN", "1700000000.000001"),
		agentUIOfferEnvelope(t, "demo-ns/demo-session"))

	// Both are assert, not require, so the two halves of the loud-failure
	// contract are checked independently in a single run — a regression that
	// drops either one is reported, rather than the second going unobserved
	// because the first assertion aborted the test.
	assert.Error(t, err, "a nil minter must return an error, not a quiet skip")
	assert.Len(t, c.postMessageCalls, 1, "a nil minter must still post a user-visible notice")
	assert.Contains(t, c.lastPostedText, "couldn't produce a link",
		"the notice must be the user-visible half of the failure")
}

// TestAgentUIOfferSenderPostsARealButton is the "renders a button, not a
// raw link" requirement, asserted against the WIRE payload rather than the
// builder: msgOptionBlocksJSON replays the captured MsgOptions through
// slack-go's own encoder, because UnsafeApplyMsgOptions does not surface
// blocks at all (it returns form values, and slack-go encodes blocks later).
func TestAgentUIOfferSenderPostsARealButton(t *testing.T) {
	const wantURL = "https://webd.example.test/sessions?session=demo-ns%2Fdemo-session"
	c := &fakeSlackClient{}
	s := &agentUIOfferSender{client: c, minter: &stubAgentUIMinter{url: wantURL}}

	_, err := s.Send(context.Background(), sessionWithChannel("C01CHAN", "1700000000.000001"),
		agentUIOfferEnvelope(t, "demo-ns/demo-session"))
	require.NoError(t, err, "Send must succeed with a wired minter")
	require.Len(t, c.postMessageCalls, 1, "exactly one chat.postMessage")

	blocks := msgOptionBlocksJSON(t, c.postMessageCalls[0].options)
	assert.Contains(t, blocks, `"type":"button"`, "the offer must render a button, not a bare URL in text")
	assert.Contains(t, blocks, wantURL, "the button must carry the minted dashboard URL")
	assert.Contains(t, blocks, agentUIOfferActionID, "the button must carry this offer's own action_id")

	// The copy is the product here: this offer exists to hand a human a button,
	// and its label is the whole of what they read before deciding to click.
	// Pinning it means a reword is a deliberate edit to this expectation rather
	// than a silent change to the one sentence the user actually sees. It also
	// distinguishes this offer from the session-view offer beside it, which
	// shares the block builder and would otherwise be indistinguishable in the
	// timeline.
	assert.Contains(t, blocks, "Open dashboard", "the button's label is user-facing copy and must not drift unnoticed")
	assert.Contains(t, blocks, "live dashboard", "the headline must say what the button opens")
	assert.NotContains(t, blocks, "agentsession", "no control-plane vocabulary in a Slack message")
	assert.NotContains(t, blocks, "AgentUI", "no CRD kind in a Slack message")
}

// TestSubChannelSender_AgentUIOfferReturnsNonNil pins the kind.go wire-up:
// without the case arm the relay resolves nil and drops the envelope with a
// log nobody reads. Mirrors the session_view_offer twin next to it.
func TestSubChannelSender_AgentUIOfferReturnsNonNil(t *testing.T) {
	sender := (&Kind{}).SubChannelSender(channelkinds.SubChannelAgentUIOffer, channelkinds.Deps{})
	require.NotNil(t, sender, "SubChannelSender(agent_ui_offer) must return a non-nil Sender")
}
