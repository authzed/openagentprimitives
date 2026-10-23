package slotspec_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/slotspec"
)

func gatedSlot(cel string) spiceboxv1alpha1.AuthzSlot {
	return spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_commit",
		Permission:   "read",
		FillFrom:     []string{"observed"},
		Requires: []spiceboxv1alpha1.SlotPrecondition{{
			CEL:              cel,
			UndeterminedHint: "call gitlike_gh to observe the pull request first",
			RefusalMessage:   "the head of this pull request lives on a fork",
		}},
	}
}

// TestFromSlots_CarriesRequiresThrough is the field P-4 is about. A conversion
// that dropped it would leave the slot's gate never firing, in silence — so the
// assertion is on the COMPILED program reaching the spec, not merely on the
// call returning without error.
func TestFromSlots_CarriesRequiresThrough(t *testing.T) {
	const expr = `facts.observed.is_cross_repository == false`

	got, err := slotspec.FromSlots([]spiceboxv1alpha1.AuthzSlot{gatedSlot(expr)}, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Requires, 1, "the slot's precondition must reach the spec pkg/authz binds from")
	assert.Equal(t, expr, got[0].Requires[0].Compiled.Expression())
	assert.NotNil(t, got[0].Requires[0].Compiled.Program(), "a compiled precondition must carry a runnable program")
	// The two messages are the SAME P-4 hazard as the program: dispatch quotes
	// them back to the agent to say why the slot is empty, and a conversion that
	// carried the predicate but dropped the words would leave every held call
	// explained by the raw SpiceDB denial — silently, since the gate still
	// fires.
	assert.Equal(t, "call gitlike_gh to observe the pull request first", got[0].Requires[0].UndeterminedHint)
	assert.Equal(t, "the head of this pull request lives on a fork", got[0].Requires[0].RefusalMessage)
}

// TestFromSlots_CarriesOccupancyAndRebindThrough is the same silent-drop hazard
// as Requires: occupancy/rebind are plain spec fields on AuthzSlot, and a
// conversion that dropped them would hand pkg/authz a binding whose empty
// Occupancy reads as single — so a multi slot would be gated as single, its
// second instance refused, with nothing red. The assertion is on the spec the
// binder consumes, not merely on the call returning.
func TestFromSlots_CarriesOccupancyAndRebindThrough(t *testing.T) {
	slots := []spiceboxv1alpha1.AuthzSlot{
		{ResourceType: "label", Permission: "apply", Occupancy: "multi", Rebind: "approval"},
		{ResourceType: "git_repo", Permission: "push", Occupancy: "single", Rebind: "never"},
		{ResourceType: "crm_company", Permission: "contact_access"}, // both unset
	}

	got, err := slotspec.FromSlots(slots, nil)
	require.NoError(t, err)
	require.Len(t, got, 3)

	assert.Equal(t, "multi", got[0].Occupancy, "a multi slot's occupancy must reach the binder")
	assert.Equal(t, "approval", got[0].Rebind)
	assert.Equal(t, "single", got[1].Occupancy)
	assert.Equal(t, "never", got[1].Rebind, "rebind must reach the gate, which quotes it in the refusal route")
	assert.Empty(t, got[2].Occupancy, "unset stays empty — which pkg/authz reads as single downstream")
	assert.Empty(t, got[2].Rebind)
}

// TestFromSlots_CarriesApproversThrough is the same silent-drop hazard as the
// program and the two messages: compileRequires is the ONE seam that copies a
// SlotPrecondition into the precondition.Rule pkg/authz gates on, and a field
// added to the CRD but not copied here compiles and never reaches the gate. The
// assertion is on the Rule the spec carries, not merely on the call returning.
func TestFromSlots_CarriesApproversThrough(t *testing.T) {
	slot := spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_commit",
		Permission:   "read",
		FillFrom:     []string{"observed"},
		Requires: []spiceboxv1alpha1.SlotPrecondition{{
			CEL:              `facts.observed.is_cross_repository == false`,
			UndeterminedHint: "call gitlike_gh to observe the pull request first",
			RefusalMessage:   "the head of this pull request lives on a fork",
			Approvers:        []string{"a", "b"},
		}},
	}

	got, err := slotspec.FromSlots([]spiceboxv1alpha1.AuthzSlot{slot}, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Requires, 1)
	assert.Equal(t, []string{"a", "b"}, got[0].Requires[0].Approvers,
		"the precondition's named waiver approvers must reach the rule pkg/authz gates on")
}

