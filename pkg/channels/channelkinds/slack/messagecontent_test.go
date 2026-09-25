package slack

import (
	"context"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// alertMsg builds the shape an alerting webhook posts into a channel: a
// bot_message with NO top-level text and NO blocks — every human-readable
// field lives in a legacy attachment. This is the payload that reached the
// agent as a bare "unknown: " line.
func alertMsg(ts string) slackapi.Message {
	m := slackapi.Message{}
	m.Timestamp = ts
	m.BotID = "B_ALERTS"
	m.SubType = "bot_message"
	m.Attachments = []slackapi.Attachment{{
		Title:    "[FIRING:2]",
		Fallback: "[FIRING:2] demo-stack-us-east-1 tenant (warning)",
		Fields: []slackapi.AttachmentField{
			{Title: "Stack name", Value: "demo-stack-us-east-1", Short: true},
			{Title: "Severity", Value: ":warning: Warning", Short: true},
			{Title: "Summary", Value: "Deployment has not matched the expected number of replicas.", Short: true},
			{Title: "Alerts", Value: "• *KubePodCrashLooping*\n    Pod tenant/demo-cache is in CrashLoopBackOff.", Short: false},
		},
	}}
	return m
}

func blockMsg(ts, text string) slackapi.Message {
	m := slackapi.Message{}
	m.Timestamp = ts
	m.BotID = "B_ALERTS"
	m.SubType = "bot_message"
	m.Blocks = slackapi.Blocks{BlockSet: []slackapi.Block{
		slackapi.NewHeaderBlock(slackapi.NewTextBlockObject(slackapi.PlainTextType, "Incident", false, false)),
		slackapi.NewSectionBlock(slackapi.NewTextBlockObject(slackapi.MarkdownType, text, false, false), nil, nil),
	}}
	return m
}

func TestMessageText(t *testing.T) {
	cases := []struct {
		name     string
		msg      slackapi.Message
		contains []string
		exact    string
	}{
		{
			name:  "plain text message: returned verbatim",
			msg:   histMsg("U_ALICE", "should we use postgres?", "100.1", ""),
			exact: "should we use postgres?",
		},
		{
			name: "attachment-only alert: title and every field are flattened in",
			msg:  alertMsg("100.2"),
			contains: []string{
				"[FIRING:2]",
				"Stack name", "demo-stack-us-east-1",
				"Severity", ":warning: Warning",
				"Summary", "Deployment has not matched the expected number of replicas.",
				"Alerts", "KubePodCrashLooping", "CrashLoopBackOff",
			},
		},
		{
			name:     "blocks-only message: block text is flattened in",
			msg:      blockMsg("100.3", "the cache tier is down"),
			contains: []string{"Incident", "the cache tier is down"},
		},
		{
			name: "top-level text present alongside attachment: both are kept",
			msg: func() slackapi.Message {
				m := alertMsg("100.4")
				m.Text = "alert fired"
				return m
			}(),
			contains: []string{"alert fired", "[FIRING:2]", "KubePodCrashLooping"},
		},
		{
			name:  "no text, no blocks, no attachments: empty",
			msg:   histMsg("U_ALICE", "", "100.5", ""),
			exact: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := messageText(tc.msg)
			if tc.exact != "" || len(tc.contains) == 0 {
				assert.Equal(t, tc.exact, got)
				return
			}
			for _, want := range tc.contains {
				assert.Contains(t, got, want, "flattened text must carry %q", want)
			}
		})
	}
}

// TestMessageText_DoesNotDuplicateBlockFallbackText guards the transcript
// against double-counting. Slack auto-populates a message's top-level `text`
// with its own flattened rendering of `blocks` (newlines collapsed to spaces),
// so a message carrying both says the same thing twice. Emitting both would
// roughly double every bot reply in the transcript.
func TestMessageText_DoesNotDuplicateBlockFallbackText(t *testing.T) {
	m := slackapi.Message{}
	m.Timestamp = "100.1"
	m.BotID = "B_AGENT"
	// Slack's own fallback rendering: same words, whitespace collapsed.
	m.Text = "Found the crashing pod. *What's crashing* the cache tier"
	m.Blocks = slackapi.Blocks{BlockSet: []slackapi.Block{
		slackapi.NewSectionBlock(slackapi.NewTextBlockObject(
			slackapi.MarkdownType,
			"Found the crashing pod.\n\n*What's crashing*\nthe cache tier",
			false, false), nil, nil),
	}}

	got := messageText(m)

	assert.Equal(t, 1, strings.Count(got, "Found the crashing pod."),
		"content must appear exactly once, not once from text and again from blocks")
	assert.Equal(t, 1, strings.Count(got, "the cache tier"))
	assert.Contains(t, got, "Found the crashing pod.\n\n*What's crashing*",
		"the richer block rendering should win over the collapsed fallback")
}

