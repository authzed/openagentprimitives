package schema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// The browser builtin fragment puts `definition web_host` into the composed
// schema, and ComposeSlots injects the same slot_grant/permission shape onto it
// that git_repo's session-only slot gets. This is the schema half of the C1
// substrate: without the definition in the base, ComposeSlots would SKIP the
// slot (definitionBlockBounds finds nothing) and no slot_grant_act relation
// would exist for a grant to write.
func TestBrowserFragment_ComposesWebHostSessionOnlySlot(t *testing.T) {
	base, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{browserschema.Fragment()})
	require.NoError(t, err, "the browser builtin fragment must compose onto the base scaffold")

	// Present before any slot is composed — inert, but declared, which is what
	// lets a slot compose onto it rather than be skipped.
	require.Contains(t, base, "definition web_host",
		"the browser builtin fragment must contribute definition web_host to the base")

	out, changed, skipped, err := schema.ComposeSlots(base,
		[]schema.SlotPair{{ResourceType: browserschema.WebHostType, Permission: browserschema.ActPermission}})
	require.NoError(t, err)
	require.True(t, changed, "composing a web_host#act slot must change the schema")
	require.Empty(t, skipped,
		"web_host IS declared by the browser fragment, so the slot is composed, not skipped — "+
			"a skipped web_host would mean it landed in the reserved scaffold instead")

	block := definitionBlock(t, out, "web_host")

	// slot.SlotGrantRelationName("act") == "slot_grant_act" must resolve to a
	// real relation with the mandatory expiration caveat.
	assert.Equal(t, "slot_grant_act", schema.SlotGrantRelationName(browserschema.ActPermission))
	assert.Contains(t, block, "relation slot_grant_act: agentsession with expiration",
		"the grant relation a slot write targets must exist with expiration")

	// The composer OWNS the act permission line: interact (the session member
	// set) + the inert owner leg — byte-identical in shape to git_repo push.
	assert.Contains(t, block, "permission act = slot_grant_act->interact + owner",
		"act must resolve through the slot grant plus the inert owner leg")
}

// The builtin fragment is session-only standing, so resolveStandingFor returns
// session-only and never requires an approverPermission. Pinned here against
// the SAME Resource() the guardian and agentclass controllers read.
func TestBrowserFragment_IsSessionOnly(t *testing.T) {
	r := browserschema.Resource()
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, r.Standing)
	require.NoError(t, r.ValidateStanding())
	// And the relation name the writer and schema derive from one definition.
	assert.Equal(t, "slot_grant_act", authz.SlotGrantRelationName(browserschema.ActPermission))
}
