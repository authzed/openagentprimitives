package slack

import (
	"context"
	"slices"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// slackMethodBotScopes maps each Slack Web API method the history code path can
// call to the bot-token scopes Slack's method reference accepts for it.
//
// Several entries mean the method takes ANY of them: conversations.replies and
// conversations.history each want whichever *:history scope matches the
// conversation type, so a feature satisfies them by declaring at least one.
// users.info and bots.info each have exactly one, which makes the assertion
// below exact for the two calls this map exists for.
var slackMethodBotScopes = map[string][]string{
	"conversations.replies": {"channels:history", "groups:history", "im:history", "mpim:history"},
	"conversations.history": {"channels:history", "groups:history", "im:history", "mpim:history"},
	"users.info":            {"users:read"},
	// bots.info has no scope of its own — it reads the same workspace user
	// directory users.info does, and Slack's reference lists users:read for it.
	"bots.info": {"users:read"},
	// auth.test requires NO scope: Slack's reference lists it as callable with
	// any valid token, which is what makes it usable for "which workspace is
	// this token from?".
	//
	// The history path calls it because a profile email is an authorization
	// identity only for a member of the INSTALLED workspace, and answering
	// that needs the installed team id. An empty slice means the assertion
	// below has nothing to require, which is correct rather than a hole — a
	// feature cannot fail to hold a scope that does not exist.
	"auth.test": {},
}

// profileEmailScope is what Slack requires before users.info will return
// user.profile.email at all. Without it the field comes back empty and nothing
// errors, which is why its absence is invisible until someone looks at who a
// backfilled message was attributed to.
const profileEmailScope = "users:read.email"

// recordingHistoryClient answers every historyClient method with enough data to
// drive resolveAuthors to completion, recording the Slack method each call
// maps to.
//
// Recording the calls is the whole point: it lets the assertion derive the
// scopes a history feature needs from what the code ACTUALLY invokes, instead
// of from a second transcription of features.go. A transcription can only ever
// agree with what it was copied from.
type recordingHistoryClient struct {
	calls []string
}

// historyFixture is one human message and one webhook-posted app message: the
// pair that reaches every user-directory call resolveAuthors can make. The app
// message deliberately carries neither Username nor BotProfile, because that is
// the only shape for which authorLabel falls through to bots.info.
func historyFixture() []slackapi.Message {
	human := slackapi.Message{}
	human.User = "U_MEMBER"
	human.Text = "did the nightly run finish?"
	human.Timestamp = "100.1"

	app := slackapi.Message{}
	app.BotID = "B_ALERTS"
	app.SubType = "bot_message"
	app.Text = "nightly run complete"
	app.Timestamp = "100.2"

	return []slackapi.Message{human, app}
}

func (c *recordingHistoryClient) GetConversationRepliesContext(
	context.Context, *slackapi.GetConversationRepliesParameters,
) ([]slackapi.Message, bool, string, error) {
	c.calls = append(c.calls, "conversations.replies")
	return historyFixture(), false, "", nil
}

func (c *recordingHistoryClient) GetConversationHistoryContext(
	context.Context, *slackapi.GetConversationHistoryParameters,
) (*slackapi.GetConversationHistoryResponse, error) {
	c.calls = append(c.calls, "conversations.history")
	return &slackapi.GetConversationHistoryResponse{Messages: historyFixture()}, nil
}

// AuthTestContext makes this fake an authTester. A profile email is an
// authorization identity only for a member of the INSTALLED workspace, so the
// fixture has to say which workspace that is — and the user below has to be in
// it — or the email is correctly dropped and this test's users:read.email
// dependency stops being exercised.
func (c *recordingHistoryClient) AuthTestContext(_ context.Context) (*slackapi.AuthTestResponse, error) {
	c.calls = append(c.calls, "auth.test")
	return &slackapi.AuthTestResponse{TeamID: histTestTeam}, nil
}

func (c *recordingHistoryClient) GetUserInfoContext(_ context.Context, id string) (*slackapi.User, error) {
	c.calls = append(c.calls, "users.info")
	u := &slackapi.User{ID: id, RealName: "Member One", TeamID: histTestTeam}
	u.Profile.Email = "member.one@example.test"
	return u, nil
}

func (c *recordingHistoryClient) GetBotInfoContext(_ context.Context, p slackapi.GetBotInfoParameters) (*slackapi.Bot, error) {
	c.calls = append(c.calls, "bots.info")
	return &slackapi.Bot{ID: p.Bot, Name: "Nightly Alerts"}, nil
}

// installHistoryClient points historyClientFactory at cli for one test.
// historyClientFactory is package-global, so a test using this must not run in
// parallel with another that does.
func installHistoryClient(t *testing.T, cli historyClient) {
	t.Helper()
	prev := historyClientFactory
	historyClientFactory = func(channelkinds.Deps) historyClient { return cli }
	t.Cleanup(func() { historyClientFactory = prev })
}

// TestHistoryFeaturesDeclareTheScopesTheirCallsNeed pins the invariant a
// feature requirement exists to state: it must declare a scope for every Slack
// method its own code path calls.
//
// ThreadHistory and ChannelHistory both end in resolveAuthors, which calls
// users.info and bots.info and reads users.info's profile email — yet neither
// feature declared users:read or users:read.email. They worked only because
// mention_lookup is default-on and happens to contribute both, so turning that
// one capability off silently cost the history path its author names AND their
// emails. The email half reaches authorization: channelsd keys a participant's
// interact grant off HistoryMessage.AuthorEmail, and with no email every
// participant falls back to a synthetic subject instead of their own identity.
//
// Driving the real ReadHistory / ReadChannelHistory is what makes this a check
// on the code rather than on another copy of the scope list: add a call to a
// new Slack method and the recorder sees it whether or not anyone remembered
// features.go.
func TestHistoryFeaturesDeclareTheScopesTheirCallsNeed(t *testing.T) {
	cases := []struct {
		name    string
		feature channelfeatures.Feature
		read    func(t *testing.T) (channelkinds.HistoryPage, error)
	}{
		{
			name:    "ThreadHistory: conversations.replies, users.info, bots.info and the profile email",
			feature: channelfeatures.ThreadHistory,
			read: func(t *testing.T) (channelkinds.HistoryPage, error) {
				t.Helper()
				return (&Kind{}).ReadHistory(context.Background(), channelkinds.Deps{},
					"thread:C123:100.0", channelkinds.ReadHistoryOpts{Limit: 50})
			},
		},
		{
			name:    "ChannelHistory: conversations.history, users.info, bots.info and the profile email",
			feature: channelfeatures.ChannelHistory,
			read: func(t *testing.T) (channelkinds.HistoryPage, error) {
				t.Helper()
				binding := &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C123"}}
				return (&Kind{}).ReadChannelHistory(context.Background(), channelkinds.Deps{},
					binding, channelkinds.ChannelHistoryOpts{Limit: 50})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingHistoryClient{}
			installHistoryClient(t, rec)

			page, err := tc.read(t)
			require.NoError(t, err, "the fixture must read cleanly, or nothing below is exercised")

			req, ok := (&Kind{}).FeatureSupport()[tc.feature]
			require.Truef(t, ok, "the Slack kind must declare a requirement for %q", tc.feature)
			declared := req.Scopes

			called := slices.Compact(slices.Sorted(slices.Values(rec.calls)))
			require.NotEmpty(t, called, "the read made no Slack call; the fixture is not driving the code path")

			for _, method := range called {
				accepted, known := slackMethodBotScopes[method]
				require.Truef(t, known,
					"the history path calls %s, which slackMethodBotScopes does not cover — look the "+
						"method up in Slack's reference and add the scopes it lists", method)
				if len(accepted) == 0 {
					// A method Slack accepts on any valid token (auth.test).
					// There is no scope to declare, so requiring one would
					// fail every feature for a call that costs them nothing.
					continue
				}
				assert.Truef(t, slices.ContainsFunc(accepted, func(s string) bool {
					return slices.Contains(declared, s)
				}), "%q calls %s, which needs one of %v, but the feature declares %v",
					tc.feature, method, accepted, declared)
			}

			emails := 0
			for _, m := range page.Messages {
				if m.AuthorEmail != "" {
					emails++
				}
			}
			require.NotZero(t, emails,
				"the fixture must yield an author email, or the %s dependency is not exercised", profileEmailScope)
			assert.Containsf(t, declared, profileEmailScope,
				"%q puts users.info's profile email on HistoryMessage.AuthorEmail, which channelsd "+
					"canonicalizes into a participant's interact grant; without %s Slack omits the "+
					"field and every participant is keyed by a synthetic subject instead",
				tc.feature, profileEmailScope)
		})
	}
}
