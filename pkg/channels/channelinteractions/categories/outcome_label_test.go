package categories

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// OutcomeLabel is the one place the "what do I show for a resolved
// interaction" question is answered, so the surfaces that ask it (Slack, the oap
// chat TUI, the web chat) cannot drift apart. The two rules easiest to lose in
// a per-surface copy are pinned below: identity_choice's raw action id must be
// swapped, and a tool approval with no OutcomeText must still say something.
func TestOutcomeLabel(t *testing.T) {
	cases := []struct {
		name        string
		category    string
		outcomeText string
		outcome     string
		want        string
	}{
		{
			name:     "tool_approval supplies no OutcomeText: the headline carries it",
			category: ToolApproval, outcome: channelevents.OutcomeApproved,
			want: "Approved",
		},
		{
			name:     "a denied tool approval reads as denied, not as the enum",
			category: ToolApproval, outcome: channelevents.OutcomeDenied,
			want: "Denied",
		},
		{
			name:     "a timeout has no decider and no OutcomeText",
			category: ToolApproval, outcome: channelevents.OutcomeExpired,
			want: "Expired",
		},
		{
			name:     "identity_choice: the raw action id swaps for the friendly label",
			category: IdentityChoice, outcomeText: "agent", outcome: channelevents.OutcomeApproved,
			want: "Running as the agent",
		},
		{
			name:     "identity_choice cancel",
			category: IdentityChoice, outcomeText: "cancel", outcome: channelevents.OutcomeDenied,
			want: "Cancelled",
		},
		{
			name:     "identity_choice with an unrecognized action id degrades to the raw text",
			category: IdentityChoice, outcomeText: "not-a-real-action", outcome: channelevents.OutcomeApproved,
			want: "not-a-real-action",
		},
		{
			name:     "another category's OutcomeText is NOT run through the identity_choice map",
			category: CredentialLink, outcomeText: "agent", outcome: channelevents.OutcomeResolved,
			want: "agent",
		},
		{
			name:     "credential_link carries a credential name in OutcomeText",
			category: CredentialLink, outcomeText: "GitHub", outcome: channelevents.OutcomeResolved,
			want: "GitHub",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, OutcomeLabel(tc.category, tc.outcomeText, tc.outcome))
		})
	}
}
