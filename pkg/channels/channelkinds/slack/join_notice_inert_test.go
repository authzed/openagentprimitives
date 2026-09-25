// pkg/channels/channelkinds/slack/join_notice_inert_test.go
//
// The join notice is the one message this kind composes out of strings that
// OTHER Slack users chose for themselves. Everything else it interpolates
// arrives from a publisher over the wire; a display name arrives from
// users.info, which means from whoever owns that profile.
package slack

import (
	"context"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// hostileDisplayNames are profile names a workspace member — including a
// single-channel guest, who needs no standing with the agent at all — can set
// on themselves, then post once in a thread. When the bot is later summoned
// into that thread it names the thread's participants, and each of these
// renders as the bot's OWN markup.
var hostileDisplayNames = []struct {
	name  string
	rname string
	// absent is the live markup that must not survive into the posted text.
	absent string
}{
	{
		name:   "a channel-wide ping in a display name: the BOT pings the channel",
		rname:  "<!channel>",
		absent: "<!channel>",
	},
	{
		name:   "a credential lure in a display name: the BOT posts a genuine-looking link",
		rname:  "<https://attacker.example.invalid/oauth|Connect your Google account>",
		absent: "<https://attacker.example.invalid/oauth|Connect your Google account>",
	},
	{
		name:   "a forged mention in a display name: the BOT pings a bystander",
		rname:  "<@U_VICTIM>",
		absent: "<@U_VICTIM>",
	},
}

// TestFormatJoinNotice_ParticipantNamesAreInert pins the escape at the unit
// that composes the notice. The notice is posted with
// MsgOptionText(_, false) (postJoinNotice), so slack-go does not escape it for
// us and every character in a display name is parsed as mrkdwn.
//
// The thread it lands in is the same thread where this kind posts real
// credential-linking prompts, which is what makes a lure link here worth
// more to an attacker than one posted under their own name.
func TestFormatJoinNotice_ParticipantNamesAreInert(t *testing.T) {
	for _, tc := range hostileDisplayNames {
		t.Run(tc.name, func(t *testing.T) {
			got := formatJoinNotice([]string{"Alice", tc.rname}, nil)

			assert.NotContains(t, got, tc.absent, "a participant's own display name must not render as markup")
			assert.Contains(t, got, "Alice", "an ordinary name is untouched")
			assert.Contains(t, got, "@-mention", "the notice still says what it says")
		})
	}
}

// TestFormatJoinNotice_InjectedMarkupStaysVisible: inert is not deleted. The
// notice exists to tell the thread who can talk to the bot, so a name that
// tried something must still be readable — a reader who sees
// "&lt;!channel&gt;" learns both who is in the thread and that they are up to
// something.
func TestFormatJoinNotice_InjectedMarkupStaysVisible(t *testing.T) {
	got := formatJoinNotice([]string{"<!channel>"}, nil)
	assert.Contains(t, got, "&lt;!channel&gt;", "the attempt renders as literal characters")
}

// TestJoinNotice_HostileDisplayNameFromUsersInfoIsInertWhenPosted drives the
// defect down the real path rather than asserting on a helper: the name is
// set on a users.info profile, read by THIS package's history reader
// (ReadHistory → AuthorDisplayName), and posted by THIS package's
// postJoinNotice. The one hop in between — channelsd's adoption grant, which
// copies AuthorDisplayName into InboundDecision.GrantedParticipants — is a
// straight copy in another package, so the two ends are what matter and this
// test owns both.
//
// The assertion is on a REAL *slackapi.Client's wire form, not on
// UnsafeApplyMsgOptions, so nothing about how the option is encoded is
// assumed.
func TestJoinNotice_HostileDisplayNameFromUsersInfoIsInertWhenPosted(t *testing.T) {
	for _, tc := range hostileDisplayNames {
		t.Run(tc.name, func(t *testing.T) {
			// 1. The attacker's profile, as users.info reports it.
			fake := &fakeHistoryClient{
				replies: []slackapi.Message{histMsg("U_MALLORY", "hello", "100.1", "")},
				users:   map[string]*slackapi.User{"U_MALLORY": histUser("U_MALLORY", tc.rname, "m@example.invalid")},
			}
			prev := historyClientFactory
			historyClientFactory = func(_ channelkinds.Deps) historyClient { return fake }
			t.Cleanup(func() { historyClientFactory = prev })

			page, err := (&Kind{}).ReadHistory(context.Background(), channelkinds.Deps{},
				"thread:C123:100.0", channelkinds.ReadHistoryOpts{Limit: 50})
			require.NoError(t, err, "ReadHistory")
			require.Len(t, page.Messages, 1)
			participant := page.Messages[0].AuthorDisplayName
			require.Equal(t, tc.rname, participant,
				"precondition: the display name reaches the adoption grant exactly as its owner set it")

			// 2. The notice the bot posts when it is summoned into that thread.
			api, posts := recordingPostMessageAPI(t)
			l := &slackListener{api: api}
			l.postJoinNotice(context.Background(), "C123", "100.0", []string{participant}, nil)

			all := posts()
			require.Len(t, all, 1, "the join notice is posted")
			assert.NotContains(t, all[0].text, tc.absent,
				"the bot must not post a thread participant's display name as live markup")
			assert.True(t, strings.Contains(all[0].text, "&lt;"),
				"...and the attempt must still be visible to a reader, escaped")
		})
	}
}

// The WITHHELD slot took the same names and did not escape them.
//
// It is the worse of the two positions, not a lesser one: a withheld author is
// by construction someone OUTSIDE the class's trust policy — the very people
// the notice exists to name — and their display name was posted as live
// mrkdwn under the bot's identity, in the thread where this kind also posts
// real credential-linking prompts. Every case above passed only because each
// one left `withheld` nil.
func TestFormatJoinNotice_WithheldNamesAreInert(t *testing.T) {
	for _, tc := range hostileDisplayNames {
		t.Run(tc.name, func(t *testing.T) {
			got := formatJoinNotice([]string{"Alice"}, []string{tc.rname})

			assert.NotContains(t, got, tc.absent,
				"a withheld author's display name must not render as markup — they are the least trusted name in the notice")
			assert.Contains(t, got, "not on this agent's access list",
				"the notice still explains why they will get no reply")
		})
	}
}

// Inert is not deleted in the withheld slot either: naming who was refused is
// the point of tracking them.
func TestFormatJoinNotice_WithheldMarkupStaysVisible(t *testing.T) {
	got := formatJoinNotice(nil, []string{"<!channel>"})

	assert.Contains(t, got, "channel", "the reader must still see who was withheld")
	assert.NotContains(t, got, "<!channel>", "but not as live markup")
}
