package schema_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

// slotBaseSchema has a resource an MCPServer fragment would contribute, with a
// permission the author declared themselves.
const slotBaseSchema = `
definition user {}

definition agentsession {
    relation owner: user
    relation participant: user
    relation denied: user
    permission interact = owner + participant - denied
}

definition crm_company {
    relation owner: user
    permission contact_access = owner
}
`

func companySlot() schema.SlotPair {
	return schema.SlotPair{ResourceType: "crm_company", Permission: "contact_access"}
}

// A slot puts the grant tuple on the RESOURCE pointing at the session, which is
// the reverse of the session-grant mechanism it replaces.
//
// That reversal is the whole point. With the link the other way round the
// composer had to emit `check_X = grant_X->X`, which evaluates permission X on
// the resource FOR THE ORIGINAL USER — a user who is in that flow precisely
// because they lack X. The only way to make it pass was a wildcard leaf, which
// is why the shipped hubspot schema carries `any_user`. Pointing the tuple at
// the session instead resolves the SESSION's member set, so the check is
// per-requester and honours denied for free.
func TestComposeSlots_emitsRelationAndPermissionArm(t *testing.T) {
	out, changed, _, err := schema.ComposeSlots(slotBaseSchema, []schema.SlotPair{companySlot()})
	require.NoError(t, err)
	assert.True(t, changed, "a slot the schema does not yet carry must change it")

	assert.Contains(t, out, "relation "+schema.SlotGrantRelationName("contact_access")+": agentsession",
		"the grant relation lives on the resource, pointing at the session")
	assert.Contains(t, out, "permission contact_access = "+schema.SlotGrantRelationName("contact_access")+"->interact + owner",
		"the composer owns the permission line: session members, plus anyone with direct standing")
}

// Byte-identical re-compose must be a no-op. The composed schema is written to
// SpiceDB on reconcile, so a composer that re-emitted a slightly different form
// each pass would rewrite the schema forever.
func TestComposeSlots_isIdempotent(t *testing.T) {
	once, changed, _, err := schema.ComposeSlots(slotBaseSchema, []schema.SlotPair{companySlot()})
	require.NoError(t, err)
	require.True(t, changed)

	twice, changedAgain, _, err := schema.ComposeSlots(once, []schema.SlotPair{companySlot()})
	require.NoError(t, err)
	assert.False(t, changedAgain, "re-composing an already-composed schema must report no change")
	assert.Equal(t, once, twice, "and must not alter a byte of it")
}

// The composer OWNS the permission line, so an author-declared expression for a
// slot permission is replaced rather than unioned with.
//
// This is the cost of the chosen split — an author can no longer freely write
// that one permission — and it is deliberate: the expression decides who may
// reach the resource through the agent, so leaving it author-editable would put
// a security-critical line in fragment-author hands and make the arm easy to
// forget entirely.
func TestComposeSlots_replacesAnAuthorDeclaredPermission(t *testing.T) {
	authored := strings.Replace(slotBaseSchema,
		"permission contact_access = owner",
		"permission contact_access = any_user + owner", 1)

	out, _, _, err := schema.ComposeSlots(authored, []schema.SlotPair{companySlot()})
	require.NoError(t, err)

	assert.NotContains(t, out, "any_user",
		"the author's wildcard leaf must not survive: it is what the reversal exists to remove")
	assert.Contains(t, out, "permission contact_access = "+schema.SlotGrantRelationName("contact_access")+"->interact + owner")
}

// THE regression this design exists to prevent. A wildcard leaf anywhere in a
// slot resource's permission means any user passes regardless of the session's
// member set, which silently un-does the per-requester property.
func TestComposeSlots_noWildcardLeafSurvives(t *testing.T) {
	for _, wildcard := range []string{"any_user", "user:*"} {
		t.Run(wildcard+" is removed", func(t *testing.T) {
			authored := strings.Replace(slotBaseSchema,
				"permission contact_access = owner",
				"permission contact_access = "+wildcard+" + owner", 1)

			out, _, _, err := schema.ComposeSlots(authored, []schema.SlotPair{companySlot()})
			require.NoError(t, err)
			assert.NotContains(t, out, wildcard)
		})
	}
}

// A slot naming a definition the schema does not declare is SKIPPED, not an
// error. Fragments are cluster-sourced and compose is all-or-nothing: failing
// the whole write because one AgentClass named a type whose MCPServer was
// deleted would wedge schema composition for every other agent.
func TestComposeSlots_skipsAnUndeclaredResourceType(t *testing.T) {
	out, changed, _, err := schema.ComposeSlots(slotBaseSchema,
		[]schema.SlotPair{{ResourceType: "not_declared_anywhere", Permission: "read"}})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, slotBaseSchema, out)
}

