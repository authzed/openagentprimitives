package slack

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// userEchoEnvelope builds a channelevents.Envelope carrying a UserEchoPayload.
func userEchoEnvelope(t *testing.T, pl channelevents.UserEchoPayload) channelevents.Envelope {
	t.Helper()
	b, err := json.Marshal(pl)
	require.NoError(t, err, "marshal UserEchoPayload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindUserEcho,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "sess-1"},
		PublishedAt: time.Now().UTC(),
		Payload:     b,
	}
}

// sessionWithChannelAndTeam is sessionWithChannel plus a slack team_id on
// External — needed so the sender can scope the mention lookup to the
// session's own workspace.
func sessionWithChannelAndTeam(channelID, threadTS, teamID string) channelkinds.SessionInfo {
	sess := sessionWithChannel(channelID, threadTS)
	sess.Channel.External["team_id"] = teamID
	return sess
}

// firstPostedText extracts the "text" query param a chat.postMessage call
// would have carried, materializing the MsgOptions the same way the other
// slack sender tests do (slackapi.UnsafeApplyMsgOptions).
func firstPostedText(t *testing.T, chanID string, opts []slackapi.MsgOption) string {
	t.Helper()
	_, vals, err := slackapi.UnsafeApplyMsgOptions("test-token", chanID, "http://test.invalid/", opts...)
	require.NoError(t, err, "UnsafeApplyMsgOptions")
	return vals.Get("text")
}

// TestUserEchoSender_ResolvedMention_RendersMentionViaAndText verifies the
// happy path: a UserIdentity with a matching slack ChannelIdentity in this
// session's team produces a real <@U123> mention, plus the viewurn.Describe
// text and the (escaped) message text.
func TestUserEchoSender_ResolvedMention_RendersMentionViaAndText(t *testing.T) {
	const (
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
		teamID   = "T1"
		email    = "alice@example.com"
	)
	canonical := "user:" + emailCanonical(email)
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(identity.Subject(canonical))},
		Status: spiceboxv1alpha1.UserIdentityStatus{ChannelIdentities: []spiceboxv1alpha1.ChannelIdentity{
			{Kind: "slack", Domain: teamID, ExternalID: "U123", DisplayName: "Alice"},
		}},
	}
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ui).Build()
	c := &fakeSlackClient{}
	s := &userEchoSender{client: c, k8s: k8s}

	pl := channelevents.UserEchoPayload{
		Text: "the CTA padding is wrong",
		Author: channelevents.ExternalIdentity{
			Kind:  "idp",
			Email: email,
		},
		Via: "urn:ap:view:artifact:artifact-3f2a1b8c",
	}
	env := userEchoEnvelope(t, pl)
	sess := sessionWithChannelAndTeam(chanID, threadTS, teamID)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")
	require.Len(t, c.postMessageCalls, 1, "expected 1 PostMessage call")

	got := c.postMessageCalls[0]
	assert.Equal(t, chanID, got.channelID, "channelID")
	text := firstPostedText(t, chanID, got.options)
	assert.Equal(t, "💬 <@U123>, via the artifact view of artifact-3f2a1b8c: the CTA padding is wrong", text)
}

// TestUserEchoSender_NoMention_FallsBackToEscapedEmail verifies that when
// mentionForSubject misses (no UserIdentity / no slack identity in this
// workspace), the sender falls back to the author's escaped email rather
// than failing the send.
func TestUserEchoSender_NoMention_FallsBackToEscapedEmail(t *testing.T) {
	const (
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
		teamID   = "T1"
	)
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).Build() // no UserIdentity objects
	c := &fakeSlackClient{}
	s := &userEchoSender{client: c, k8s: k8s}

	pl := channelevents.UserEchoPayload{
		Text: "<script>alert(1)</script> looks off",
		Author: channelevents.ExternalIdentity{
			Kind:  "idp",
			Email: "bob@example.com",
		},
		Via: "",
	}
	env := userEchoEnvelope(t, pl)
	sess := sessionWithChannelAndTeam(chanID, threadTS, teamID)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")
	require.Len(t, c.postMessageCalls, 1, "expected 1 PostMessage call")

	text := firstPostedText(t, chanID, c.postMessageCalls[0].options)
	assert.Equal(t, "💬 bob@example.com: &lt;script&gt;alert(1)&lt;/script&gt; looks off", text,
		"no via clause when Describe is empty; message text is slack-escaped")
}

// TestUserEchoSender_HumanTextIsEscaped pins that mrkdwn-special characters in
// the human-authored message text are escaped, distinct from the mention
// markup (which must NOT be escaped).
func TestUserEchoSender_HumanTextIsEscaped(t *testing.T) {
	const (
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
		teamID   = "T1"
		email    = "carol@example.com"
	)
	canonical := "user:" + emailCanonical(email)
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(identity.Subject(canonical))},
		Status: spiceboxv1alpha1.UserIdentityStatus{ChannelIdentities: []spiceboxv1alpha1.ChannelIdentity{
			{Kind: "slack", Domain: teamID, ExternalID: "U456"},
		}},
	}
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ui).Build()
	c := &fakeSlackClient{}
	s := &userEchoSender{client: c, k8s: k8s}

	pl := channelevents.UserEchoPayload{
		Text: "a < b & b > c",
		Author: channelevents.ExternalIdentity{
			Kind:  "idp",
			Email: email,
		},
		Via: "urn:ap:view:chat",
	}
	env := userEchoEnvelope(t, pl)
	sess := sessionWithChannelAndTeam(chanID, threadTS, teamID)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")
	require.Len(t, c.postMessageCalls, 1, "expected 1 PostMessage call")

	text := firstPostedText(t, chanID, c.postMessageCalls[0].options)
	assert.Equal(t, "💬 <@U456>, via the web chat: a &lt; b &amp; b &gt; c", text,
		"mention markup unescaped; human text escaped")
}

// TestUserEchoSender_ThreadsUnderSessionThread verifies the mirrored message
// posts into the session's own thread, matching every other slack sender.
func TestUserEchoSender_ThreadsUnderSessionThread(t *testing.T) {
	const (
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
		teamID   = "T1"
	)
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	c := &fakeSlackClient{}
	s := &userEchoSender{client: c, k8s: k8s}

	pl := channelevents.UserEchoPayload{
		Text:   "hello",
		Author: channelevents.ExternalIdentity{Kind: "idp", Email: "dan@example.com"},
	}
	env := userEchoEnvelope(t, pl)
	sess := sessionWithChannelAndTeam(chanID, threadTS, teamID)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")
	require.Len(t, c.postMessageCalls, 1)

	_, vals, err := slackapi.UnsafeApplyMsgOptions("test-token", chanID,
		"http://test.invalid/", c.postMessageCalls[0].options...)
	require.NoError(t, err)
	assert.Equal(t, threadTS, vals.Get("thread_ts"), "echo must thread under the session's own thread")
}

// TestUserEchoSender_RegisteredOnSubChannelSender verifies kind.go's
// SubChannelSender switch routes "user_echo" to the userEchoSender rather
// than returning nil (the graceful-degradation default for unknown names).
func TestUserEchoSender_RegisteredOnSubChannelSender(t *testing.T) {
	k := &Kind{}
	sender := k.SubChannelSender(string(channelevents.KindUserEcho), channelkinds.Deps{})
	require.NotNil(t, sender, "user_echo must be a registered sub-channel, not a nil-degrade")
	_, ok := sender.(*userEchoSender)
	assert.True(t, ok, "SubChannelSender(user_echo) must return a *userEchoSender")
}