func TestAuthorLabel(t *testing.T) {
	withUsername := alertMsg("100.1")
	withUsername.Username = "Alertmanager"

	withProfile := alertMsg("100.2")
	withProfile.BotProfile = &slackapi.BotProfile{Name: "Alertmanager"}

	cases := []struct {
		name string
		msg  slackapi.Message
		want string
	}{
		{name: "bot with username: username wins", msg: withUsername, want: "Alertmanager"},
		{name: "bot with only bot_profile: profile name is used", msg: withProfile, want: "Alertmanager"},
		{name: "bot with neither: falls back to the bot id, never empty", msg: alertMsg("100.3"), want: "B_ALERTS"},
		{name: "human message: no bot label", msg: histMsg("U_ALICE", "hi", "100.4", ""), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authorLabel(tc.msg))
		})
	}
}

// botMsg is an app message carrying nothing but a bot id — the shape a
// webhook-posted alert actually has (no username, no bot_profile).
func botMsg(botID, text, ts string) slackapi.Message {
	m := slackapi.Message{}
	m.BotID = botID
	m.SubType = "bot_message"
	m.Text = text
	m.Timestamp = ts
	return m
}

// TestReadHistory_ResolvesBotNamesViaBotsInfo covers the last mile of app
// attribution. When Slack attaches neither username nor bot_profile — which is
// exactly what an alerting webhook sends — the only readable name available is
// behind a bots.info lookup. It mirrors the users.info cache: one call per
// DISTINCT bot per page, and a failure degrades to the bot id rather than
// failing the page.
func TestReadHistory_ResolvesBotNamesViaBotsInfo(t *testing.T) {
	cases := []struct {
		name      string
		msgs      []slackapi.Message
		bots      map[string]*slackapi.Bot
		botErr    error
		wantNames []string
		wantReqs  []string
	}{
		{
			name: "id-only bot: resolved to its real name",
			msgs: []slackapi.Message{botMsg("B_ALERTS", "disk full", "100.1")},
			bots: map[string]*slackapi.Bot{"B_ALERTS": {ID: "B_ALERTS", Name: "Alertmanager"}},
			// Reads as "Alertmanager: disk full" in the transcript.
			wantNames: []string{"Alertmanager"},
			wantReqs:  []string{"B_ALERTS"},
		},
		{
			name: "same bot twice: looked up once, not per message",
			msgs: []slackapi.Message{botMsg("B_ALERTS", "disk full", "100.1"), botMsg("B_ALERTS", "disk ok", "100.2")},
			bots: map[string]*slackapi.Bot{"B_ALERTS": {ID: "B_ALERTS", Name: "Alertmanager"}},
			// Two distinct bots would be two calls; the same bot must be one.
			wantNames: []string{"Alertmanager", "Alertmanager"},
			wantReqs:  []string{"B_ALERTS"},
		},
		{
			name: "username already present: no lookup spent",
			msgs: func() []slackapi.Message {
				m := botMsg("B_ALERTS", "disk full", "100.1")
				m.Username = "Alertmanager"
				return []slackapi.Message{m}
			}(),
			wantNames: []string{"Alertmanager"},
			wantReqs:  nil,
		},
		{
			name:      "bots.info fails: degrades to the bot id, page still returned",
			msgs:      []slackapi.Message{botMsg("B_ALERTS", "disk full", "100.1")},
			botErr:    assert.AnError,
			wantNames: []string{"B_ALERTS"},
			wantReqs:  []string{"B_ALERTS"},
		},
		{
			name:      "bots.info returns nothing: still the bot id, never empty",
			msgs:      []slackapi.Message{botMsg("B_ALERTS", "disk full", "100.1")},
			bots:      map[string]*slackapi.Bot{},
			wantNames: []string{"B_ALERTS"},
			wantReqs:  []string{"B_ALERTS"},
		},
		{
			name:      "human message: never triggers a bot lookup",
			msgs:      []slackapi.Message{histMsg("U_ALICE", "hi", "100.1", "")},
			wantNames: []string{""}, // resolved via users.info, which the fake has no entry for
			wantReqs:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeHistoryClient{replies: tc.msgs, bots: tc.bots, botErr: tc.botErr}
			prev := historyClientFactory
			historyClientFactory = func(_ channelkinds.Deps) historyClient { return fake }
			t.Cleanup(func() { historyClientFactory = prev })

			page, err := (&Kind{}).ReadHistory(context.Background(), channelkinds.Deps{},
				"thread:C123:100.0", channelkinds.ReadHistoryOpts{Limit: 50})
			require.NoError(t, err, "a bot-name lookup must never fail the page")
			require.Len(t, page.Messages, len(tc.wantNames))

			for i, want := range tc.wantNames {
				assert.Equal(t, want, page.Messages[i].AuthorDisplayName, "message %d display name", i)
			}
			assert.Equal(t, tc.wantReqs, fake.botReqs, "bots.info calls (order and count)")
		})
	}
}