// agentsession is reserved: a slot must never rewrite it, whatever it declares.
func TestComposeSlots_refusesToTouchAgentsession(t *testing.T) {
	out, changed, _, err := schema.ComposeSlots(slotBaseSchema,
		[]schema.SlotPair{{ResourceType: "agentsession", Permission: "interact"}})
	require.NoError(t, err)
	assert.False(t, changed, "agentsession is reserved and must be left alone")
	assert.Equal(t, slotBaseSchema, out)
}

// This case used to assert the opposite, and the reversal is the point.
//
// While the session-grant mechanism was live, composing over a resource whose
// permission carried a `grant_*` leg would have DELETED the leg that flow wrote
// and checked — declaring a slot would silently disarm an agent's existing
// approvals. A migration guard skipped those resources and reported them.
//
// Nothing writes a grant_* tuple any more, so such a leg is a dead relation and
// the guard protected nothing. What is left is the property that outlives the
// migration: the composer OWNS this line, so whatever leaf the fragment wrote —
// a dead grant leg, a wildcard, both — is replaced rather than unioned with.
// Composition can only tighten who reaches the resource through an agent.
func TestComposeSlots_composesOverADeadLegacyLeg(t *testing.T) {
	legacy := strings.Replace(slotBaseSchema,
		"permission contact_access = owner",
		"permission contact_access = any_user + grant_contact_access + owner", 1)

	out, changed, skipped, err := schema.ComposeSlots(legacy, []schema.SlotPair{companySlot()})
	require.NoError(t, err)

	assert.True(t, changed, "a dead grant_* leg no longer blocks composition")
	assert.Empty(t, skipped, "and the slot is live, not reported inert")
	assert.Contains(t, out, "permission contact_access = "+
		schema.SlotGrantRelationName("contact_access")+"->interact + owner")
	// Matched whole, not by substring: the composed relation name
	// `slot_grant_contact_access` CONTAINS the legacy `grant_contact_access`.
	assert.NotContains(t, out, "any_user + grant_contact_access + owner",
		"the legacy expression is replaced wholesale, not unioned with")
	assert.NotContains(t, out, "any_user", "so the wildcard leaf goes with it")
}

// The companion: a fragment that never carried a legacy leg composes the same
// way. Same destination, whichever shape the author started from.
func TestComposeSlots_takesOverOnceTheLegacyLegIsRemoved(t *testing.T) {
	migrated := strings.Replace(slotBaseSchema,
		"permission contact_access = owner",
		"permission contact_access = any_user + owner", 1)

	out, changed, skipped, err := schema.ComposeSlots(migrated, []schema.SlotPair{companySlot()})
	require.NoError(t, err)

	assert.True(t, changed)
	assert.Empty(t, skipped)
	assert.Contains(t, out, "permission contact_access = "+schema.SlotGrantRelationName("contact_access")+"->interact + owner")
	assert.NotContains(t, out, "any_user", "and the wildcard goes with it")
}

// TestComposeSlots_OnePermissionsGrantCannotSatisfyAnother is the property the
// whole instance axis rests on once a grant is no longer bound to an exact
// call.
//
// Dropping the args-hash binding is only safe if a slot for one permission
// cannot be used to exercise a DIFFERENT permission on the same instance. The
// concrete case: a github_repo slot granting `read` must not let the agent
// `write` — same tool (the git CLI), same resource id, different args selecting
// a different permission.
//
// A single shared relation name breaks this outright: `permission read =
// slot_grant->...` and `permission write = slot_grant->...` are satisfied by the
// SAME tuple, so a read grant is a write grant. The composer is global, so the
// two permissions need not even come from the same AgentClass.
func TestComposeSlots_OnePermissionsGrantCannotSatisfyAnother(t *testing.T) {
	src := "definition github_repo {\n    relation owner: user\n}\n"
	out, _, _, err := schema.ComposeSlots(src, []schema.SlotPair{
		{ResourceType: "github_repo", Permission: "read"},
		{ResourceType: "github_repo", Permission: "write"},
	})
	require.NoError(t, err)

	readRel := schema.SlotGrantRelationName("read")
	writeRel := schema.SlotGrantRelationName("write")
	require.NotEqual(t, readRel, writeRel, "the relation must distinguish the permission")

	assert.Contains(t, out, "permission read = "+readRel+"->interact + owner")
	assert.Contains(t, out, "permission write = "+writeRel+"->interact + owner")
	assert.NotContains(t, out, "permission write = "+readRel+"->",
		"a read grant must not satisfy write")
	assert.NotContains(t, out, "permission read = "+writeRel+"->",
		"a write grant must not satisfy read")
}

