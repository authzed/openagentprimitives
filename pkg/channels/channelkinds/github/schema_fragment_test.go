package github_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
)

// github_user moved to the base scaffold (pkg/authz/spicedb/schema/schema.zed)
// because three independent fragments now name it — this kind's own session
// links below, the gh toolkit's repo roles, and the directory sync's writes —
// and only one may declare it; a second declaration is refused by the
// composer. The kind still satisfies SchemaContributor (SessionRelationLinks
// depends on github_user resolving, and the interface is where a future
// fragment contribution would go) but must not re-add the definition here.
func TestKind_FragmentNoLongerDeclaresGithubUser(t *testing.T) {
	k := &github.Kind{}

	sc, ok := any(k).(channelkinds.SchemaContributor)
	require.True(t, ok, "the github kind must satisfy SchemaContributor")

	frag := sc.SpiceDBSchemaFragment()
	require.NotNil(t, frag)
	assert.NotContains(t, frag.RawZed, "definition github_user",
		"github_user now lives in the base scaffold; redeclaring it here is refused by the composer")
}

// The github kind registers github_user#user as a session relation link, so the
// guardian composer unions it into agentsession's owner/participant/denied.
// That is what lets a pull-request-triggered session name its PR author as an
// owner by GitHub account — a subject-set that resolves to a platform user only
// through the attested identity edge (github_user#user@user:<canonical>) the
// useridentity reconciler mints from a VERIFIED credential. Grant/deny parity
// comes from the composer's sessionSubjectRelations set, not from anything here.
func TestKind_LinksGithubUserIntoSessionRelations(t *testing.T) {
	k := &github.Kind{}

	rl, ok := any(k).(channelkinds.SessionRelationLinker)
	require.True(t, ok, "the github kind must satisfy SessionRelationLinker")
	assert.Equal(t, []string{"github_user#user"}, rl.SessionRelationLinks(),
		"exactly the attested-identity binding, keyed the way the scaffold's github_user definition defines it")
}

// The fragment declares no schema of its own now — github_user moved to the
// scaffold — but must never grow a permission through either path if it ever
// does contribute one again: a linked credential conferring access on its own
// is the one thing this edge must never do.
func TestKind_FragmentGrantsNoPermission(t *testing.T) {
	frag := (&github.Kind{}).SpiceDBSchemaFragment()
	require.NotNil(t, frag)

	// Check RawZed path: no permission keyword in raw text.
	assert.NotContains(t, frag.RawZed, "permission ",
		"github_user binds identity only; authority comes from the directory sync or an approval")

	// Check Resources path: no permissions in structured definitions.
	// SpiceDBSchemaFragment offers two ways to declare schema: RawZed (raw text)
	// and Resources (structured). A future author could bypass the raw-text guard
	// by adding a permission through the structured path, so both must be guarded.
	for _, res := range frag.Resources {
		assert.Empty(t, res.Permissions,
			"github_user must not grant any permission through the structured path either")
	}
}
