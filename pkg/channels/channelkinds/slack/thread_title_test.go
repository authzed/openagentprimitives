package slack

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"

	slackapi "github.com/slack-go/slack"
)

// titleEnv builds a KindThreadTitle envelope carrying (title, emoji) at a
// given Seq. SessionUID is left empty in every test here, so the ordering
// guard's uid comparison always matches across calls in the same test.
func titleEnv(t *testing.T, title, emoji string, seq uint64) channelevents.Envelope {
	t.Helper()
	pl, err := json.Marshal(channelevents.ThreadTitlePayload{Title: title, Emoji: emoji})
	require.NoError(t, err)
	return channelevents.Envelope{Kind: channelevents.KindThreadTitle, Payload: pl, Seq: seq}
}

// titleTestSender builds a threadTitleSender wired to a fake client and the
// given starter cache, plus a SessionInfo whose channel_id the sender reads
// from Channel.External. deps.K8sClient is left nil (zero Deps), and the
// test SessionInfo carries no Annotations and no External["thread_ts"], so
// effectiveOutboundThreadTS resolves to "" here — the sender must therefore
// use "" as the effective thread_ts in every assertion below (production
// sets the LastInboundTS annotation via the outbound relay, so the resolved
// value is non-empty there).
func titleTestSender(t *testing.T, channelID string, cache *starterCache) (*threadTitleSender, *fakeslack.Client, channelkinds.SessionInfo) {
	t.Helper()
	fc := fakeslack.New()
	s := &threadTitleSender{
		client:   fc,
		starters: cache,
		last:     map[string]titleState{},
	}
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "s1",
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": channelID}},
	}
	return s, fc, sess
}

func TestThreadTitle_ChannelPath_EditsStarter_DefaultGlyph(t *testing.T) {
	cache := newStarterCache()
	s, fc, sess := titleTestSender(t, "C123", cache)
	_, ts, err := fc.PostMessageContext(context.Background(), "C123",
		slackapi.MsgOptionText(startingMessageAt("hubspot-companies", 1720000000), false))
	require.NoError(t, err)
	cache.put("default/s1", starterCoords{ChannelID: "C123", MessageTS: ts, AgentName: "hubspot-companies", StartedUnix: 1720000000})

	_, err = s.Send(context.Background(), sess, titleEnv(t, "Refactoring the auth layer", "", 1))
	require.NoError(t, err)

	got, ok := fc.Last("C123")
	require.True(t, ok)
	assert.Equal(t, ts, got.TS, "the starter message must be edited in place, not a new message posted")
	assert.Contains(t, got.Text, "🤖 *Refactoring the auth layer*")
	assert.Contains(t, got.Text, "· hubspot-companies · started <!date^1720000000^")
}

func TestThreadTitle_ChannelPath_EmojiReplacesGlyph(t *testing.T) {
	cache := newStarterCache()
	s, fc, sess := titleTestSender(t, "C123", cache)
	_, ts, err := fc.PostMessageContext(context.Background(), "C123",
		slackapi.MsgOptionText(startingMessageAt("agent", 1720000000), false))
	require.NoError(t, err)
	cache.put("default/s1", starterCoords{ChannelID: "C123", MessageTS: ts, AgentName: "agent", StartedUnix: 1720000000})

	_, err = s.Send(context.Background(), sess, titleEnv(t, "Deploy pipeline", "🚀", 1))
	require.NoError(t, err)

	got, ok := fc.Last("C123")
	require.True(t, ok)
	assert.Contains(t, got.Text, "🚀 *Deploy pipeline*")
	assert.NotContains(t, got.Text, "🤖 *Deploy pipeline*")
}

func TestThreadTitle_DMPath_NativeSetTitle_EmojiFolded(t *testing.T) {
	cache := newStarterCache() // empty: no starter → DM path (channel "D…")
	s, fc, sess := titleTestSender(t, "D999", cache)

	_, err := s.Send(context.Background(), sess, titleEnv(t, "Weekly report", "📊", 1))
	require.NoError(t, err)

	// effectiveOutboundThreadTS resolves to "" in this fixture (see
	// titleTestSender doc comment); the fake keys titles by "channel|thread_ts".
	got, ok := fc.TitleFor("D999", "")
	require.True(t, ok, "SetAssistantThreadsTitleContext must have been called")
	assert.Equal(t, "📊 Weekly report", got)

	_, hasMsg := fc.Last("D999")
	assert.False(t, hasMsg, "no chat.update/postMessage on the DM path")
}