// A definition that declares no `owner` relation or permission — a
// session-only resource, whose Standing forbids naming an approver permission
// at all (SpiceDBResource.ValidateStanding) — must not get an owner leg it
// cannot resolve. See slotPermissionExpr's doc for why dropping it narrows
// rather than widens who passes.
func TestComposeSlots_omitsOwnerLegWhenNotDeclared(t *testing.T) {
	src := "definition user {}\n\ndefinition widget {\n    relation viewer: user\n    permission read = viewer\n}\n"

	out, changed, skipped, err := schema.ComposeSlots(src, []schema.SlotPair{
		{ResourceType: "widget", Permission: "read"},
	})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, skipped)

	assert.Contains(t, out, "permission read = "+schema.SlotGrantRelationName("read")+"->interact")
	assert.NotContains(t, out, "owner", "widget declares no owner relation; the leg must not be emitted")
}

// The counterpart: a definition that DOES declare owner keeps getting the
// leg. Without this, the fix above could degenerate into never emitting the
// leg at all, which would silently remove the no-approval-needed path for
// every resource that has direct standing.
func TestComposeSlots_keepsOwnerLegWhenDeclared(t *testing.T) {
	src := "definition user {}\n\ndefinition widget {\n    relation owner: user\n    relation viewer: user\n    permission read = viewer\n}\n"

	out, changed, skipped, err := schema.ComposeSlots(src, []schema.SlotPair{
		{ResourceType: "widget", Permission: "read"},
	})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, skipped)

	assert.Contains(t, out, "permission read = "+schema.SlotGrantRelationName("read")+"->interact + owner")
}

// The composer must produce PARSEABLE schema when it creates a permission line
// that the fragment did not already declare. Every existing fixture declares
// its permission, so the append path was never exercised — and it emitted the
// line AFTER the definition's closing brace.
func TestComposeSlots_CreatedPermissionLandsInsideTheDefinition(t *testing.T) {
	src := "definition http_target {\n    relation owner: user\n}\n"
	out, _, _, err := schema.ComposeSlots(src, []schema.SlotPair{
		{ResourceType: "http_target", Permission: "reachable"},
	})
	require.NoError(t, err)

	body := out[strings.Index(out, "definition http_target {"):]
	body = body[:strings.Index(body, "\n}")+2]
	assert.Contains(t, body, "permission reachable = ",
		"a permission the composer creates must be inside the definition block, not after its closing brace")
}

// Pointing a slot at the audience permission would have ComposeSlots replace
// the audience definition with the slot expression — silently changing who can
// read every entry in the pool. Refuse it by name, with an error that says why.
func TestComposeSlots_RefusesASlotOnTheAudiencePermission(t *testing.T) {
	src := `definition user {}

definition customer {
	relation viewer: user
	permission view = viewer
	permission view_memory = view
}
`
	_, _, _, err := schema.ComposeSlots(src,
		[]schema.SlotPair{{ResourceType: "customer", Permission: "view_memory"}})
	require.Error(t, err, "a slot on view_memory must be refused, not composed")
	assert.Contains(t, err.Error(), "view_memory")
	assert.Contains(t, err.Error(), "write_",
		"the error must name the rule, so an author knows what to rename it to")
}

// The write side is slottable and must stay so.
func TestComposeSlots_AllowsASlotOnTheWritePermission(t *testing.T) {
	src := `definition user {}

definition customer {
	relation owner: user
	permission write_memory = owner
}
`
	out, changed, skipped, err := schema.ComposeSlots(src,
		[]schema.SlotPair{{ResourceType: "customer", Permission: "write_memory"}})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, skipped)
	assert.Contains(t, out, schema.SlotGrantRelationName("write_memory"))
}

// The rule is scoped to memory permissions. Slots on ordinary permissions ship
// today — github_repo and github_repo_url both slot on `read` — and must be
// untouched by this guard.
func TestComposeSlots_LeavesOrdinaryPermissionSlotsAlone(t *testing.T) {
	src := `definition user {}

definition gadget {
	relation admin: user
	permission read = admin
}
`
	_, changed, _, err := schema.ComposeSlots(src,
		[]schema.SlotPair{{ResourceType: "gadget", Permission: "read"}})
	require.NoError(t, err, "a slot on an ordinary permission is unaffected by the memory rule")
	assert.True(t, changed)
}
