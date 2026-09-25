package pipeline

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// canonicalID opts into AllowSynthetic on purpose (see its doc comment): an
// inbound sender with no verified email — a Slack guest, a Connect user from
// another workspace, every local/browser sender — must still get a usable
// subject rather than have their turn refused. These pin the two properties
// that make that safe, so a later "fail closed on a missing email" change has
// to confront them:
//
//  1. the email-less subject is TEAM-SCOPED, so one external id reused in two
//     workspaces never collapses into one subject that inherits the other's
//     grants; and
//  2. a verified email wins the canonical regardless of workspace, so the same
//     human reached through two workspaces stays ONE subject.
func TestCanonicalID_SyntheticSubjectIsTeamScopedAndEmailWins(t *testing.T) {
	guestTeamA := canonicalID(channelkinds.ExternalIdentity{Kind: "slack", TeamScope: "T0ALPHA", ExternalID: "U0SAME"})
	guestTeamB := canonicalID(channelkinds.ExternalIdentity{Kind: "slack", TeamScope: "T0BETA", ExternalID: "U0SAME"})
	require.NotEmpty(t, guestTeamA, "an email-less inbound sender must still canonicalize, not be refused")
	assert.NotEqual(t, guestTeamA, guestTeamB,
		"the same external id in two workspaces must be two subjects: a foreign-workspace user must not inherit grants written for a same-id member")

	assert.Equal(t, guestTeamA,
		canonicalID(channelkinds.ExternalIdentity{Kind: "slack", TeamScope: "T0ALPHA", ExternalID: "U0SAME"}),
		"the synthetic subject must be stable across calls — grants written for it have to resolve on the next turn")

	mailedTeamA := canonicalID(channelkinds.ExternalIdentity{Kind: "slack", TeamScope: "T0ALPHA", ExternalID: "U0ALICE", Email: "alice@example.com"})
	mailedTeamB := canonicalID(channelkinds.ExternalIdentity{Kind: "slack", TeamScope: "T0BETA", ExternalID: "U0OTHER", Email: "alice@example.com"})
	assert.Equal(t, mailedTeamA, mailedTeamB,
		"a verified email wins the canonical: one human reached through two workspaces is one subject")
	assert.NotEqual(t, guestTeamA, mailedTeamA,
		"the synthetic and email encodings must not collide")
}
