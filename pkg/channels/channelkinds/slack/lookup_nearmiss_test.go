package slack

import (
	"context"
	"errors"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestLookupUserByNameNearMiss covers the third pass of the name path:
// when neither exact pass (display_name, real_name) matches, the lookup
// returns a MentionNearMissError carrying ranked candidates — users whose
// surname (last token) matches, or whose full name is within edit
// distance — for the agent to judge. Each candidate carries its
// account-standing flag (member/guest) so the model can weigh who was
// meant. Exact matches always win; zero candidates is still
// ErrMentionNotFound.
func TestLookupUserByNameNearMiss(t *testing.T) {
	cases := []struct {
		name      string
		users     []slackapi.User
		query     string
		wantID    string                          // non-empty → expect exact-match success with this ID
		wantCands []channelkinds.MentionCandidate // non-nil → expect near-miss error with exactly these, in rank order
		wantErr   error                           // sentinel expectation when neither of the above
	}{
		{
			name: "nickname given-name with matching surname: unique near-miss candidate",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Freddie Cavendish"}},
				{ID: "U02", Profile: slackapi.UserProfile{DisplayName: "Jane Doe"}},
			},
			query: "Frederick Cavendish",
			wantCands: []channelkinds.MentionCandidate{
				{ExternalID: "U01", DisplayName: "Freddie Cavendish", AccountType: "member"},
			},
		},
		{
			name: "guest near-miss carries the guest flag",
			users: []slackapi.User{
				{ID: "U01", IsRestricted: true, Profile: slackapi.UserProfile{DisplayName: "Freddie Cavendish"}},
			},
			query: "Frederick Cavendish",
			wantCands: []channelkinds.MentionCandidate{
				{ExternalID: "U01", DisplayName: "Freddie Cavendish", AccountType: "guest"},
			},
		},
		{
			name: "surname match only, given name nothing alike: still a candidate",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Bob Cavendish"}},
			},
			query: "Frederick Cavendish",
			wantCands: []channelkinds.MentionCandidate{
				{ExternalID: "U01", DisplayName: "Bob Cavendish", AccountType: "member"},
			},
		},
		{
			name: "close misspelling within edit distance, surname token differs: candidate",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Fred Smith"}},
			},
			query: "Fred Smyth",
			wantCands: []channelkinds.MentionCandidate{
				{ExternalID: "U01", DisplayName: "Fred Smith", AccountType: "member"},
			},
		},
		{
			name: "empty DisplayName: candidate matched and named via RealName",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "", RealName: "Freddie Cavendish"}},
			},
			query: "Frederick Cavendish",
			wantCands: []channelkinds.MentionCandidate{
				{ExternalID: "U01", DisplayName: "Freddie Cavendish", AccountType: "member"},
			},
		},
		{
			name: "candidates ranked by edit distance, closest first",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Bob Cavendish"}},
				{ID: "U02", Profile: slackapi.UserProfile{DisplayName: "Freddie Cavendish"}},
			},
			query: "Frederick Cavendish",
			wantCands: []channelkinds.MentionCandidate{
				{ExternalID: "U02", DisplayName: "Freddie Cavendish", AccountType: "member"},
				{ExternalID: "U01", DisplayName: "Bob Cavendish", AccountType: "member"},
			},
		},
		{
			name: "more than five candidates: capped at five, distance then ID order",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Ann Cavendish"}},
				{ID: "U02", Profile: slackapi.UserProfile{DisplayName: "Bea Cavendish"}},
				{ID: "U03", Profile: slackapi.UserProfile{DisplayName: "Cal Cavendish"}},
				{ID: "U04", Profile: slackapi.UserProfile{DisplayName: "Dot Cavendish"}},
				{ID: "U05", Profile: slackapi.UserProfile{DisplayName: "Eve Cavendish"}},
				{ID: "U06", Profile: slackapi.UserProfile{DisplayName: "Fay Cavendish"}},
				{ID: "U07", Profile: slackapi.UserProfile{DisplayName: "Gus Cavendish"}},
			},
			query: "Zed Cavendish",
			wantCands: []channelkinds.MentionCandidate{
				{ExternalID: "U02", DisplayName: "Bea Cavendish", AccountType: "member"}, // distance 2; all others 3
				{ExternalID: "U01", DisplayName: "Ann Cavendish", AccountType: "member"},
				{ExternalID: "U03", DisplayName: "Cal Cavendish", AccountType: "member"},
				{ExternalID: "U04", DisplayName: "Dot Cavendish", AccountType: "member"},
				{ExternalID: "U05", DisplayName: "Eve Cavendish", AccountType: "member"},
			},
		},
		{
			name: "exact match present: near-miss pass never runs",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Fred Smith"}},
				{ID: "U02", Profile: slackapi.UserProfile{DisplayName: "Freddy Smith"}},
			},
			query:  "Fred Smith",
			wantID: "U01",
		},
		{
			name: "deleted and bot users are not candidates: ErrMentionNotFound",
			users: []slackapi.User{
				{ID: "U01", Deleted: true, Profile: slackapi.UserProfile{DisplayName: "Frank Cavendish"}},
				{ID: "U02", IsBot: true, Profile: slackapi.UserProfile{DisplayName: "Botty Cavendish"}},
			},
			query:   "Frederick Cavendish",
			wantErr: channelkinds.ErrMentionNotFound,
		},
		{
			name: "nothing close: ErrMentionNotFound unchanged",
			users: []slackapi.User{
				{ID: "U01", Profile: slackapi.UserProfile{DisplayName: "Jane Doe"}},
			},
			query:   "Frederick Cavendish",
			wantErr: channelkinds.ErrMentionNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeSlackClient{usersList: tc.users}
			k := &Kind{}
			withFakeClient(t, k, c)

			id, _, err := k.LookupUser(context.Background(), channelkinds.LookupDeps{},
				channelkinds.MentionLookupName, tc.query)
			switch {
			case tc.wantID != "":
				require.NoError(t, err, "exact match must succeed without a near-miss error")
				assert.Equal(t, tc.wantID, id, "externalID")
			case tc.wantCands != nil:
				var nm *channelkinds.MentionNearMissError
				require.ErrorAs(t, err, &nm, "expected MentionNearMissError")
				assert.Equal(t, tc.wantCands, nm.Candidates, "candidates in rank order")
				assert.False(t, nm.Exact, "fuzzy near-miss must not set Exact")
				assert.NotEmpty(t, nm.Error(), "Error() must be non-empty")
			default:
				assert.ErrorIs(t, err, tc.wantErr, "expected sentinel error")
				var nm *channelkinds.MentionNearMissError
				assert.False(t, errors.As(err, &nm), "must not be a near-miss error")
			}
		})
	}
}