// TestFromSlots_RefusesAPredicateThatWillNotCompile: the AgentClass reconciler
// already refuses such a class, so reaching here means the object was written
// past validation. Bind nothing and say why — a partial list would look
// complete while the slot whose gate vanished went unaccounted for.
func TestFromSlots_RefusesAPredicateThatWillNotCompile(t *testing.T) {
	_, err := slotspec.FromSlots([]spiceboxv1alpha1.AuthzSlot{gatedSlot(`facts.observed.x ==`)}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `slot "git_commit" requires[0]`,
		"the message must name the entry an author has to go fix")
}

// TestFromSlots_CarriesTheFieldsThatPredateRequires is the regression guard for
// every existing class: the conversion moved into a shared constructor, and a
// field lost in that move would be as silent as the one it was written to
// protect.
func TestFromSlots_CarriesTheFieldsThatPredateRequires(t *testing.T) {
	in := []spiceboxv1alpha1.AuthzSlot{{
		ResourceType: "github_repo",
		Permission:   "read",
		Defaults:     []string{"demo-org/demo-repo"},
		FillFrom:     []string{"default", "query"},
		AutoFillArgs: []spiceboxv1alpha1.AuthzSlotAutoFillArg{{ArgName: "repo", ToolNamePattern: "gitlike_*"}},
	}}
	transforms := map[string][]string{"github_repo": {"spicedb_escape"}}

	got, err := slotspec.FromSlots(in, transforms)
	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Equal(t, "github_repo", got[0].ResourceType)
	assert.Equal(t, "read", got[0].Permission)
	assert.Equal(t, []string{"demo-org/demo-repo"}, got[0].Defaults)
	assert.Equal(t, []string{"default", "query"}, got[0].FillFrom)
	assert.Equal(t, []string{"spicedb_escape"}, got[0].ValueTransforms,
		"the transform chain is what makes the derived object id legal for SpiceDB")
	require.Len(t, got[0].AutoFillArgs, 1)
	assert.Equal(t, "repo", got[0].AutoFillArgs[0].ArgName)
	assert.Equal(t, "gitlike_*", got[0].AutoFillArgs[0].ToolNamePattern)
	assert.Empty(t, got[0].Requires, "a slot declaring no requires must carry none")
}

// A caller holding only the spec-side slot list (authzd) passes nil transforms;
// every slot is then not value-keyed, which is exactly what that call site did
// before this package existed.
func TestFromSlots_NilTransformsLeavesEverySlotUnkeyed(t *testing.T) {
	got, err := slotspec.FromSlots([]spiceboxv1alpha1.AuthzSlot{gatedSlot(`facts.observed.a == 1`)}, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].ValueTransforms)
}

func TestFromSlots_EmptyInputIsNoSpecs(t *testing.T) {
	got, err := slotspec.FromSlots(nil, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// FromClass is the shorthand the two class-holding callers use; a nil class is
// a session with no AgentClass loaded, not an error.
func TestFromClass_NilClassIsNoSpecs(t *testing.T) {
	got, err := slotspec.FromClass(nil, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFromClass_ReadsTheClassesDeclaredSlots(t *testing.T) {
	class := &spiceboxv1alpha1.AgentClass{}
	class.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{gatedSlot(`facts.envelope.head_is_fork == false`)},
	}

	got, err := slotspec.FromClass(class, map[string][]string{"git_commit": {"lowercase"}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"lowercase"}, got[0].ValueTransforms)
	require.Len(t, got[0].Requires, 1)
	assert.Equal(t, `facts.envelope.head_is_fork == false`, got[0].Requires[0].Compiled.Expression())
}
