package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// candidateIDs extracts the ExternalIDs from a candidate slice in order,
// so tests can assert ranking without spelling out every field.
func candidateIDs(cands []channelkinds.MentionCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.ExternalID)
	}
	return out
}

// candidateTypes extracts the AccountType flag from each candidate in
// order, so tests can assert the member/guest standing surfaced to the
// model alongside the ranking.
func candidateTypes(cands []channelkinds.MentionCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.AccountType)
	}
	return out
}

// TestLookupByNameSkipsDeactivatedAndPrefersMember covers the exact-name
// path's account-standing rules: deactivated and bot accounts are never
// matched (pinging a former employee is the wrong person), and among
// active matches a lone full member (employee) wins over any guests. When
// no single winner emerges, the matches are surfaced as candidates —
// member-first — for the model to choose, rather than a dead-end error.
//
// All names here are fabricated (repo rule: no real names in code).
func TestLookupByNameSkipsDeactivatedAndPrefersMember(t *testing.T) {
	const dupName = "Robin Hart"
	cases := []struct {
		name          string
		users         []slackapi.User
		query         string
		wantID        string   // non-empty → expect exact success with this ID
		wantCandIDs   []string // non-nil → expect MentionNearMissError with these IDs, in order
		wantCandTypes []string // AccountType flag expected per candidate, same order
		wantErr       error    // else → this sentinel
	}{
		{
			name: "deactivated twin skipped, sole active member returned",
			users: []slackapi.User{
				{ID: "U_OLD", Deleted: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_EMP", Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:  dupName,
			wantID: "U_EMP",
		},
		{
			name: "only a deactivated account matches: not returned",
			users: []slackapi.User{
				{ID: "U_OLD", Deleted: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:   dupName,
			wantErr: channelkinds.ErrMentionNotFound,
		},
		{
			name: "member and guest share a name: member auto-picked",
			users: []slackapi.User{
				{ID: "U_GST", IsRestricted: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_EMP", Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:  dupName,
			wantID: "U_EMP",
		},
		{
			name: "two members share a name: candidates returned member-first for the model",
			users: []slackapi.User{
				{ID: "U_A", Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_B", Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:         dupName,
			wantCandIDs:   []string{"U_A", "U_B"},
			wantCandTypes: []string{"member", "member"},
		},
		{
			name: "no member, two guests share a name: candidates returned for the model",
			users: []slackapi.User{
				{ID: "U_G2", IsUltraRestricted: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_G1", IsRestricted: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:         dupName,
			wantCandIDs:   []string{"U_G1", "U_G2"},
			wantCandTypes: []string{"guest", "guest"},
		},
		{
			name: "two members and a guest share a name: guest ranked after members",
			users: []slackapi.User{
				{ID: "U_GST", IsRestricted: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_B", Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_A", Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:         dupName,
			wantCandIDs:   []string{"U_A", "U_B", "U_GST"},
			wantCandTypes: []string{"member", "member", "guest"},
		},
		{
			name: "shared-channel strangers share a name: ranked and labelled as guests",
			users: []slackapi.User{
				{ID: "U_S2", IsStranger: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_S1", IsStranger: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:         dupName,
			wantCandIDs:   []string{"U_S1", "U_S2"},
			wantCandTypes: []string{"guest", "guest"},
		},
		{
			name: "member and a shared-channel stranger share a name: member auto-picked",
			users: []slackapi.User{
				{ID: "U_STR", IsStranger: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_EMP", Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:  dupName,
			wantID: "U_EMP",
		},
		{
			name: "bot with matching name is never matched",
			users: []slackapi.User{
				{ID: "U_BOT", IsBot: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:   dupName,
			wantErr: channelkinds.ErrMentionNotFound,
		},
		{
			name: "deactivated ignored, guest and member present: member wins",
			users: []slackapi.User{
				{ID: "U_OLD", Deleted: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_GST", IsRestricted: true, Profile: slackapi.UserProfile{DisplayName: dupName}},
				{ID: "U_EMP", Profile: slackapi.UserProfile{DisplayName: dupName}},
			},
			query:  dupName,
			wantID: "U_EMP",
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
				require.NoError(t, err, "expected a single confident match")
				assert.Equal(t, tc.wantID, id, "externalID")
			case tc.wantCandIDs != nil:
				var nm *channelkinds.MentionNearMissError
				require.ErrorAs(t, err, &nm, "expected candidate list for the model")
				assert.Equal(t, tc.wantCandIDs, candidateIDs(nm.Candidates), "candidates in rank order")
				assert.Equal(t, tc.wantCandTypes, candidateTypes(nm.Candidates), "account-type flag per candidate")
				assert.True(t, nm.Exact, "exact-name ambiguity must flag Exact so the tool words it accordingly")
			default:
				assert.ErrorIs(t, err, tc.wantErr, "expected sentinel error")
			}
		})
	}
}
