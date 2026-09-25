// pkg/channels/channelkinds/slack/notice_lead_inert_test.go
//
// The notice Lead's DEGRADE sinks.
//
// Lead is the one payload field escapePublisherPayload deliberately does not
// neutralise, because its sink on the card is a rich_text element Slack
// renders literally — so inert.go's rule for it is per-SINK: escape where the
// surface parses markup, leave it raw where it does not. Three sinks broke
// that rule while their siblings on the same payload (noticeNotifyText,
// buildNoticeFallbackBlocks) kept it, and one inert rendering beside one live
// one for the same value is worse than either alone.
//
// Honest scope: all three are DEGRADE paths, reached only when a notice fails
// to render, and all 26 notice.New call sites pass a literal Lead today — so
// nothing attacker-controlled reaches them now. This is the per-sink rule
// being applied uniformly, not a live exposure.
package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// unrenderableNotice is a notice whose category is not in the registry, so
// Payload() fails and every caller takes its degrade branch — the branch that
// posts Args().Lead directly. Its Lead carries forged markup so the sink's
// treatment of it is observable.
func unrenderableNotice() *notice.Notice {
	return notice.New("not-a-registered-category", notice.Args{
		Lead:     "Couldn't process your message " + lureLink,
		Body:     "something went wrong",
		NextStep: "Send it again.",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

// TestNoticeLead_DegradePathsAgreeWithTheirSiblings walks the three sinks that
// posted the Lead raw. Each is asserted through the listener's own API double,
// i.e. at the point the message actually leaves.
func TestNoticeLead_DegradePathsAgreeWithTheirSiblings(t *testing.T) {
	cases := []struct {
		name string
		post func(t *testing.T, l *slackListener)
	}{
		{
			name: "postNoticeEphemeral's unrenderable-payload fallback: the lead is inert",
			post: func(t *testing.T, l *slackListener) {
				t.Helper()
				require.NoError(t, l.postNoticeEphemeral(context.Background(), "C1", "U1",
					channelevents.SessionRef{Namespace: "default", Name: "s1"}, unrenderableNotice()))
			},
		},
		{
			name: "postNoticeInThread's unrenderable-payload fallback: the lead is inert",
			post: func(t *testing.T, l *slackListener) {
				t.Helper()
				l.postNoticeInThread(context.Background(), "C1", "100.1",
					channelevents.SessionRef{Namespace: "default", Name: "s1"}, unrenderableNotice())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, posts := recordingPostMessageAPI(t)
			l := &slackListener{api: api}
			tc.post(t, l)

			all := posts()
			require.Len(t, all, 1, "the degrade path still posts — the user is waiting on it")
			assert.NotContains(t, all[0].text, lureLink,
				"a degraded rendering must be no more live than the one it degraded from")
			assert.Contains(t, all[0].text, "&lt;", "...and must still show what the lead said")
		})
	}
}

// TestPostNoticeReturningTS_DegradedLeadIsInert is the third sink. It is a
// package-level function on the slackClient seam rather than a listener
// method, which is precisely why it was missed: it is not next to the other
// two.
func TestPostNoticeReturningTS_DegradedLeadIsInert(t *testing.T) {
	fc := &fakeSlackClient{}

	ts, err := postNoticeReturningTS(context.Background(), fc, "C1", unrenderableNotice(),
		channelevents.SessionRef{Namespace: "default", Name: "s1"}, "req-1")
	require.NoError(t, err, "a render failure still posts the lead — this call is establishing a thread root")
	assert.NotEmpty(t, ts, "the ts must come back or the thread has no root")

	require.Len(t, fc.postMessageCalls, 1)
	assert.NotContains(t, renderTextFromOpts(t, fc.postMessageCalls[0].options), lureLink)
}

// TestSharedPosters_PassTextThroughVerbatim is the double-escape guard, and
// the reason the three fixes go at their CALL SITES rather than inside
// postEphemeralText / postInThread.
//
// Those two helpers each have a second caller that hands them
// noticeNotifyText(pl) — text whose Lead is ALREADY inert. Moving the escape
// into the helper would render "&amp;lt;" to those readers, showing them the
// entity instead of the character. So the helpers' contract is: they escape
// nothing, and every caller arrives with text already fit for a mrkdwn sink.
//
// Asserted on the helpers directly. Driving it through postNoticeInThread with
// a renderable notice would prove nothing — that path calls the API itself and
// never reaches these helpers at all, so the assertion would pass whether or
// not the escape had been moved.
func TestSharedPosters_PassTextThroughVerbatim(t *testing.T) {
	const alreadyInert = "Couldn't process your message &lt;https://x.invalid|y&gt;"

	cases := []struct {
		name string
		post func(t *testing.T, l *slackListener)
	}{
		{
			name: "postInThread: text arrives inert and is posted unchanged",
			post: func(t *testing.T, l *slackListener) {
				t.Helper()
				l.postInThread(context.Background(), "C1", "100.1", alreadyInert)
			},
		},
		{
			name: "postEphemeralText: text arrives inert and is posted unchanged",
			post: func(t *testing.T, l *slackListener) {
				t.Helper()
				require.NoError(t, l.postEphemeralText(context.Background(), "C1", "U1", alreadyInert))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, posts := recordingPostMessageAPI(t)
			l := &slackListener{api: api}
			tc.post(t, l)

			all := posts()
			require.Len(t, all, 1)
			assert.Equal(t, alreadyInert, all[0].text,
				"the shared posters escape nothing — their callers arrive already inert")
		})
	}
}
