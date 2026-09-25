package slack

import (
	"context"
	"errors"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestKindSatisfiesUserProfileProvider(t *testing.T) {
	var _ channelkinds.UserProfileProvider = (*Kind)(nil)
}

func TestProfileFromSlackUser(t *testing.T) {
	cases := []struct {
		name string
		in   *slackapi.User
		want userprofile.Profile
	}{
		{
			name: "full member: every mapped field populated",
			in: &slackapi.User{
				TeamID: "T1", TZ: "America/New_York", TZLabel: "Eastern Standard Time",
				Locale: "en-US",
				Profile: slackapi.UserProfile{
					DisplayName: "Dana Whitfield", RealName: "Dana Whitfield",
					Email: "dana@example.com", Title: "Director of Support",
					Pronouns: "they/them", StatusText: "In a meeting",
					StartDate: "2021-03-01", Phone: "+1-555-0100",
				},
			},
			want: userprofile.Profile{
				DisplayName: "Dana Whitfield", RealName: "Dana Whitfield",
				Email: "dana@example.com", Title: "Director of Support",
				Pronouns: "they/them", Timezone: "America/New_York",
				TimezoneLabel: "Eastern Standard Time", Locale: "en-US",
				StatusText: "In a meeting", StartDate: "2021-03-01",
				AccountType: "member", Phone: "+1-555-0100",
			},
		},
		{
			name: "display name falls back to real name",
			in: &slackapi.User{
				Profile: slackapi.UserProfile{RealName: "Ravi Okonkwo"},
			},
			want: userprofile.Profile{DisplayName: "Ravi Okonkwo", RealName: "Ravi Okonkwo", AccountType: "member"},
		},
		{
			name: "admin is labelled admin",
			in:   &slackapi.User{IsAdmin: true, Profile: slackapi.UserProfile{DisplayName: "Ada"}},
			want: userprofile.Profile{DisplayName: "Ada", AccountType: "admin"},
		},
		{
			name: "restricted account is labelled guest",
			in:   &slackapi.User{IsRestricted: true, Profile: slackapi.UserProfile{DisplayName: "Guest"}},
			want: userprofile.Profile{DisplayName: "Guest", AccountType: "guest"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, profileFromSlackUser(tc.in))
		})
	}
}

func TestProfileFieldsAdvertisesEveryMappedField(t *testing.T) {
	k := &Kind{}
	assert.ElementsMatch(t, userprofile.AllFields(), k.ProfileFields(),
		"Slack maps every kind-neutral field today; update this test with the seam if that changes")
}

func TestFetchProfileByEmail(t *testing.T) {
	cases := []struct {
		name      string
		email     string
		stub      *stubProfileClient
		wantTitle string
		wantErrIs error
	}{
		{
			name:  "hit returns the mapped profile",
			email: "dana@example.com",
			stub: &stubProfileClient{teamID: "T1", user: &slackapi.User{
				TeamID:  "T1",
				Profile: slackapi.UserProfile{DisplayName: "Dana Whitfield", Title: "Director of Support"},
			}},
			wantTitle: "Director of Support",
		},
		{
			name:      "users_not_found maps to the sentinel",
			email:     "nobody@example.com",
			stub:      &stubProfileClient{teamID: "T1", err: errors.New("users_not_found")},
			wantErrIs: channelkinds.ErrProfileNotFound,
		},
		{
			name:      "empty email never reaches the API",
			email:     "",
			stub:      &stubProfileClient{teamID: "T1", err: errors.New("must not be called")},
			wantErrIs: channelkinds.ErrProfileNotFound,
		},
		{
			name:      "auth.test failure fails closed: cannot establish our workspace",
			email:     "dana@example.com",
			stub:      &stubProfileClient{user: &slackapi.User{TeamID: "T1"}}, // teamID "" => auth.test errors
			wantErrIs: channelkinds.ErrProfileNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &Kind{}
			k.clientFactory = func(channelkinds.LookupDeps) slackClient { return tc.stub }

			got, err := k.FetchProfileByEmail(context.Background(), channelkinds.LookupDeps{}, tc.email)
			if tc.wantErrIs != nil {
				require.ErrorIs(t, err, tc.wantErrIs)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantTitle, got.Title)
		})
	}
}

