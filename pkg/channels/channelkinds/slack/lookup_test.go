package slack

import (
	"context"
	"errors"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// withFakeClient swaps the Kind's resolver client factory for the test
// double. Cleanup restores the default factory.
func withFakeClient(t *testing.T, k *Kind, c slackClient) {
	t.Helper()
	prev := k.clientFactory
	k.clientFactory = func(_ channelkinds.LookupDeps) slackClient { return c }
	t.Cleanup(func() { k.clientFactory = prev })
}

func TestLookupUserEmailHit(t *testing.T) {
	c := &fakeSlackClient{}
	c.lookupByEmail = map[string]*slackapi.User{
		"fred@example.com": {
			ID:      "U01234",
			TeamID:  "T1",
			Profile: slackapi.UserProfile{Email: "fred@example.com", DisplayName: "Fred Smith", RealName: "Frederick Smith"},
		},
	}
	k := &Kind{}
	withFakeClient(t, k, c)

	id, name, err := k.LookupUser(context.Background(), channelkinds.LookupDeps{},
		channelkinds.MentionLookupEmail, "fred@example.com")
	require.NoError(t, err, "LookupUser")
	assert.Equal(t, "U01234", id, "externalID")
	assert.Equal(t, "Fred Smith", name, "displayName")
}

func TestLookupUserEmailMiss(t *testing.T) {
	c := &fakeSlackClient{lookupByEmailErr: slackapi.SlackErrorResponse{Err: "users_not_found"}}
	k := &Kind{}
	withFakeClient(t, k, c)

	_, _, err := k.LookupUser(context.Background(), channelkinds.LookupDeps{},
		channelkinds.MentionLookupEmail, "nobody@example.com")
	assert.ErrorIs(t, err, channelkinds.ErrMentionNotFound, "users_not_found should map to ErrMentionNotFound")
}

func TestLookupUserEmailTransportError(t *testing.T) {
	c := &fakeSlackClient{lookupByEmailErr: errors.New("rate limited")}
	k := &Kind{}
	withFakeClient(t, k, c)

	_, _, err := k.LookupUser(context.Background(), channelkinds.LookupDeps{},
		channelkinds.MentionLookupEmail, "fred@example.com")
	require.Error(t, err, "transport error must surface")
	assert.NotContains(t, err.Error(), "no user matched",
		"transport error must NOT be reported as a name-not-found error")
	assert.NotErrorIs(t, err, channelkinds.ErrMentionNotFound,
		"transport error must NOT be ErrMentionNotFound")
}

// TestLookupUserByName covers the name-based lookup matrix: a single
// match, case-insensitive match, zero matches, and the realName fallback
// when DisplayName is empty. They share the same shape (usersList input,
// name query, expected id/name/err). Ambiguity across active accounts is
// no longer a bare error — it surfaces candidates for the model — and is
// covered in TestLookupByNameSkipsDeactivatedAndPrefersMember.
func TestLookupUserByName(t *testing.T) {
	cases := []struct {
		name    string
		users   []slackapi.User
		query   string
		wantID  string
		wantNm  string
		wantErr error // nil → success; otherwise errors.Is target
	}{
		{
			name: "unique DisplayName match: returns user",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Fred Smith"}},
				{ID: "U02", Profile: slackapi.UserProfile{DisplayName: "Jane Doe"}},
			},
			query:  "Fred Smith",
			wantID: "U01",
			wantNm: "Fred Smith",
		},
		{
			name: "case-insensitive DisplayName match: returns user",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Fred Smith"}},
			},
			query:  "fred smith",
			wantID: "U01",
			wantNm: "Fred Smith",
		},
		{
			name: "no matching DisplayName: ErrMentionNotFound",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Fred"}},
			},
			query:   "Mallory",
			wantErr: channelkinds.ErrMentionNotFound,
		},
		{
			name: "empty DisplayName falls back to RealName: returns user",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "", RealName: "Fred Smith"}},
			},
			query:  "fred smith",
			wantID: "U01",
			wantNm: "Fred Smith",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeSlackClient{usersList: tc.users}
			k := &Kind{}
			withFakeClient(t, k, c)

			id, name, err := k.LookupUser(context.Background(), channelkinds.LookupDeps{},
				channelkinds.MentionLookupName, tc.query)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr, "expected sentinel error")
				return
			}
			require.NoError(t, err, "LookupUser")
			assert.Equal(t, tc.wantID, id, "externalID")
			assert.Equal(t, tc.wantNm, name, "displayName")
		})
	}
}

func TestLookupUserNameCachedAcrossCalls(t *testing.T) {
	c := &fakeSlackClient{
		usersList: []slackapi.User{{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Fred"}}},
	}
	k := &Kind{}
	withFakeClient(t, k, c)
	k.resolverInit()
	// Drop memberCache TTL so the first call populates and the second hits cache.
	k.memberCache.ttl = time.Hour

	_, _, _ = k.LookupUser(context.Background(), channelkinds.LookupDeps{},
		channelkinds.MentionLookupName, "Fred")
	_, _, _ = k.LookupUser(context.Background(), channelkinds.LookupDeps{},
		channelkinds.MentionLookupName, "Fred")

	assert.Equal(t, 1, c.usersListCalls, "second call should hit cache")
}

// TestLookupUserAny verifies the routing logic for MentionLookupAny:
// an input containing "@" goes through GetUserByEmailContext only
// (no usersList scan), and an input without "@" goes through
// usersList only.
func TestLookupUserAny(t *testing.T) {
	cases := []struct {
		name           string
		setupClient    func() *fakeSlackClient
		query          string
		wantID         string
		wantEmailCalls int
		wantListCalls  int
	}{
		{
			name: "query with @: routes to email lookup only",
			setupClient: func() *fakeSlackClient {
				return &fakeSlackClient{
					lookupByEmail: map[string]*slackapi.User{
						"fred@example.com": {ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Fred"}},
					},
				}
			},
			query:          "fred@example.com",
			wantID:         "U01",
			wantEmailCalls: 1,
			wantListCalls:  0,
		},
		{
			name: "query without @: routes to name lookup only",
			setupClient: func() *fakeSlackClient {
				return &fakeSlackClient{
					usersList: []slackapi.User{{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Fred"}}},
				}
			},
			query:          "Fred",
			wantID:         "U01",
			wantEmailCalls: 0,
			wantListCalls:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.setupClient()
			k := &Kind{}
			withFakeClient(t, k, c)

			id, _, err := k.LookupUser(context.Background(), channelkinds.LookupDeps{},
				channelkinds.MentionLookupAny, tc.query)
			require.NoError(t, err, "LookupUser")
			assert.Equal(t, tc.wantID, id, "externalID")
			assert.Len(t, c.lookupByEmailCalls, tc.wantEmailCalls, "lookupByEmail call count")
			assert.Equal(t, tc.wantListCalls, c.usersListCalls, "usersList call count")
		})
	}
}

func TestLookupUserUnsupportedKind(t *testing.T) {
	c := &fakeSlackClient{}
	k := &Kind{}
	withFakeClient(t, k, c)

	_, _, err := k.LookupUser(context.Background(), channelkinds.LookupDeps{},
		channelkinds.MentionLookupKind("phone"), "+1-555-0100")
	assert.ErrorIs(t, err, channelkinds.ErrMentionUnsupported,
		"unknown lookup kind should return ErrMentionUnsupported")
}
