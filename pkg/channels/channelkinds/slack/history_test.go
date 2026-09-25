package slack

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// fakeHistoryClient is a hand-rolled double for historyClient.
type fakeHistoryClient struct {
	replies []slackapi.Message
	hasMore bool
	cursor  string
	users   map[string]*slackapi.User
	bots    map[string]*slackapi.Bot
	repErr  error
	userErr error
	botErr  error
	lastReq *slackapi.GetConversationRepliesParameters
	botReqs []string // every bots.info lookup, in order — asserts the cache
}

// AuthTestContext makes this fake an authTester, which is how the kind learns
// which workspace its token belongs to. Without it resolveInstalledTeamID
// returns "" and every author's email is dropped as untrusted — correct
// behaviour, but the fixture would then describe a bot that never resolved its
// own workspace rather than the ordinary case.
func (f *fakeHistoryClient) AuthTestContext(_ context.Context) (*slackapi.AuthTestResponse, error) {
	return &slackapi.AuthTestResponse{TeamID: histTestTeam}, nil
}

func (f *fakeHistoryClient) GetConversationRepliesContext(_ context.Context, p *slackapi.GetConversationRepliesParameters) ([]slackapi.Message, bool, string, error) {
	f.lastReq = p
	return f.replies, f.hasMore, f.cursor, f.repErr
}

func (f *fakeHistoryClient) GetUserInfoContext(_ context.Context, user string) (*slackapi.User, error) {
	if f.userErr != nil {
		return nil, f.userErr
	}
	return f.users[user], nil
}

func (f *fakeHistoryClient) GetConversationHistoryContext(_ context.Context, _ *slackapi.GetConversationHistoryParameters) (*slackapi.GetConversationHistoryResponse, error) {
	return nil, nil
}

func (f *fakeHistoryClient) GetBotInfoContext(_ context.Context, p slackapi.GetBotInfoParameters) (*slackapi.Bot, error) {
	f.botReqs = append(f.botReqs, p.Bot)
	if f.botErr != nil {
		return nil, f.botErr
	}
	return f.bots[p.Bot], nil
}

func histMsg(user, text, ts, botID string) slackapi.Message {
	m := slackapi.Message{}
	m.User = user
	m.Text = text
	m.Timestamp = ts
	m.BotID = botID
	return m
}

// histTestTeam is the workspace the fake bot token belongs to. A fixture user
// must carry it: a profile email is an authorization identity only for a full
// member of the INSTALLED workspace (emailTrusted), so a user with no team is
// a foreign account and correctly yields no email.
const histTestTeam = "T_HIST"

func histUser(id, real, email string) *slackapi.User {
	u := &slackapi.User{ID: id, RealName: real, TeamID: histTestTeam}
	u.Profile.Email = email
	return u
}