// TestFetchProfileRefusesUntrustedAccounts pins the security property: a
// restricted / foreign-workspace / bot / deleted account resolves to no
// profile at all, matching what emailTrusted already refuses for email.
func TestFetchProfileRefusesUntrustedAccounts(t *testing.T) {
	cases := []struct {
		name string
		user *slackapi.User
	}{
		{name: "restricted guest", user: &slackapi.User{IsRestricted: true, TeamID: "T1"}},
		{name: "ultra-restricted guest", user: &slackapi.User{IsUltraRestricted: true, TeamID: "T1"}},
		{name: "stranger", user: &slackapi.User{IsStranger: true, TeamID: "T1"}},
		{name: "bot", user: &slackapi.User{IsBot: true, TeamID: "T1"}},
		{name: "deleted", user: &slackapi.User{Deleted: true, TeamID: "T1"}},
		{name: "foreign workspace", user: &slackapi.User{TeamID: "T-OTHER"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &Kind{}
			k.clientFactory = func(channelkinds.LookupDeps) slackClient {
				return &stubProfileClient{teamID: "T1", user: tc.user}
			}
			_, err := k.FetchProfileByEmail(context.Background(), channelkinds.LookupDeps{}, "x@example.com")
			require.ErrorIs(t, err, channelkinds.ErrProfileNotFound)
		})
	}
}

// TestFetchProfileFailsClosedWithoutAuthTest pins the other half of the trust
// story: a client that cannot tell us which workspace is ours yields no
// profile, even for an otherwise perfectly ordinary member.
func TestFetchProfileFailsClosedWithoutAuthTest(t *testing.T) {
	k := &Kind{}
	k.clientFactory = func(channelkinds.LookupDeps) slackClient {
		return &noAuthTestClient{user: &slackapi.User{
			TeamID:  "T1",
			Profile: slackapi.UserProfile{DisplayName: "Dana Whitfield", Title: "Director of Support"},
		}}
	}
	_, err := k.FetchProfileByEmail(context.Background(), channelkinds.LookupDeps{}, "dana@example.com")
	require.ErrorIs(t, err, channelkinds.ErrProfileNotFound)
}

// TestFetchProfileRetriesAuthTestAfterTransientFailure pins the fix for a
// review finding: a failed auth.test must NOT be cached, because it is far
// more likely to be a transient rate-limit/network blip than a permanent
// condition. The first call sees the failure and fails closed; the second
// call, against the same Kind, must retry auth.test rather than reuse a
// cached "" and must succeed once the underlying client recovers.
func TestFetchProfileRetriesAuthTestAfterTransientFailure(t *testing.T) {
	cli := &flakyAuthTestClient{
		teamID: "T1",
		user: &slackapi.User{
			TeamID:  "T1",
			Profile: slackapi.UserProfile{DisplayName: "Dana Whitfield", Title: "Director of Support"},
		},
	}
	k := &Kind{}
	k.clientFactory = func(channelkinds.LookupDeps) slackClient { return cli }

	_, err := k.FetchProfileByEmail(context.Background(), channelkinds.LookupDeps{}, "dana@example.com")
	require.ErrorIs(t, err, channelkinds.ErrProfileNotFound,
		"first auth.test call fails; must fail closed rather than trust an unverified profile")

	got, err := k.FetchProfileByEmail(context.Background(), channelkinds.LookupDeps{}, "dana@example.com")
	require.NoError(t, err, "second call must retry auth.test instead of reusing a cached failure")
	assert.Equal(t, "Director of Support", got.Title)
}

// stubProfileClient serves one canned users.lookupByEmail result and one
// canned auth.test team ID. It implements authTester so the profile path can
// establish which workspace is "ours"; teamID "" simulates a client that
// cannot answer.
type stubProfileClient struct {
	slackClient // embedded: unimplemented methods panic if ever called
	user        *slackapi.User
	err         error
	teamID      string
}

func (s *stubProfileClient) GetUserByEmailContext(context.Context, string) (*slackapi.User, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.user, nil
}

func (s *stubProfileClient) AuthTestContext(context.Context) (*slackapi.AuthTestResponse, error) {
	if s.teamID == "" {
		return nil, errors.New("auth.test unavailable")
	}
	return &slackapi.AuthTestResponse{TeamID: s.teamID}, nil
}

// noAuthTestClient implements slackClient but NOT authTester, so the profile
// path cannot establish the installed workspace.
type noAuthTestClient struct {
	slackClient
	user *slackapi.User
}

func (c *noAuthTestClient) GetUserByEmailContext(context.Context, string) (*slackapi.User, error) {
	return c.user, nil
}

// flakyAuthTestClient fails auth.test on its first call and succeeds on every
// call after, with the canned teamID — it pins that a transient auth.test
// failure is retried on the next lookup rather than cached forever.
type flakyAuthTestClient struct {
	slackClient
	user   *slackapi.User
	teamID string
	calls  int
}

func (c *flakyAuthTestClient) GetUserByEmailContext(context.Context, string) (*slackapi.User, error) {
	return c.user, nil
}

func (c *flakyAuthTestClient) AuthTestContext(context.Context) (*slackapi.AuthTestResponse, error) {
	c.calls++
	if c.calls == 1 {
		return nil, errors.New("auth.test rate limited")
	}
	return &slackapi.AuthTestResponse{TeamID: c.teamID}, nil
}
