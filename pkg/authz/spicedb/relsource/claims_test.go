package relsource_test

import (
	"strings"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// The Slack relations the out-of-tree directory sync will maintain, reserved
// by slack.DirectorySyncSource. NOTHING in tree writes them today (verified
// by grep before this landed — every in-tree occurrence of slack_channel,
// slack_workspace, slack_usergroup and slack_user is a doc comment, a
// LookupSubjects read, or a schema-composition reference, never a
// relationship write), so claiming them protects real relations and breaks
// nothing.
//
// Importing the slack package here (rather than constructing a look-alike
// fixture) exercises the REAL registered claim, not a copy of it that could
// drift from what DirectorySyncSource actually declares.
func TestSyncClaims_AreOwnedAndRefuseEveryoneElse(t *testing.T) {
	require.NotEmpty(t, slack.DirectorySyncSource.Claims, "the sync source must carry its reserved claims")

	intruder := relsource.Source{Name: "claims_test_intruder"}

	for _, claim := range slack.DirectorySyncSource.Claims {
		resourceType, relation, ok := strings.Cut(claim, "#")
		require.True(t, ok, "claim %q must be type#relation", claim)

		update := touch(resourceType, "R1", relation, "user", "U1")

		err := relsource.CheckWrite(intruder, []*v1.RelationshipUpdate{update})
		require.Error(t, err, "claim %q must refuse a non-owning source", claim)
		assert.Contains(t, err.Error(), "claims_test_intruder")
		assert.Contains(t, err.Error(), claim)
		assert.Contains(t, err.Error(), slack.DirectorySyncSource.Name)

		assert.NoError(t, relsource.CheckWrite(slack.DirectorySyncSource, []*v1.RelationshipUpdate{update}),
			"the owning source itself must still be allowed to write %q", claim)
	}
}

// The MCP relationship writer resolves tuples a TOOL DECLARATION named, so it
// owns nothing at all. A tool declaring a write to a claimed relation is then
// refused by construction rather than by review.
func TestMCPRelWriter_OwnsNothingAndIsRefusedOnAClaimedRelation(t *testing.T) {
	assert.Empty(t, relwrites.Source.Claims, "relwrites owns nothing: a CEL-resolved write can name any relation a toolspec declares")

	// slack_channel#member is claimed by slack.DirectorySyncSource, imported
	// above by TestSyncClaims_AreOwnedAndRefuseEveryoneElse — relwrites must
	// be refused on it exactly like any other non-owning source.
	err := relsource.CheckWrite(relwrites.Source, []*v1.RelationshipUpdate{
		touch("slack_channel", "C1", "member", "slack_user", "U1"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "relwrites")
	assert.Contains(t, err.Error(), "slack_channel#member")
}

// Every registered source's claims are well-formed "definition#relation".
// Scoped to whatever this test binary has pulled in (the fixtures above plus
// this package's own test registrations) — not an exhaustive repo-wide scan,
// which would need every production writer package imported here.
func TestEveryClaimIsWellFormed(t *testing.T) {
	for _, src := range relsource.All() {
		for _, claim := range src.Claims {
			resourceType, relation, ok := strings.Cut(claim, "#")
			assert.True(t, ok, "source %q claim %q must be type#relation", src.Name, claim)
			assert.NotEmpty(t, resourceType, "source %q claim %q has an empty resource type", src.Name, claim)
			assert.NotEmpty(t, relation, "source %q claim %q has an empty relation", src.Name, claim)
		}
	}
}