func TestReadHistory_MapsMessagesAndResolvesAuthors(t *testing.T) {
	fake := &fakeHistoryClient{
		replies: []slackapi.Message{
			histMsg("U_ALICE", "should we use postgres?", "100.1", ""),
			histMsg("U_BOB", "yes", "100.2", ""),
			histMsg("", "beep boop", "100.3", "B_BOT"), // app message
		},
		hasMore: false,
		users: map[string]*slackapi.User{
			"U_ALICE": histUser("U_ALICE", "Alice Anderson", "alice@example.com"),
			"U_BOB":   histUser("U_BOB", "Bob Brown", "bob@example.com"),
		},
	}
	prev := historyClientFactory
	historyClientFactory = func(_ channelkinds.Deps) historyClient { return fake }
	t.Cleanup(func() { historyClientFactory = prev })

	page, err := (&Kind{}).ReadHistory(context.Background(), channelkinds.Deps{},
		"thread:C123:100.0", channelkinds.ReadHistoryOpts{Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Messages, 3)

	assert.Equal(t, "U_ALICE", page.Messages[0].AuthorExternalID)
	assert.Equal(t, "Alice Anderson", page.Messages[0].AuthorDisplayName)
	assert.Equal(t, "alice@example.com", page.Messages[0].AuthorEmail)
	assert.Equal(t, "should we use postgres?", page.Messages[0].Text)
	assert.Equal(t, "100.1", page.Messages[0].TS)
	assert.False(t, page.Messages[0].FromApp)

	assert.True(t, page.Messages[2].FromApp, "bot message must be flagged FromApp")
	assert.Empty(t, page.Messages[2].AuthorEmail, "app messages are not user-resolved")

	assert.Equal(t, "C123", fake.lastReq.ChannelID)
	assert.Equal(t, "100.0", fake.lastReq.Timestamp)
}

func TestReadHistory_HonorsCursorBoundsAndHasMore(t *testing.T) {
	fake := &fakeHistoryClient{
		replies: []slackapi.Message{histMsg("U_ALICE", "x", "100.5", "")},
		hasMore: true,
		users:   map[string]*slackapi.User{"U_ALICE": histUser("U_ALICE", "Alice", "a@example.com")},
	}
	prev := historyClientFactory
	historyClientFactory = func(_ channelkinds.Deps) historyClient { return fake }
	t.Cleanup(func() { historyClientFactory = prev })

	page, err := (&Kind{}).ReadHistory(context.Background(), channelkinds.Deps{},
		"thread:C123:100.0", channelkinds.ReadHistoryOpts{AfterTS: "100.4", BeforeTS: "100.9", Limit: 10})
	require.NoError(t, err)
	assert.True(t, page.HasMore)
	assert.Equal(t, "100.4", fake.lastReq.Oldest)
	assert.Equal(t, "100.9", fake.lastReq.Latest)
	assert.Equal(t, 10, fake.lastReq.Limit)
}

// TestReadHistory_LogsUserInfoFailure is the no-silent-errors guard for author
// resolution. A users.info failure degrades that message to id-only attribution
// and deliberately never fails the page — but swallowing it leaves an operator
// no way to tell a revoked users:read scope from a workspace where nobody has
// set a display name. Both render as bare user ids in the transcript, and only
// one of them is a problem to fix.
func TestReadHistory_LogsUserInfoFailure(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	fake := &fakeHistoryClient{
		replies: []slackapi.Message{histMsg("U_ALICE", "hi", "100.1", "")},
		userErr: assert.AnError,
	}
	prev := historyClientFactory
	historyClientFactory = func(_ channelkinds.Deps) historyClient { return fake }
	t.Cleanup(func() { historyClientFactory = prev })

	page, err := (&Kind{}).ReadHistory(
		logf.IntoContext(context.Background(), capLogger),
		channelkinds.Deps{}, "thread:C123:100.0", channelkinds.ReadHistoryOpts{Limit: 50})
	require.NoError(t, err, "a users.info failure must never fail the page")
	require.Len(t, page.Messages, 1)
	assert.Empty(t, page.Messages[0].AuthorDisplayName, "degrades to id-only attribution")

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "U_ALICE", "the log must name the user that failed to resolve")
	assert.Contains(t, strings.ToLower(joined), "users.info", "and must name what failed")
}

// TestReadHistory_LogsBotInfoFailure is the same no-silent-errors guard for the
// bot half. bots.info needs users:read, exactly as users.info does, so a
// workspace whose app was installed without it fails EVERY bot lookup — and
// without a log the only symptom is alerts quietly attributed to raw bot ids
// forever.
func TestReadHistory_LogsBotInfoFailure(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	botOnly := slackapi.Message{}
	botOnly.BotID = "B_ALERTS"
	botOnly.SubType = "bot_message"
	botOnly.Text = "disk full"
	botOnly.Timestamp = "100.1"

	fake := &fakeHistoryClient{replies: []slackapi.Message{botOnly}, botErr: assert.AnError}
	prev := historyClientFactory
	historyClientFactory = func(_ channelkinds.Deps) historyClient { return fake }
	t.Cleanup(func() { historyClientFactory = prev })

	page, err := (&Kind{}).ReadHistory(
		logf.IntoContext(context.Background(), capLogger),
		channelkinds.Deps{}, "thread:C123:100.0", channelkinds.ReadHistoryOpts{Limit: 50})
	require.NoError(t, err, "a bots.info failure must never fail the page")
	require.Len(t, page.Messages, 1)
	assert.Equal(t, "B_ALERTS", page.Messages[0].AuthorDisplayName, "degrades to the bot id")

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "B_ALERTS", "the log must name the bot that failed to resolve")
	assert.Contains(t, strings.ToLower(joined), "bots.info", "and must name what failed")
}

func TestReadHistory_BadChannelKey(t *testing.T) {
	_, err := (&Kind{}).ReadHistory(context.Background(), channelkinds.Deps{},
		"dm:U123", channelkinds.ReadHistoryOpts{})
	require.Error(t, err)
}
