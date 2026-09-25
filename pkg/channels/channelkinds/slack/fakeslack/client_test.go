package fakeslack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFake_TopLevelVsThreadedReply(t *testing.T) {
	c := New()
	ctx := context.Background()

	// User posts a top-level message (root).
	_, rootTS, err := c.PostMessageContext(ctx, "D01", slackapi.MsgOptionText("hi", false))
	require.NoError(t, err)

	// Agent replies threaded under the root.
	_, replyTS, err := c.PostMessageContext(ctx, "D01",
		slackapi.MsgOptionText("hello", false), slackapi.MsgOptionTS(rootTS))
	require.NoError(t, err)

	assert.Len(t, c.TopLevel("D01"), 1, "only the user root is top-level")
	replies := c.Replies("D01", rootTS)
	require.Len(t, replies, 1, "agent reply must be a threaded child of the root")
	assert.Equal(t, replyTS, replies[0].TS)
	assert.Equal(t, rootTS, replies[0].ThreadTS, "reply carries the root as thread_ts")

	// conversations.replies returns root + replies.
	msgs, _, _, err := c.GetConversationRepliesContext(ctx,
		&slackapi.GetConversationRepliesParameters{ChannelID: "D01", Timestamp: rootTS})
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, rootTS, msgs[0].Timestamp)
	assert.Equal(t, replyTS, msgs[1].Timestamp)
}

func TestFake_UpdateAndStatus(t *testing.T) {
	c := New()
	ctx := context.Background()
	_, ts, _ := c.PostMessageContext(ctx, "D01", slackapi.MsgOptionText("draft", false), slackapi.MsgOptionTS("1700000000.000000"))
	_, _, _, err := c.UpdateMessageContext(ctx, "D01", ts, slackapi.MsgOptionText("final", false))
	require.NoError(t, err)
	last, ok := c.Last("D01")
	require.True(t, ok)
	assert.Equal(t, "final", last.Text, "chat.update mutates the message text in place")

	require.NoError(t, c.SetAssistantThreadsStatusContext(ctx,
		slackapi.AssistantThreadsSetStatusParameters{ChannelID: "D01", ThreadTS: "1700000000.000000", Status: "thinking…"}))
	got, ok := c.StatusFor("D01", "1700000000.000000")
	require.True(t, ok)
	assert.Equal(t, "thinking…", got)
}

func TestFake_SetAssistantThreadsTitle_Records(t *testing.T) {
	c := New()
	require.NoError(t, c.SetAssistantThreadsTitleContext(context.Background(),
		slackapi.AssistantThreadsSetTitleParameters{ChannelID: "D01", ThreadTS: "1.1", Title: "hello"}))
	got, ok := c.TitleFor("D01", "1.1")
	require.True(t, ok)
	assert.Equal(t, "hello", got)
}