// TestReadHistory_MarksOnlyOurOwnBotAsFromSelf is the guard on the catch-up
// filter's input. catchUp skips FromSelf (not FromApp) so third-party alerts
// survive; that is only safe if the kind actually distinguishes this agent's
// own bot identity from every other app in the channel. Both halves of the
// distinction are asserted here: get it wrong one way and the agent re-reads
// its own replies every turn, the other way and alerts vanish again.
func TestReadHistory_MarksOnlyOurOwnBotAsFromSelf(t *testing.T) {
	ourReply := slackapi.Message{}
	ourReply.User = "U_OURBOT" // Slack sets `user` on our own bot's posts
	ourReply.BotID = "B_OURAPP"
	ourReply.Text = "on it, checking the pods"
	ourReply.Timestamp = "100.2"

	fake := &fakeHistoryClient{replies: []slackapi.Message{
		alertMsg("100.1"), // third-party webhook: no user id at all
		ourReply,
		histMsg("U_ALICE", "any update?", "100.3", ""),
	}}
	prev := historyClientFactory
	historyClientFactory = func(_ channelkinds.Deps) historyClient { return fake }
	t.Cleanup(func() { historyClientFactory = prev })

	deps := channelkinds.Deps{Channel: &spiceboxv1alpha1.Channel{
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:  "slack",
			Slack: &spiceboxv1alpha1.SlackChannelConfig{BotUserID: "U_OURBOT"},
		},
	}}
	page, err := (&Kind{}).ReadHistory(context.Background(), deps,
		"thread:C123:100.0", channelkinds.ReadHistoryOpts{Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Messages, 3)

	assert.True(t, page.Messages[0].FromApp, "third-party alert is an app message")
	assert.False(t, page.Messages[0].FromSelf,
		"a third-party alert bot is NOT us — marking it FromSelf would drop alerts on catch-up")

	assert.True(t, page.Messages[1].FromApp)
	assert.True(t, page.Messages[1].FromSelf,
		"our own bot's reply must be FromSelf so catch-up does not re-seed it")

	assert.False(t, page.Messages[2].FromApp, "human")
	assert.False(t, page.Messages[2].FromSelf, "a human is never FromSelf")
}

// TestReadHistory_AttachmentAlertReachesTheAgent is the end-to-end guard for
// the reported bug: an alert posted by a webhook before the agent was invited
// must arrive with its contents and an attributable author, not "unknown: ".
func TestReadHistory_AttachmentAlertReachesTheAgent(t *testing.T) {
	fake := &fakeHistoryClient{replies: []slackapi.Message{alertMsg("100.1")}}
	prev := historyClientFactory
	historyClientFactory = func(_ channelkinds.Deps) historyClient { return fake }
	t.Cleanup(func() { historyClientFactory = prev })

	page, err := (&Kind{}).ReadHistory(context.Background(), channelkinds.Deps{},
		"thread:C123:100.0", channelkinds.ReadHistoryOpts{Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)

	got := page.Messages[0]
	assert.True(t, got.FromApp, "webhook alert is still an app message")
	assert.NotEmpty(t, got.Text, "alert contents must not be dropped")
	assert.Contains(t, got.Text, "KubePodCrashLooping")
	assert.Contains(t, got.Text, "demo-stack-us-east-1")
	assert.NotEmpty(t, got.AuthorDisplayName, "app messages must carry an attributable label, not render as \"unknown\"")
	assert.Empty(t, got.AuthorEmail, "app messages are still not user-resolved")
	assert.Empty(t, got.AuthorExternalID, "app messages must not gain a user id (participant grants key off it)")
}
