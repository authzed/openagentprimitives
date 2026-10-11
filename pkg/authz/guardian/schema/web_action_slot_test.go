package schema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// nonCaveatedSlotResource is a session-only resource standing in for any
// ORDINARY slot type (git_repo, crm_company, …): an inert owner leg plus a
// single permission. The point of the test below is that its slot grant stays
// `with expiration` while web_action's gains the pagestate_matches caveat, so it
// must NOT be named web_action.
func nonCaveatedSlotResource() spiceboxv1alpha1.SpiceDBResource {
	return spiceboxv1alpha1.SpiceDBResource{
		Name:     "crm_company",
		Standing: spiceboxv1alpha1.StandingSessionOnly,
		Relations: []spiceboxv1alpha1.SpiceDBRelation{
			{Name: "owner", SubjectType: "user"},
		},
		Permissions: []spiceboxv1alpha1.SpiceDBPermission{
			{Name: "read", Expr: "owner", Title: "Read the company"},
		},
	}
}

// THE key constraint of task E1: the pagestate_matches caveat is scoped to
// web_action's slot grant ALONE. A class that declares BOTH a web_action slot
// and an ordinary slot must get the caveat on web_action#act's grant relation
// and NOT on the other type's grant relation.
//
// If this fails with the caveat on both, slotGrantRelationDecl stopped keying on
// the resource type and is caveating every slot — which would bind, say, a
// git_repo push grant to a browser page fingerprint it has no concept of, and
// freeze it the moment the (absent) fingerprint fails the `presented != ""`
// guard.
func TestComposeSlots_CaveatsWebActionGrantOnlyNotOtherSlots(t *testing.T) {
	base, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{
		browserschema.WebActionFragment(),
		{Resources: []spiceboxv1alpha1.SpiceDBResource{nonCaveatedSlotResource()}},
	})
	require.NoError(t, err, "the web_action builtin fragment + an ordinary slot resource must compose onto the base scaffold")
	require.Contains(t, base, "definition web_action", "the browser fragment must contribute definition web_action")
	require.Contains(t, base, "definition crm_company", "the ordinary slot resource must be present")

	out, changed, skipped, err := schema.ComposeSlots(base, []schema.SlotPair{
		{ResourceType: browserschema.WebActionType, Permission: browserschema.ActPermission},
		{ResourceType: "crm_company", Permission: "read"},
	})
	require.NoError(t, err)
	require.True(t, changed, "composing a web_action slot and a crm_company slot must change the schema")
	require.Empty(t, skipped, "both types are declared, so neither slot is skipped")

	// web_action's grant relation carries pagestate_matches — the TOCTOU
	// revalidation caveat — in ADDITION to the mandatory expiration.
	webActionBlock := definitionBlock(t, out, "web_action")
	assert.Contains(t, webActionBlock, "relation slot_grant_act: agentsession with pagestate_matches and expiration",
		"web_action's slot grant must be revalidated by pagestate_matches")
	assert.Contains(t, webActionBlock, "permission act = slot_grant_act->interact + owner",
		"the composer still owns the act permission line, caveat or not")

	// The ordinary slot's grant relation stays `with expiration` — NO caveat.
	// This is the scoping: the caveat is per resource type, not per permission.
	crmBlock := definitionBlock(t, out, "crm_company")
	assert.Contains(t, crmBlock, "relation slot_grant_read: agentsession with expiration",
		"an ordinary slot grant keeps the mandatory expiration and nothing more")
	assert.NotContains(t, crmBlock, "pagestate_matches",
		"pagestate_matches must NOT leak onto a non-web_action slot grant")

	// The composed text must be a schema a live SpiceDB would accept: the
	// pagestate_matches reference must resolve to a defined caveat. This is the
	// in-process proxy for the WriteSchema integration test.
	require.NoError(t, schema.ValidateComposedSchema(out),
		"the composed schema with the pagestate_matches caveat reference must type-check")
}

// web_host and web_action BOTH slot on `act`, so their grant relations share the
// name slot_grant_act — but only web_action's is caveated. A class that somehow
// carried both must get pagestate_matches on web_action#act and a bare
// `with expiration` on web_host#act: proof the scoping keys on the resource
// TYPE, not on the permission (which would caveat both or neither).
func TestComposeSlots_WebHostActStaysUncaveatedWhileWebActionIsCaveated(t *testing.T) {
	base, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{
		browserschema.Fragment(),          // web_host
		browserschema.WebActionFragment(), // web_action
	})
	require.NoError(t, err)

	out, changed, skipped, err := schema.ComposeSlots(base, []schema.SlotPair{
		{ResourceType: browserschema.WebHostType, Permission: browserschema.ActPermission},
		{ResourceType: browserschema.WebActionType, Permission: browserschema.ActPermission},
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Empty(t, skipped)

	hostBlock := definitionBlock(t, out, "web_host")
	assert.Contains(t, hostBlock, "relation slot_grant_act: agentsession with expiration",
		"web_host#act is NOT revalidated by page state — it is the host-level standing slot (C1)")
	assert.NotContains(t, hostBlock, "pagestate_matches",
		"the TOCTOU caveat belongs to web_action, not web_host")

	actionBlock := definitionBlock(t, out, "web_action")
	assert.Contains(t, actionBlock, "relation slot_grant_act: agentsession with pagestate_matches and expiration",
		"web_action#act IS revalidated by the page fingerprint")

	require.NoError(t, schema.ValidateComposedSchema(out))
}
