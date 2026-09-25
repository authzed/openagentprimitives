package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trustFakeClient answers users.info from a fixed table.
type trustFakeClient struct{ users map[string]*slackapi.User }

func (c trustFakeClient) GetUserInfoContext(_ context.Context, id string) (*slackapi.User, error) {
	return c.users[id], nil
}

func (c trustFakeClient) GetBotInfoContext(context.Context, slackapi.GetBotInfoParameters) (*slackapi.Bot, error) {
	return nil, nil
}

func (c trustFakeClient) GetConversationRepliesContext(context.Context, *slackapi.GetConversationRepliesParameters) ([]slackapi.Message, bool, string, error) {
	return nil, false, "", nil
}

func (c trustFakeClient) GetConversationHistoryContext(context.Context, *slackapi.GetConversationHistoryParameters) (*slackapi.GetConversationHistoryResponse, error) {
	return nil, nil
}

const installedTeam = "T_HOME"

func memberUser(id, email string) *slackapi.User {
	return &slackapi.User{ID: id, TeamID: installedTeam, RealName: "Member",
		Profile: slackapi.UserProfile{Email: email}}
}

// foreignUser is a Slack Connect participant from another workspace. Their
// home directory sets the profile email, and this deployment does not govern
// it.
func foreignUser(id, email string) *slackapi.User {
	return &slackapi.User{ID: id, TeamID: "T_OTHER", RealName: "Guest",
		Profile: slackapi.UserProfile{Email: email}}
}

func guestUser(id, email string) *slackapi.User {
	return &slackapi.User{ID: id, TeamID: installedTeam, IsRestricted: true, RealName: "Guest",
		Profile: slackapi.UserProfile{Email: email}}
}

func msgFrom(user, ts string) slackapi.Message {
	m := slackapi.Message{}
	m.User = user
	m.Text = "hello"
	m.Timestamp = ts
	return m
}

func authorEmails(t *testing.T, cli trustFakeClient, msgs []slackapi.Message) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, hm := range resolveAuthors(context.Background(), cli, msgs, "", installedTeam) {
		out[hm.AuthorExternalID] = hm.AuthorEmail
	}
	return out
}

// The LIVE inbound path zeroes a profile email for a foreign-workspace user, a
// guest, a stranger, a bot or a deleted account, so they key on a synthetic
// subject instead. The reason is stated in emailTrusted: the canonical SpiceDB
// subject is derived from that email, and a foreign or guest account is
// low-trust.
//
// The history path copied profile.Email with no such gate — and canonical
// derivation prefers the email whenever it is non-empty, so thread-adoption
// backfill minted grants under it. A Slack Connect participant whose home
// directory sets their email to a member's address therefore had their pasted
// values bound as slot grants, and participant standing written, under that
// member's identity.
//
// The pipeline believed the kind had already gated this: its own comment says
// these authors "may be guests / foreign-workspace users with no verified
// email".
func TestResolveAuthors_DropsAnUntrustedProfileEmail(t *testing.T) {
	cli := trustFakeClient{users: map[string]*slackapi.User{
		"U_MEMBER":  memberUser("U_MEMBER", "member@corp.example"),
		"U_FOREIGN": foreignUser("U_FOREIGN", "member@corp.example"),
		"U_GUEST":   guestUser("U_GUEST", "guest@corp.example"),
	}}

	got := authorEmails(t, cli, []slackapi.Message{
		msgFrom("U_MEMBER", "1"), msgFrom("U_FOREIGN", "2"), msgFrom("U_GUEST", "3"),
	})

	assert.Equal(t, "member@corp.example", got["U_MEMBER"],
		"a full member of the installed workspace keeps their verified email")
	assert.Empty(t, got["U_FOREIGN"],
		"a foreign-workspace author's self-asserted email must not become an authorization identity")
	assert.Empty(t, got["U_GUEST"],
		"a guest is low-trust on the live path and must be low-trust here too")
}

// With no installed team resolved, nothing can be shown to be a member, so no
// email is trusted. auth.test failing must not silently promote every author.
func TestResolveAuthors_FailsClosedWithoutAnInstalledTeam(t *testing.T) {
	cli := trustFakeClient{users: map[string]*slackapi.User{
		"U_MEMBER": memberUser("U_MEMBER", "member@corp.example"),
	}}

	out := resolveAuthors(context.Background(), cli, []slackapi.Message{msgFrom("U_MEMBER", "1")}, "", "")

	require.Len(t, out, 1)
	assert.Empty(t, out[0].AuthorEmail,
		"an unresolved installed team must fail closed, not trust every profile")
}

// Display names are unaffected: they are presentation, not identity, and
// dropping them would make the transcript unreadable for exactly the
// participants this gate is about.
func TestResolveAuthors_KeepsDisplayNamesForUntrustedAuthors(t *testing.T) {
	cli := trustFakeClient{users: map[string]*slackapi.User{
		"U_FOREIGN": foreignUser("U_FOREIGN", "someone@elsewhere.example"),
	}}

	out := resolveAuthors(context.Background(), cli, []slackapi.Message{msgFrom("U_FOREIGN", "1")}, "", installedTeam)

	require.Len(t, out, 1)
	assert.Equal(t, "Guest", out[0].AuthorDisplayName)
	assert.Empty(t, out[0].AuthorEmail)
}
