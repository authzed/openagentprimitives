package slack

import (
	"context"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAppMentionMissingSuspected pins the gate for the no-silent-errors
// channelsd hint: only a top-level @-mention that arrives while no app_mention
// has ever been seen should be treated as a missing-subscription signal — a
// @-mention after app_mention has worked is the benign deduped duplicate.
func TestAppMentionMissingSuspected(t *testing.T) {
	const bot = "U123BOT"
	cases := []struct {
		name          string
		text          string
		botUserID     string
		sawAppMention bool
		want          bool
	}{
		{"mention + never saw app_mention → suspected", "hey <@U123BOT> do X", bot, false, true},
		{"mention but app_mention already seen → benign duplicate", "hey <@U123BOT> do X", bot, true, false},
		{"no mention → not suspected", "just chatting here", bot, false, false},
		{"empty botUserID → cannot detect", "hey <@U123BOT>", "", false, false},
		{"different user mentioned → not suspected", "hey <@U999OTHER>", bot, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, appMentionMissingSuspected(tc.text, tc.botUserID, tc.sawAppMention))
		})
	}
}

const hintTestBot = "U123BOT"

// newHintListener builds a minimal listener exercising only the missing-
// app_mention hint path: the top-level message.channels drop and the
// app_mention flag flip. A small grace window keeps the deferred check fast.
func newHintListener(t *testing.T, fc *fakeSlackClient, grace time.Duration) *slackListener {
	t.Helper()
	return &slackListener{
		seenEvts:            newEventIDCache(64),
		threads:             newThreadIndex(),
		assistantThreads:    map[string]string{},
		botUserID:           hintTestBot,
		ephemeralClient:     fc,
		appMentionHintGrace: grace,
	}
}

// channelMentionEvent is a top-level (non-thread) message.channels @-mention —
// the event that reaches the missing-subscription drop path.
func channelMentionEvent(channelID, userID, ts, text string) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "message",
			Data: &slackevents.MessageEvent{
				Channel:     channelID,
				ChannelType: "channel",
				User:        userID,
				TimeStamp:   ts,
				Text:        text,
			},
		},
	}
}

// botAppMentionEvent is an app_mention from a bot: it flips sawAppMention (the
// proof the subscription is active) and returns early without the heavy
// session-routing path, so it stands in for "the matching app_mention arrived".
func botAppMentionEvent(channelID, ts string) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "app_mention",
			Data: &slackevents.AppMentionEvent{
				Channel:   channelID,
				BotID:     "B999",
				TimeStamp: ts,
				Text:      "<@" + hintTestBot + "> via bot",
			},
		},
	}
}

func ephemeralText(t *testing.T, call postEphemeralCall) string {
	t.Helper()
	_, values, err := slackapi.UnsafeApplyMsgOptions("xoxb-test", call.channelID, "https://slack.example/", call.options...)
	require.NoError(t, err, "apply msg options")
	return values.Get("text")
}

// TestMissingAppMentionHint_RacingAppMentionSuppresses pins the fix for the
// false positive: when the message.channels @-mention is processed before the
// SAME mention's app_mention (no ordering guarantee on the first mention after
// start), the deferred grace-window check must observe the now-set flag and
// stay silent — the subscription is healthy, so NO "I'm not receiving
// app_mention" ephemeral may be posted.
func TestMissingAppMentionHint_RacingAppMentionSuppresses(t *testing.T) {
	fc := &fakeSlackClient{}
	l := newHintListener(t, fc, 40*time.Millisecond)
	ctx := context.Background()

	// message.channels wins the race (arrives first), then app_mention.
	l.handleEventsAPI(ctx, channelMentionEvent("C1", "U_ALICE", "1700000000.000100", "<@"+hintTestBot+"> hi"))
	l.handleEventsAPI(ctx, botAppMentionEvent("C1", "1700000000.000200"))

	assert.Never(t, func() bool {
		return len(fc.snapshotEphemeralCalls()) > 0
	}, 250*time.Millisecond, 10*time.Millisecond,
		"a racing app_mention must suppress the missing-subscription hint")
}

// TestMissingAppMentionHint_NoAppMentionEverWarns pins the genuine case: when
// no app_mention ever arrives, the grace-window check fires exactly one
// per-channel hint, targeting the first mentioner.
func TestMissingAppMentionHint_NoAppMentionEverWarns(t *testing.T) {
	fc := &fakeSlackClient{}
	l := newHintListener(t, fc, 40*time.Millisecond)
	ctx := context.Background()

	// Two top-level @-mentions, no app_mention ever (missing subscription).
	l.handleEventsAPI(ctx, channelMentionEvent("C1", "U_ALICE", "1700000000.000100", "<@"+hintTestBot+"> hi"))
	l.handleEventsAPI(ctx, channelMentionEvent("C1", "U_BOB", "1700000000.000300", "<@"+hintTestBot+"> hello"))

	require.Eventually(t, func() bool {
		return len(fc.snapshotEphemeralCalls()) == 1
	}, 2*time.Second, 10*time.Millisecond,
		"a sustained absence of app_mention must warn exactly once per channel")

	got := fc.snapshotEphemeralCalls()[0]
	assert.Equal(t, "C1", got.channelID)
	assert.Equal(t, "U_ALICE", got.userID, "hint targets the first mentioner")
	assert.Contains(t, ephemeralText(t, got), "app_mention", "hint names the missing subscription")
}
