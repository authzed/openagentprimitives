package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// SlotWithoutOwnerFragment is a genuinely legitimate MCPServer fragment: a
// session-only resource (SpiceDBResource.Standing — see its doc, "no local
// permission governs approval for this type") that declares a permission but
// never declares an `owner` relation or permission at all. Nothing about the
// fragment is malformed on its own — composeFragmentSet validates it in
// isolation and accepts it (see
// TestComposeAllWithSkipped_ConfirmsTheSurgeryOmitsTheOwnerLeg below).
//
// This used to be the shape that made ComposeSlots' composeOneSlot break:
// composeOneSlot unconditionally rewrote the named permission to
// `slot_grant_<perm>->interact + owner` (slots.go, slotPermissionExpr) with no
// check that the target definition declared `owner`, so a SlotPair naming
// this fragment's resource/permission produced schema SpiceDB's type system
// refused — `owner` resolved to nothing on `widget`. composeOneSlot now calls
// definitionDeclaresName before deciding whether to include that leg, so this
// fixture composes to valid schema with the leg omitted; see the test below.
//
// Exported (capital S) rather than package-private so composer_test.go —
// package schema_test, a different Go package sharing this test binary — can
// reuse the exact same fixture instead of duplicating it inline. Test files
// in package schema and package schema_test in the same directory link into
// one test binary, so an exported test-only helper here is visible there
// without becoming part of the production API.
func SlotWithoutOwnerFragment() IdentifiedFragment {
	return IdentifiedFragment{
		Key:  "mcpserver/default/widget",
		Tier: TierTenant,
		Fragment: &spiceboxv1alpha1.SpiceDBSchemaFragment{
			Resources: []spiceboxv1alpha1.SpiceDBResource{{
				Name:      "widget",
				Standing:  spiceboxv1alpha1.StandingSessionOnly,
				Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "viewer", SubjectType: "user"}},
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{
					{Name: "read", Expr: "viewer"},
				},
			}},
		},
	}
}

// WidgetReadSlot is the SlotPair naming SlotWithoutOwnerFragment's resource
// and permission — see that function's doc. Exported for the same
// cross-package-sharing reason.
func WidgetReadSlot() SlotPair {
	return SlotPair{ResourceType: "widget", Permission: "read"}
}

// SlotCollidesWithRelationFragment is a DIFFERENT genuinely-legitimate-on-its-
// own fragment, used by the two tests below to prove the post-surgery gate
// still refuses an invalid rewrite. `viewer` is declared as a RELATION; the
// slot below names a permission of the SAME name. composeOneSlot's insert
// path (slots.go: "if !hasPerm") only recognizes an EXISTING `permission
// viewer = …` line via permLine's regex — a relation of that name doesn't
// match it — so it inserts a SECOND declaration named `viewer` alongside the
// relation. SpiceDB's type system refuses a duplicate relation/permission
// name.
//
// This defect is untouched by the owner-leg fix: it fires regardless of
// whether the target declares `owner`, so unlike SlotWithoutOwnerFragment it
// stays genuinely invalid after composeOneSlot's definitionDeclaresName check
// and remains a reliable fixture for exercising the "refuse an invalid
// post-surgery write" gate.
func SlotCollidesWithRelationFragment() IdentifiedFragment {
	return IdentifiedFragment{
		Key:  "mcpserver/default/gadget",
		Tier: TierTenant,
		Fragment: &spiceboxv1alpha1.SpiceDBSchemaFragment{
			Resources: []spiceboxv1alpha1.SpiceDBResource{{
				Name:      "gadget",
				Standing:  spiceboxv1alpha1.StandingSessionOnly,
				Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "viewer", SubjectType: "user"}},
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{
					{Name: "read", Expr: "viewer"},
				},
			}},
		},
	}
}

// GadgetViewerSlot names a permission equal to
// SlotCollidesWithRelationFragment's RELATION name (not its declared `read`
// permission) — see that function's doc for why the mismatch is the point.
func GadgetViewerSlot() SlotPair {
	return SlotPair{ResourceType: "gadget", Permission: "viewer"}
}

