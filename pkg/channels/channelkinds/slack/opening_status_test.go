package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestSlackSender_EditOpeningMessage_UpdatesTheRootMessage(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)

	err := s.EditOpeningMessage(context.Background(),
		channelkinds.SessionInfo{Namespace: "default", Name: "gh-sess"},
		channelkinds.OpeningMessageContent{
			Ref:         channelkinds.MessageRef{ChannelID: "C_OUT", TS: "111.222"},
			OpeningText: "Picked up PR demo-org/demo-repo#4",
			Badge:       spiceboxv1alpha1.OpeningBadgeProblemsFound,
			Body:        "2 findings",
			Link:        "https://webd.example/view",
		})
	require.NoError(t, err)

	require.Len(t, c.updateCalls, 1, "editing goes through chat.update, not a fresh post")
	require.Empty(t, c.postMessageCalls)
	assert.Equal(t, "C_OUT", c.updateCalls[0].channelID)
	assert.Equal(t, "111.222", c.updateCalls[0].ts)
	// The rendered fallback text carries the badge label + opening line.
	_, vals, err := slackapi.UnsafeApplyMsgOptions("t", "C_OUT", "http://x/", c.updateCalls[0].opts...)
	require.NoError(t, err)
	assert.Contains(t, vals.Get("text"), "Problems found")
	assert.Contains(t, vals.Get("text"), "Picked up PR demo-org/demo-repo#4")
}

func TestSlackSender_ImplementsOpeningMessageEditor(t *testing.T) {
	var _ channelkinds.OpeningMessageEditor = (*slackSender)(nil)
}
