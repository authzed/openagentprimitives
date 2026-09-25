// pkg/channels/channelkinds/slack/interaction_lead_text_test.go
//
// The Lead's OTHER sinks: the message's plain-text `text` field, and the
// degraded mrkdwn section.
//
// interaction_inertness_test.go covers the CARD — where the Lead rides a
// rich_text element Slack renders literally, which is why
// escapePublisherPayload leaves it live. This file covers the sinks the same
// Lead reaches on every delivery: `MsgOptionText`, the notification preview /
// channel-list line, which Slack DOES parse as mrkdwn, and
// buildNoticeFallbackBlocks' section. A `<!channel>` or `<url|label>` there is
// a live ping / a forged platform link, and postBroadcast's copy of it is a
// PUBLIC chat.postMessage.
//
// These tests observe the real form values via slackapi.UnsafeApplyMsgOptions,
// which surfaces `text`; it deliberately omits blocks, so nothing here asserts
// on blocks (the block assertions live in interaction_inertness_test.go, which
// calls the builders directly).
package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// leadLure is a Lead shaped like the ones real publishers supply from
// semi-trusted text — info_leakage's summarizer output, credential_update's
// provider Reason, provider_error_retry's FailureReason — carrying both a
// channel-wide ping and a forged platform action.
const leadLure = "<!channel> " + lureLink

// msgOptionText applies opts the way the real client would and returns the
// `text` form value — the notification preview Slack parses as mrkdwn.
func msgOptionText(t *testing.T, channelID string, opts []slackapi.MsgOption) string {
	t.Helper()
	_, vals, err := slackapi.UnsafeApplyMsgOptions("test-token", channelID, "http://test.invalid/", opts...)
	require.NoError(t, err, "UnsafeApplyMsgOptions")
	return vals.Get("text")
}

// TestInteractionSender_Request_LeadIsInertInNotificationText pins the
// notification-text half of the Lead's inertness on every delivery shape
// sendRequest has: the participants broadcast (a PUBLIC channel post), the
// requester ephemeral, and the DM fallback.
func TestInteractionSender_Request_LeadIsInertInNotificationText(t *testing.T) {
	const (
		email    = "approver@corp.example"
		slackID  = "U_APPROVER"
		chanID   = "C_LEAD"
		threadTS = "1700000000.000009"
	)
	requester := channelevents.InteractionAudience{
		Scope: channelevents.AudienceRequester,
		Requester: &channelevents.ExternalIdentity{
			Kind: "slack", ExternalID: identity.RawExternalID(slackID), Email: identity.Email(email),
		},
	}
	cases := []struct {
		name     string
		audience channelevents.InteractionAudience
		sess     channelkinds.SessionInfo
		// sentText pulls the `text` the delivery actually sent.
		sentText func(t *testing.T, c *fakeSlackClient) string
	}{
		{
			name:     "participants broadcast: the PUBLIC post's text carries no live markup",
			audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
			sess:     sessionWithChannel(chanID, threadTS),
			sentText: func(t *testing.T, c *fakeSlackClient) string {
				t.Helper()
				require.Len(t, c.postMessageCalls, 1, "the broadcast posts once, publicly")
				return msgOptionText(t, c.postMessageCalls[0].channelID, c.postMessageCalls[0].options)
			},
		},
		{
			name:     "requester ephemeral: the preview text carries no live markup",
			audience: requester,
			sess:     sessionWithChannel(chanID, threadTS),
			sentText: func(t *testing.T, c *fakeSlackClient) string {
				t.Helper()
				require.Len(t, c.postEphemeralCalls, 1, "one ephemeral to the requester")
				return msgOptionText(t, c.postEphemeralCalls[0].channelID, c.postEphemeralCalls[0].options)
			},
		},
		{
			name:     "DM fallback: the DM's text carries no live markup",
			audience: requester,
			sess:     sessionDMOnly(),
			sentText: func(t *testing.T, c *fakeSlackClient) string {
				t.Helper()
				require.Len(t, c.postMessageCalls, 1, "one DM post")
				return msgOptionText(t, c.postMessageCalls[0].channelID, c.postMessageCalls[0].options)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeClientWithUser(email, slackID)
			s := &interactionSender{client: c}

			p := channelevents.InteractionRequestPayload{
				Category:   "credential_update",
				RequestRef: "req-lead",
				Lead:       leadLure,
				Body:       "The agent said: my push kept failing.",
				Audience:   tc.audience,
			}
			_, err := s.Send(context.Background(), tc.sess, interactionEnvelope(t, channelevents.KindInteractionRequest, p))
			require.NoError(t, err, "Send")

			got := tc.sentText(t, c)
			assert.NotContains(t, got, "<!channel>",
				"a publisher Lead must not open a channel-wide ping in the message text Slack parses")
			assert.NotContains(t, got, "<https://",
				"a publisher Lead must not open a Slack link span in the message text")
			assert.Contains(t, got, "&lt;!channel&gt;",
				"...and the attempt must stay visible, not be deleted")
		})
	}
}

