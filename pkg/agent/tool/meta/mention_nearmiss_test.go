package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestLookupUserForMentionExecuteNearMiss covers the candidate-list
// contract: when LookupUser returns a MentionNearMissError, the tool
// renders each candidate's mention token (via RenderMention) alongside
// its display name and account-standing flag, and returns a NON-error
// result — the model judges which candidate, if any, is the intended
// person. The wording differs for a fuzzy near-miss (approximate names)
// versus an exact-name ambiguity (several people share the name). An
// empty candidate list is defensive fallback to the not-found error shape.
func TestLookupUserForMentionExecuteNearMiss(t *testing.T) {
	cases := []struct {
		name         string
		exact        bool
		candidates   []channelkinds.MentionCandidate
		wantIsError  bool
		wantContains []string
	}{
		{
			name: "fuzzy single candidate: token + display name + standing rendered, IsError false",
			candidates: []channelkinds.MentionCandidate{
				{ExternalID: "U9", DisplayName: "Freddie Cavendish", AccountType: "member"},
			},
			wantIsError:  false,
			wantContains: []string{"no exact match", "<@U9>", "Freddie Cavendish", "(member)", "plain text"},
		},
		{
			name: "fuzzy multiple candidates: every candidate rendered with its standing",
			candidates: []channelkinds.MentionCandidate{
				{ExternalID: "U9", DisplayName: "Freddie Cavendish", AccountType: "member"},
				{ExternalID: "U10", DisplayName: "Bob Cavendish", AccountType: "guest"},
			},
			wantIsError:  false,
			wantContains: []string{"<@U9>", "Freddie Cavendish", "(member)", "<@U10>", "Bob Cavendish", "(guest)"},
		},
		{
			name:  "exact-name ambiguity: distinct wording steering toward the member",
			exact: true,
			candidates: []channelkinds.MentionCandidate{
				{ExternalID: "U9", DisplayName: "Robin Hart", AccountType: "member"},
				{ExternalID: "U10", DisplayName: "Robin Hart", AccountType: "guest"},
			},
			wantIsError:  false,
			wantContains: []string{"multiple", "<@U9>", "(member)", "<@U10>", "(guest)", "member"},
		},
		{
			name:         "zero candidates: falls back to the not-found error shape",
			candidates:   nil,
			wantIsError:  true,
			wantContains: []string{"no user found"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &stubKind{
				supports: []channelkinds.MentionLookupKind{channelkinds.MentionLookupName},
				mention:  func(id string) string { return "<@" + id + ">" },
				lookup: func(_ context.Context, _ channelkinds.MentionLookupKind, _ string) (string, string, error) {
					return "", "", &channelkinds.MentionNearMissError{Exact: tc.exact, Candidates: tc.candidates}
				},
			}
			tl := newLookupTool(t, k)
			require.NotNil(t, tl, "tool must be non-nil")

			args, err := json.Marshal(map[string]string{"kind": "name", "value": "Robin Hart"})
			require.NoError(t, err, "marshal args")
			res, err := tl.Execute(context.Background(), args, nil)
			require.NoError(t, err, "Execute")
			assert.Equal(t, tc.wantIsError, res.IsError, "IsError")
			assert.True(t, res.Trusted, "Trusted")
			for _, want := range tc.wantContains {
				assert.Contains(t, res.Content, want, "content must include %q", want)
			}
		})
	}
}