// THE hole this fixture pins, closed: SlotWithoutOwnerFragment/WidgetReadSlot
// used to be the shape that made ComposeSlots' rewrite produce schema
// SpiceDB's type system refused — `owner` resolving to nothing on `widget`,
// a resource a real MCPServer fragment could legitimately contribute
// (session-only standing means no `owner` relation is required or expected).
// composeOneSlot now calls definitionDeclaresName before deciding whether to
// include the `owner` leg (slots.go, slotPermissionExpr), so the identical
// fixture composes to schema that VALIDATES, with the leg omitted rather than
// dangling.
//
// Asserts three things: composeFragmentSet still accepts the fragment alone
// (unchanged by this fix), ComposeSlots still never errors on this input, and
// — the part that used to fail — the POST-SLOT text passes
// ValidateComposedSchema, carrying the grant-path leg the slot exists to
// create and none of the owner leg the target cannot resolve.
func TestComposeAllWithSkipped_ConfirmsTheSurgeryOmitsTheOwnerLeg(t *testing.T) {
	frag := SlotWithoutOwnerFragment()

	base, err := composeFragmentSet([]IdentifiedFragment{frag})
	require.NoError(t, err, "the fragment alone, before any slot runs, must be valid")
	require.NoError(t, ValidateComposedSchema(base), "and must pass the same validation the gate uses")

	afterSlot, _, _, err := ComposeSlots(base, []SlotPair{WidgetReadSlot()})
	require.NoError(t, err, "ComposeSlots itself never errors on this input")

	assert.NoError(t, ValidateComposedSchema(afterSlot),
		"a session-only resource with no owner relation must compose to VALID schema: "+
			"the owner leg is omitted, not left dangling")
	assert.Contains(t, afterSlot, "permission read = "+SlotGrantRelationName("read")+"->interact",
		"the grant-path leg the slot exists to create must still be there")
	assert.NotContains(t, afterSlot, "->interact + owner",
		"widget declares no owner; the leg must be omitted, not written dangling")
}

// Grant and slot composition rewrite the assembled text after
// composeFragmentSet has already validated it. If a rewrite produces
// something SpiceDB would refuse, the write must not happen: a refused write
// leaves the previous schema serving, whereas a bad write takes authorization
// down for the whole cluster.
//
// This used to run on SlotWithoutOwnerFragment/WidgetReadSlot, the same
// fixture TestComposeAllWithSkipped_ConfirmsTheSurgeryOmitsTheOwnerLeg now
// proves composes cleanly — so it moved to
// SlotCollidesWithRelationFragment/GadgetViewerSlot, a fixture the owner-leg
// fix does not touch (see that function's doc), to keep exercising this gate
// with something genuinely still invalid.
//
// The assertion is on the SPECIFIC failure, not merely "validate composed
// schema" (which validateCompiled emits at BOTH the assembly stage, inside
// composeFragmentSet, and this post-surgery stage): a fixture broken so it
// fails at assembly instead — never reaching the slot stage this test exists
// to exercise — would still satisfy a bare substring match on that phrase.
// "grant/slot composition produced invalid schema" is the wrapper ONLY the
// post-surgery gate adds (composeAllWithSkipped, right after ComposeSlots),
// and the duplicate-name pairing names the defect the slot's insert path
// specifically introduces.
func TestComposeAllWithSkipped_RefusesAnInvalidPostSurgeryResult(t *testing.T) {
	frags := []IdentifiedFragment{SlotCollidesWithRelationFragment()}
	slots := []SlotPair{GadgetViewerSlot()}

	_, _, err := composeAllWithSkipped(frags, nil, nil, slots)
	require.Error(t, err, "surgery producing an invalid schema must not be returned as success")
	assert.Contains(t, err.Error(), "grant/slot composition produced invalid schema",
		"must be the post-surgery gate, not merely any validateCompiled failure")
	assert.Contains(t, err.Error(), "duplicate relation/permission name `viewer` under definition `gadget`",
		"must name the specific defect the slot's insert path introduced")
}

// Companion to the test above: proves the fragment set is fine BEFORE the
// slot stage runs (composeFragmentSet alone accepts it), and that
// ValidateComposedSchema — the same function the gate calls — is what
// actually rejects the post-slot text. Without this, the test above could
// pass for the wrong reason (e.g. a typo elsewhere in the fixture) and nobody
// would know.
func TestComposeAllWithSkipped_ConfirmsTheSurgeryIntroducesADuplicateName(t *testing.T) {
	frag := SlotCollidesWithRelationFragment()

	base, err := composeFragmentSet([]IdentifiedFragment{frag})
	require.NoError(t, err, "the fragment alone, before any slot runs, must be valid")
	require.NoError(t, ValidateComposedSchema(base), "and must pass the same validation the gate uses")

	afterSlot, _, _, err := ComposeSlots(base, []SlotPair{GadgetViewerSlot()})
	require.NoError(t, err, "ComposeSlots itself never errors on this input; it just writes a duplicate name")

	verr := ValidateComposedSchema(afterSlot)
	require.Error(t, verr)
	assert.Contains(t, verr.Error(), "duplicate relation/permission name", "the defect the slot's insert path introduced must be nameable")
	assert.Contains(t, verr.Error(), "gadget")
}
