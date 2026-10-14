package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

type fakeHistClient struct {
	gotParams *slackapi.GetConversationHistoryParameters
	resp      *slackapi.GetConversationHistoryResponse
}

func (f *fakeHistClient) GetConversationRepliesContext(context.Context, *slackapi.GetConversationRepliesParameters) ([]slackapi.Message, bool, string, error) {
	return nil, false, "", nil
}
func (f *fakeHistClient) GetUserInfoContext(_ context.Context, id string, _ ...slackapi.GetUserInfoOption) (*slackapi.User, error) {
	return &slackapi.User{Name: "u" + id, RealName: "User " + id, Profile: slackapi.UserProfile{Email: id + "@x.com"}}, nil
}
func (f *fakeHistClient) GetConversationHistoryContext(_ context.Context, p *slackapi.GetConversationHistoryParameters) (*slackapi.GetConversationHistoryResponse, error) {
	f.gotParams = p
	return f.resp, nil
}
func (f *fakeHistClient) GetBotInfoContext(_ context.Context, p slackapi.GetBotInfoParameters) (*slackapi.Bot, error) {
	return &slackapi.Bot{ID: p.Bot, Name: "Bot " + p.Bot}, nil
}

// fakeSecretWithBotToken builds a minimal Secret carrying a bot token, the
// shape historyClientFactory needs from Deps.Secret.
func fakeSecretWithBotToken() *corev1.Secret {
	return &corev1.Secret{Data: map[string][]byte{SecretKeyBotToken: []byte("xoxb-test")}}
}

func TestReadChannelHistory(t *testing.T) {
	fake := &fakeHistClient{resp: &slackapi.GetConversationHistoryResponse{
		HasMore:  true,
		Messages: []slackapi.Message{{Msg: slackapi.Msg{User: "U1", Text: "hi", Timestamp: "100.000"}}},
	}}
	fake.resp.ResponseMetaData.NextCursor = "CUR2"

	orig := historyClientFactory
	historyClientFactory = func(channelkinds.Deps) historyClient { return fake }
	t.Cleanup(func() { historyClientFactory = orig })

	k := &Kind{}
	binding := &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C123"}}
	page, err := k.ReadChannelHistory(context.Background(),
		channelkinds.Deps{Secret: fakeSecretWithBotToken()}, binding,
		channelkinds.ChannelHistoryOpts{Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, "C123", fake.gotParams.ChannelID)
	assert.Equal(t, 50, fake.gotParams.Limit)
	require.Len(t, page.Messages, 1)
	assert.Equal(t, "User U1", page.Messages[0].AuthorDisplayName)
	assert.True(t, page.HasMore)
	assert.Equal(t, "CUR2", page.NextCursor)

	ref, ok := k.ChannelViewSubjectRef(binding)
	assert.True(t, ok)
	assert.Equal(t, "slack_channel:C123#view", ref)
}

// TestReadChannelHistory_ReversesNewestFirstToOldestFirst guards the one piece
// of logic in ReadChannelHistory most prone to an off-by-one: conversations.history
// returns messages newest-first, but HistoryPage is contractually oldest-first
// (OldestCursor is taken from Messages[0].TS). A single-message page can't catch
// a broken reversal, so this exercises three.
func TestReadChannelHistory_ReversesNewestFirstToOldestFirst(t *testing.T) {
	fake := &fakeHistClient{resp: &slackapi.GetConversationHistoryResponse{
		// Slack order: newest first.
		Messages: []slackapi.Message{
			{Msg: slackapi.Msg{User: "U3", Text: "newest", Timestamp: "300.000"}},
			{Msg: slackapi.Msg{User: "U2", Text: "middle", Timestamp: "200.000"}},
			{Msg: slackapi.Msg{User: "U1", Text: "oldest", Timestamp: "100.000"}},
		},
	}}
	orig := historyClientFactory
	historyClientFactory = func(channelkinds.Deps) historyClient { return fake }
	t.Cleanup(func() { historyClientFactory = orig })

	k := &Kind{}
	binding := &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C1"}}
	page, err := k.ReadChannelHistory(context.Background(),
		channelkinds.Deps{Secret: fakeSecretWithBotToken()}, binding,
		channelkinds.ChannelHistoryOpts{Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Messages, 3)
	assert.Equal(t,
		[]string{"oldest", "middle", "newest"},
		[]string{page.Messages[0].Text, page.Messages[1].Text, page.Messages[2].Text},
		"HistoryPage must be oldest-first even though conversations.history is newest-first")
	assert.Equal(t, "100.000", page.Messages[0].TS, "oldest message (lowest ts) must sort first")
}
