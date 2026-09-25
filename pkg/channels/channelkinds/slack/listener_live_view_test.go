package slack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// recordingMinter implements channelkinds.ArtifactViewMinter, recording the
// args it was called with and returning canned results.
type recordingMinter struct {
	gotArtifact, gotSession, gotBackLink string
	gotSubject                           identity.Subject
	gotSubjectVerified                   bool
	url                                  string
	err                                  error
	called                               bool
}

func (m *recordingMinter) MintArtifactViewLink(artifactID, sessionRef string, subject identity.Principal, backLink string) (string, error) {
	m.called = true
	subj, err := subject.Subject()
	if err != nil {
		return "", err
	}
	m.gotArtifact, m.gotSession, m.gotSubject, m.gotBackLink = artifactID, sessionRef, subj, backLink
	m.gotSubjectVerified = subject.EmailVerified()
	return m.url, m.err
}

// liveViewClickCB builds a block_actions callback for a "View live" click.
func liveViewClickCB(t *testing.T, val liveViewButtonValue) slackapi.InteractionCallback {
	t.Helper()
	b, err := json.Marshal(val)
	require.NoError(t, err, "marshal value")
	return slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_CLICKER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: liveViewActionID, Value: string(b)},
			},
		},
	}
}

func goodValue() liveViewButtonValue {
	return liveViewButtonValue{V: "live_view", A: "art-1", S: "default/sess-1", C: "C01", T: "1700000000.000100"}
}

// TestHandleLiveViewClick_SubjectIsCanonicalNotRawEmail guards the fix for the
// regression where the link Subject was the clicker's RAW email. On an OIDC-off
// install identityd's trust-link fallback mints the webd cookie from that
// Subject and gates CheckArtifactView on it — and SpiceDB rejects a raw email
// (@/. violate the object-id regex). The Subject must be the canonical
// user:<base64> form, matching every other channelsd link.
func TestHandleLiveViewClick_SubjectIsCanonicalNotRawEmail(t *testing.T) {
	idents := NewIdentityCache(8)
	idents.Put(userInfo{UserID: "U_CLICKER", Email: "alice@example.com", TeamID: "T1"})
	m := &recordingMinter{url: "https://webd.example.com/artifact-view?d=NEW&sig=SIG"}
	l := &slackListener{
		deps:            channelkinds.Deps{ArtifactViewMinter: m},
		installedTeamID: "T1",
		idents:          idents,
	}
	c := &fakeSlackClient{}

	handled, err := l.handleLiveViewClick(context.Background(), liveViewClickCB(t, goodValue()), c)
	require.NoError(t, err)
	assert.True(t, handled)
	require.True(t, m.called, "minter must be called")

	canon, err := identity.EmailReference("alice@example.com").Canonical()
	require.NoError(t, err, "canonicalize reference email")
	want := canon.Subject()
	assert.Equal(t, want, m.gotSubject, "link Subject must be the canonical user:<base64> form")
	assert.NotContains(t, m.gotSubject, "@", "link Subject must never be a raw email (SpiceDB rejects @ in object ids)")
}