// TestInteractionSender_Request_DMThreadTitleKeepsTheRawLead is the
// false-positive guard on the fix above: the escape belongs at the `text`
// field, which Slack parses, and NOT at the DM thread title, which renders
// characters literally — escaping there would show a reader "&amp;" for an
// ordinary ampersand, the same regression that keeps the rich_text title raw.
func TestInteractionSender_Request_DMThreadTitleKeepsTheRawLead(t *testing.T) {
	const (
		email   = "owner@corp.example"
		slackID = "U_OWNER"
		lead    = "Share the Q3 P&L summary with finance?"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c}

	p := channelevents.InteractionRequestPayload{
		Category:   "info_leakage",
		RequestRef: "req-title",
		Lead:       lead,
		Audience: channelevents.InteractionAudience{
			Scope: channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{
				Kind: "slack", ExternalID: identity.RawExternalID(slackID), Email: identity.Email(email),
			},
		},
	}
	_, err := s.Send(context.Background(), sessionDMOnly(), interactionEnvelope(t, channelevents.KindInteractionRequest, p))
	require.NoError(t, err, "Send")

	require.Len(t, c.setTitleCalls, 1, "the DM's thread is titled so it surfaces on mobile")
	assert.Equal(t, lead, c.setTitleCalls[0].title,
		"a thread title is not a markup surface; escaping it would show the reader an entity")
}

// TestBuildNoticeFallbackBlocks_LeadIsInert covers the degrade rendering, where
// the Lead lands in a mrkdwn section rather than a rich_text element — the one
// place the same string WOULD render as a live link. escapePublisherPayload
// never escapes Lead (its container sink renders literally), so this builder is
// the last place its own mrkdwn sink can make it inert.
//
// The reason is per-SINK, not per-trust: this builder is reached only from the
// *notice.Notice paths (noticeMsgOptions, notice_post.go's degrade branches),
// whose Args are trusted by contract, and sendRequest never degrades to it.
// It escapes anyway because its section is mrkdwn — the same reason
// noticeNotifyText, the other mrkdwn sink on that same path, does
// (TestNoticeNotifyText_LeadIsInert). Body/NextStep/Fields stay live there:
// their sink is uniformly mrkdwn, so their trust decision is made once.
func TestBuildNoticeFallbackBlocks_LeadIsInert(t *testing.T) {
	p := channelevents.InteractionRequestPayload{
		Category:   "credential_update",
		RequestRef: "req-fallback",
		Lead:       leadLure,
	}
	got := firstSectionText(t, buildNoticeFallbackBlocks(p, channelinteractions.ToneRoutine, false, "default/sess-1"))
	assert.NotContains(t, got, lureLink,
		"the fallback section is mrkdwn: an unescaped Lead renders as a live hyperlink")
	assert.NotContains(t, got, "<!channel>",
		"the fallback section is mrkdwn: an unescaped Lead can ping the channel")
	assert.Contains(t, got, "&lt;https://attacker.example.invalid/update|Update credential&gt;",
		"...and the attempt must stay visible")
}