func TestThreadTitle_NoStarter_NonDM_NoOp(t *testing.T) {
	cache := newStarterCache()                       // empty
	s, fc, sess := titleTestSender(t, "C555", cache) // channel, not "D…"

	_, err := s.Send(context.Background(), sess, titleEnv(t, "orphan", "", 1))
	require.NoError(t, err)

	_, ok := fc.TitleFor("C555", "")
	assert.False(t, ok, "no setTitle in a non-DM without a cached starter")
	_, hasMsg := fc.Last("C555")
	assert.False(t, hasMsg, "no message posted/edited either")
}

func TestThreadTitle_StaleSeq_Dropped(t *testing.T) {
	cache := newStarterCache()
	s, fc, sess := titleTestSender(t, "D999", cache)

	_, err := s.Send(context.Background(), sess, titleEnv(t, "first", "", 5))
	require.NoError(t, err)
	_, err = s.Send(context.Background(), sess, titleEnv(t, "stale", "", 3)) // older seq
	require.NoError(t, err)

	got, ok := fc.TitleFor("D999", "")
	require.True(t, ok)
	assert.Equal(t, "first", got, "older-seq title must be dropped, last-applied title must stand")
}

func TestThreadTitle_UnchangedPair_Skipped(t *testing.T) {
	cache := newStarterCache()
	s, fc, sess := titleTestSender(t, "D999", cache)

	_, err := s.Send(context.Background(), sess, titleEnv(t, "same", "🔧", 1))
	require.NoError(t, err)
	callsBefore := fc.SetTitleCallCount()
	require.Equal(t, 1, callsBefore)

	_, err = s.Send(context.Background(), sess, titleEnv(t, "same", "🔧", 2))
	require.NoError(t, err)
	assert.Equal(t, callsBefore, fc.SetTitleCallCount(), "identical (title,emoji) must skip the API call")
}

// TestThreadTitle_ChannelPath_AgentTitleIsInert is the guard for the thread
// starter's mrkdwn sink. Title and Emoji come from the set_thread_title META
// TOOL, so the model chooses both strings, and the channel path interpolates
// them into a chat.update posted with MsgOptionText(_, false) — mrkdwn, which
// slack-go does not escape.
//
// The starter is the message pinned at the top of the thread and styled as the
// platform's own header, which makes a forged `<url|label>` there worth more
// to an attacker than one in the agent's ordinary reply text (which is
// deliberately live, because an agent's message IS its message — see
// slackifyText).
func TestThreadTitle_ChannelPath_AgentTitleIsInert(t *testing.T) {
	cases := []struct {
		name  string
		title string
		emoji string
	}{
		{name: "a forged link in the title cannot render as a hyperlink", title: lureLink},
		{name: "a channel-wide ping in the title cannot ping the channel", title: "<!channel> deploy"},
		{name: "the emoji slot is model-chosen too and is no different", title: "Deploy", emoji: lureLink},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := newStarterCache()
			s, fc, sess := titleTestSender(t, "C123", cache)
			_, ts, err := fc.PostMessageContext(context.Background(), "C123",
				slackapi.MsgOptionText(startingMessageAt("demo-agent", 1720000000), false))
			require.NoError(t, err)
			cache.put("default/s1", starterCoords{ChannelID: "C123", MessageTS: ts, AgentName: "demo-agent", StartedUnix: 1720000000})

			_, err = s.Send(context.Background(), sess, titleEnv(t, tc.title, tc.emoji, 1))
			require.NoError(t, err)

			got, ok := fc.Last("C123")
			require.True(t, ok)
			assert.NotContains(t, got.Text, lureLink, "a model-chosen title must not render live markup")
			assert.NotContains(t, got.Text, "<!channel>")
			assert.Contains(t, got.Text, "&lt;", "inert is not deleted — the title stays readable")
		})
	}
}

// TestThreadTitle_ChannelPath_DateTokenStaysLive is the order guard. The
// starter line ends with `<!date^…|just now>` — a Slack token THIS kind
// composes so the timestamp renders in each reader's own locale. Escaping the
// finished line instead of the model's two slots would print the raw token to
// every reader.
func TestThreadTitle_ChannelPath_DateTokenStaysLive(t *testing.T) {
	cache := newStarterCache()
	s, fc, sess := titleTestSender(t, "C123", cache)
	_, ts, err := fc.PostMessageContext(context.Background(), "C123",
		slackapi.MsgOptionText(startingMessageAt("demo-agent", 1720000000), false))
	require.NoError(t, err)
	cache.put("default/s1", starterCoords{ChannelID: "C123", MessageTS: ts, AgentName: "demo-agent", StartedUnix: 1720000000})

	_, err = s.Send(context.Background(), sess, titleEnv(t, "Deploy "+lureLink, "", 1))
	require.NoError(t, err)

	got, ok := fc.Last("C123")
	require.True(t, ok)
	assert.Contains(t, got.Text, "started <!date^1720000000^",
		"the kind's own date token must stay live — escape the model's slots, not the composed line")
}