func TestHandleLiveViewClick(t *testing.T) {
	const freshURL = "https://webd.example.com/artifact-view?d=NEW&sig=SIG"

	cases := []struct {
		name             string
		value            liveViewButtonValue
		minter           *recordingMinter
		nilMinter        bool
		permalink        string
		permErr          error
		postEphemeralErr error
		check            func(t *testing.T, l *slackListener, c *fakeSlackClient, m *recordingMinter, handled bool, err error)
	}{
		{
			name:      "happy path: mints fresh, posts ephemeral Open button with back-link",
			value:     goodValue(),
			minter:    &recordingMinter{url: freshURL},
			permalink: "https://example.slack.com/archives/C01/p170?thread_ts=170&cid=C01",
			check: func(t *testing.T, l *slackListener, c *fakeSlackClient, m *recordingMinter, handled bool, err error) {
				require.NoError(t, err)
				assert.True(t, handled, "click must be handled")
				require.True(t, m.called, "minter must be called")
				assert.Equal(t, "art-1", m.gotArtifact)
				assert.Equal(t, "default/sess-1", m.gotSession)
				assert.Equal(t, "https://example.slack.com/archives/C01/p170?thread_ts=170&cid=C01", m.gotBackLink, "fresh mint carries the permalink back-link")
				require.Len(t, c.getPermalinkCalls, 1, "getPermalink called once")
				assert.Equal(t, "C01", c.getPermalinkCalls[0].Channel)
				assert.Equal(t, "1700000000.000100", c.getPermalinkCalls[0].Ts, "permalink uses the thread ts")
				require.Len(t, c.postEphemeralCalls, 1, "one ephemeral to the clicker")
				assert.Equal(t, "U_CLICKER", c.postEphemeralCalls[0].userID)
				assert.Equal(t, "C01", c.postEphemeralCalls[0].channelID)
			},
		},
		{
			name:    "permalink failure: still mints with empty back-link",
			value:   goodValue(),
			minter:  &recordingMinter{url: freshURL},
			permErr: errors.New("channel_not_found"),
			check: func(t *testing.T, l *slackListener, c *fakeSlackClient, m *recordingMinter, handled bool, err error) {
				require.NoError(t, err)
				assert.True(t, handled)
				require.True(t, m.called, "mint still happens")
				assert.Empty(t, m.gotBackLink, "back-link omitted when getPermalink fails")
				require.Len(t, c.postEphemeralCalls, 1, "Open button still posted")
			},
		},
		{
			name:   "no thread_ts: skips permalink, mints, posts ephemeral",
			value:  liveViewButtonValue{V: "live_view", A: "art-1", S: "default/sess-1", C: "C01", T: ""},
			minter: &recordingMinter{url: freshURL},
			check: func(t *testing.T, l *slackListener, c *fakeSlackClient, m *recordingMinter, handled bool, err error) {
				require.NoError(t, err)
				assert.True(t, handled)
				assert.Empty(t, c.getPermalinkCalls, "no permalink call without a thread ts")
				assert.Empty(t, m.gotBackLink)
				require.Len(t, c.postEphemeralCalls, 1)
			},
		},
		{
			name:   "mint returns empty (webd URL not ready): try-again ephemeral, no Open button",
			value:  goodValue(),
			minter: &recordingMinter{url: ""},
			check: func(t *testing.T, l *slackListener, c *fakeSlackClient, m *recordingMinter, handled bool, err error) {
				require.NoError(t, err)
				assert.True(t, handled)
				require.Len(t, c.postEphemeralCalls, 1, "an explanatory ephemeral is posted")
			},
		},
		{
			name:   "mint error: error ephemeral, handled",
			value:  goodValue(),
			minter: &recordingMinter{err: errors.New("signing key revoked")},
			check: func(t *testing.T, l *slackListener, c *fakeSlackClient, m *recordingMinter, handled bool, err error) {
				require.NoError(t, err, "mint error is surfaced to the user, not bubbled")
				assert.True(t, handled)
				require.Len(t, c.postEphemeralCalls, 1, "error ephemeral posted")
			},
		},
		{
			name:      "nil minter: not-configured ephemeral",
			value:     goodValue(),
			nilMinter: true,
			check: func(t *testing.T, l *slackListener, c *fakeSlackClient, m *recordingMinter, handled bool, err error) {
				require.NoError(t, err)
				assert.True(t, handled)
				require.Len(t, c.postEphemeralCalls, 1, "explains live view isn't configured")
			},
		},
		{
			name:   "missing artifactId in value: returns error, does not mint",
			value:  liveViewButtonValue{V: "live_view", A: "", S: "default/sess-1", C: "C01", T: ""},
			minter: &recordingMinter{url: freshURL},
			check: func(t *testing.T, l *slackListener, c *fakeSlackClient, m *recordingMinter, handled bool, err error) {
				require.Error(t, err, "missing artifactId must surface as an error")
				assert.True(t, handled, "a recognized action with a bad value is still handled")
				assert.False(t, m.called, "must not mint when the value is missing artifactId")
			},
		},
		{
			name:             "postEphemeral failure on success path: returns error",
			value:            goodValue(),
			minter:           &recordingMinter{url: freshURL},
			postEphemeralErr: errors.New("channel_not_found"),
			check: func(t *testing.T, l *slackListener, c *fakeSlackClient, m *recordingMinter, handled bool, err error) {
				require.Error(t, err, "a failed Open-button ephemeral must surface as an error")
				assert.True(t, handled)
				require.True(t, m.called, "mint still happened before the ephemeral failed")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeSlackClient{getPermalinkResult: tc.permalink, getPermalinkErr: tc.permErr, postEphemeralErr: tc.postEphemeralErr}
			deps := channelkinds.Deps{}
			if !tc.nilMinter {
				deps.ArtifactViewMinter = tc.minter
			}
			l := &slackListener{deps: deps, installedTeamID: "T1"}
			handled, err := l.handleLiveViewClick(context.Background(), liveViewClickCB(t, tc.value), c)
			tc.check(t, l, c, tc.minter, handled, err)
		})
	}
}

// TestHandleLiveViewClick_NotOurAction falls through for a foreign action_id.
func TestHandleLiveViewClick_NotOurAction(t *testing.T) {
	c := &fakeSlackClient{}
	l := &slackListener{deps: channelkinds.Deps{}}
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{{ActionID: "something_else", Value: "{}"}},
		},
	}
	handled, err := l.handleLiveViewClick(context.Background(), cb, c)
	require.NoError(t, err)
	assert.False(t, handled, "a non-live-view action must fall through")
	assert.Empty(t, c.postEphemeralCalls)
}

// TestHandleLiveViewClick_MalformedValue covers a recognized action_id whose
// button value is not valid JSON: the handler treats it as ours (handled) but
// surfaces the decode error rather than silently dropping the click.
func TestHandleLiveViewClick_MalformedValue(t *testing.T) {
	c := &fakeSlackClient{}
	l := &slackListener{deps: channelkinds.Deps{ArtifactViewMinter: &recordingMinter{}}}
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_CLICKER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: liveViewActionID, Value: "this is not json"},
			},
		},
	}
	handled, err := l.handleLiveViewClick(context.Background(), cb, c)
	require.Error(t, err, "a malformed button value must surface an error")
	assert.True(t, handled, "a recognized action_id with a bad value is still 'handled'")
}