// TestNoticeNotifyText_LeadIsInert closes the disagreement the fallback-blocks
// escape exposed: buildNoticeFallbackBlocks (above) makes a Lead inert on the
// degrade path while noticeNotifyText — the OTHER mrkdwn sink on that exact
// same path, and the only sink on its non-degraded one — left the same string
// live. Both are MsgOptionText(_, false), which means slack-go does not escape
// the field for us, so the two sinks disagreed about the same payload.
//
// The rule they now share is per-SINK, not per-trust: the Lead is the one
// field escapePublisherPayload cannot make inert at the payload boundary,
// because its card sink is a rich_text element Slack renders literally — so
// every mrkdwn sink it reaches escapes it itself. That is lossless (Slack
// decodes the entities back when it renders), and a notice's Body / NextStep /
// Fields — whose sink is uniformly mrkdwn, and where the trust decision is
// therefore made once — stay live.
func TestNoticeNotifyText_LeadIsInert(t *testing.T) {
	p := noticeReq("demo_notice")
	p.Lead = leadLure

	got := noticeNotifyText(p)
	assert.NotContains(t, got, "<!channel>",
		"MsgOptionText(_, false) is parsed as mrkdwn: an unescaped Lead opens a channel-wide ping")
	assert.NotContains(t, got, lureLink,
		"...and a forged platform action")
	assert.Contains(t, got, "&lt;!channel&gt;", "...and the attempt must stay visible, not be deleted")
	assert.Contains(t, got, "Start a new thread to try again.",
		"NextStep is not a Lead: its sink is uniformly mrkdwn, so a trusted notice keeps it live")
}

// The delivery-level proof, on the path a notice actually takes: every
// noticeMsgOptions caller posts the same text field, degraded or not.
func TestNoticeMsgOptions_LeadIsInertInBothRenderings(t *testing.T) {
	registerNoticeCat(t, "demo_notice", channelinteractions.ToneCritical, true, "")
	n := notice.New("demo_notice", notice.Args{
		Lead:     leadLure,
		Body:     "It ran out of memory.",
		NextStep: "Start a new thread to try again.",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})

	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "container rendering", true: "degraded rendering"}[fallback], func(t *testing.T) {
			opts, err := noticeMsgOptions(n, channelevents.SessionRef{Namespace: "agents", Name: "demo"}, "req-1", fallback)
			require.NoError(t, err, "noticeMsgOptions")

			got := msgOptionText(t, "C_NOTICE", opts)
			assert.NotContains(t, got, "<!channel>", "the notification text must not open a channel-wide ping")
			assert.NotContains(t, got, lureLink, "the notification text must not open a forged platform action")
			assert.Contains(t, got, "&lt;!channel&gt;", "...and the attempt must stay visible")
		})
	}
}

// TestDeliverEphemeralOrDM_NotifyTextGoesThroughTheOneDoor closes the last
// MsgOptionText(_, false) in the interaction sender that did not route through
// notifyOption. Both of its branches — ephemeral when the session has channel
// context, DM when it does not — post the same text field, and escape=false
// means slack-go does not escape it for us.
//
// Its one caller (sendApplied's credential_link "connected" notice) passes a
// literal today, so nothing attacker-controlled reaches it: this pins that the
// door is single, not that a live payload was landing here.
func TestDeliverEphemeralOrDM_NotifyTextGoesThroughTheOneDoor(t *testing.T) {
	const notify = "A credential was just linked " + lureLink

	t.Run("channel context: the ephemeral post's text field is inert", func(t *testing.T) {
		c := &fakeSlackClient{}
		s := &interactionSender{client: c}
		require.NoError(t, s.deliverEphemeralOrDM(context.Background(),
			sessionWithChannel("C_CRED", "1700000000.000001"), "U_REQ", nil, notify))

		require.Len(t, c.postEphemeralCalls, 1, "ephemeral post")
		got := msgOptionText(t, c.postEphemeralCalls[0].channelID, c.postEphemeralCalls[0].options)
		assert.NotContains(t, got, lureLink, "the notification text must not carry a live forged action")
		assert.Contains(t, got, "&lt;https://attacker.example.invalid/update|Update credential&gt;",
			"...and the attempt must stay visible")
	})

	t.Run("DM-only session: the DM post's text field is inert", func(t *testing.T) {
		c := &fakeSlackClient{openConvChannelID: "D_REQ"}
		s := &interactionSender{client: c}
		require.NoError(t, s.deliverEphemeralOrDM(context.Background(),
			channelkinds.SessionInfo{Namespace: "default", Name: "sess-1"}, "U_REQ", nil, notify))

		require.Len(t, c.postMessageCalls, 1, "DM post")
		got := msgOptionText(t, c.postMessageCalls[0].channelID, c.postMessageCalls[0].options)
		assert.NotContains(t, got, lureLink, "the notification text must not carry a live forged action")
		assert.Contains(t, got, "&lt;https://attacker.example.invalid/update|Update credential&gt;",
			"...and the attempt must stay visible")
	})
}
