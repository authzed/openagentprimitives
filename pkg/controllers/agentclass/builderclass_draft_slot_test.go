// pkg/controllers/agentclass/builderclass_draft_slot_test.go
//
// The shipped builder's draft slot, resolved through the real reconciler
// helpers against the real bundle.
package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/builderbundle"
)

// TestBuilderClass_WorkshopDraftSlotResolvesFromTheSidecarAlone spans the JOIN
// no other test can see. The bundle's own tests pin the toolbox half (the four
// checks and the resource they name); this package's tests cover the resolver
// with hand-built fixtures. Neither asks the question that actually decides
// whether a build phase covers an apply: does the SHIPPED class declare a slot
// the SHIPPED toolbox's fragment can resolve, for exactly the permissions those
// checks key on?
//
// Both halves have to be right together, and either half alone looks fine. A
// class with no slot leaves the runner's SlotResourceTypes guard denying every
// call to a card (the state this change removed); a slot naming a permission no
// check keys is a ceiling nothing ever binds within.
//
// The class is resolved with NO MCPServers and NO toolkits on purpose: the
// builder has neither, so the sidecar's fragment is the only thing that can
// classify workshop_draft. Passing one would hide a regression in the sidecar
// path behind a fixture the real class does not have.
func TestBuilderClass_WorkshopDraftSlotResolvesFromTheSidecarAlone(t *testing.T) {
	class, toolbox := builderClassAndToolbox(t)

	got, reason, msg := resolveSlotValueKeying(class, nil, nil,
		[]spiceboxv1alpha1.SidecarToolbox{*toolbox}, nil)
	require.Empty(t, reason, "the shipped class must resolve its slots; msg=%s", msg)
	require.Len(t, got, 1, "the builder declares exactly one slot: the draft it is building")

	assert.Equal(t, "workshop_draft", got[0].ResourceType)
	assert.Equal(t, "change", got[0].Permission, "the slot's primary permission is the one an apply needs")
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, got[0].Standing,
		"no local permission governs who may approve a change to a draft nobody has built yet; the session's approvers decide")
	assert.Empty(t, got[0].ApproverPermission,
		"session-only consults no local permission, so publishing one would name a pool nothing routes to")
	assert.Empty(t, got[0].ValueTransforms,
		"the draft's id is a CONSTANT, not minted from a value: there is no value→id mapping for a grant writer to reproduce, "+
			"and a published chain here would be a mapping a seeder would faithfully apply to nothing")

	// The ceiling reaches the schema composer. A slot_grant_<permission>
	// relation is emitted per pair, and a grant can only be written for a
	// relation the schema carries — so a permission missing here is an approval
	// that fails at SpiceDB and buys nothing at all.
	assert.Equal(t, []spiceboxv1alpha1.GrantPair{
		{ResourceType: "workshop_draft", Permission: "change"},
		{ResourceType: "workshop_draft", Permission: "project"},
		{ResourceType: "workshop_draft", Permission: "remove"},
		{ResourceType: "workshop_draft", Permission: "test"},
	}, extractSlotPairs(class),
		"every permission the slot declares must reach the composer, dedup'd and sorted")

	// The instance side of the join: each declared permission is keyed by a
	// check naming the CONSTANT id `draft`. A constant is what lets a plan
	// phase bind the instance at all — a phase names what it will touch before
	// any call exists, so an id derived from a call's own arguments could never
	// be one of them. It reaches an approval through
	// permsurface.Descriptor.ConstantResourceID, which resolves the template
	// with no args; it is deliberately absent from ResolvedSlot, whose empty
	// ValueTransforms above says the same thing from the other side.
	ids := map[string]string{}
	for _, tl := range toolbox.Spec.Tools {
		for _, chk := range checksFrom(tl.Permission, tl.PermissionVariants) {
			if chk.ResourceType != "workshop_draft" {
				continue
			}
			assert.Empty(t, chk.ResourceIDExpr,
				"tool %q: an expr reads the call's arguments, so the instance could not be named before the call", tl.Name)
			ids[chk.Permission] = chk.ResourceIDTemplate
		}
	}
	assert.Equal(t, map[string]string{
		"change": "draft", "remove": "draft", "test": "draft", "project": "draft",
	}, ids, "the four permissions the slot declares are exactly the four the toolbox keys on the constant draft")

	// The phrase the card shows in place of the wire handle. The toolbox is the
	// ONLY declarer of workshop_draft, so if the displays walk skipped sidecar
	// fragments this row would not exist and the person approving a build phase
	// would be shown `workshop_draft:draft`.
	assert.Contains(t, resolveResourceDisplays(nil, nil, []spiceboxv1alpha1.SidecarToolbox{*toolbox}),
		spiceboxv1alpha1.ResolvedResourceDisplay{
			ResourceType: "workshop_draft", Name: "the draft in this workshop",
		}, "the draft's declared display must reach status, or the card renders the wire handle")

	// The per-PERMISSION half of the same publication, and the walk's only
	// input for this type is again the sidecar. A resource display names the
	// instance ("the draft in this workshop"); a permission title names the
	// ACTION being approved, and a planning note is what the builder reads
	// while declaring the phase that will make it. Four rows, because the slot
	// declares four permissions and a phase can name any of them.
	titles := resolvePermissionTitles(nil, nil, []spiceboxv1alpha1.SidecarToolbox{*toolbox})
	perms := map[string]spiceboxv1alpha1.ResolvedPermissionTitle{}
	for _, row := range titles {
		if row.ResourceType == "workshop_draft" {
			perms[row.Permission] = row
		}
	}
	require.Len(t, perms, 4, "each permission the slot declares must publish its own prose, got %+v", titles)
	for _, permission := range []string{"change", "remove", "test", "project"} {
		row, ok := perms[permission]
		require.True(t, ok, "no published prose for %q", permission)
		assert.NotEmpty(t, row.Title, "%q must carry the phrase a card shows in place of its handle", permission)
		assert.NotEmpty(t, row.PlanningNote, "%q must carry the line the builder reads while planning", permission)
	}
	assert.Equal(t, "Change this workshop's draft (any of its definitions)", perms["change"].Title,
		"the authored phrase reaches status verbatim; the composer owns the EXPR of a slotted permission, never its prose")
	assert.Contains(t, perms["change"].PlanningNote, "workshop_draft slot",
		"the note must tell the planner to declare the slot beside the handle — the handle alone is refused")
}

// TestBuilderClass_DraftSlotLeavesMembershipToTheInstaller pins the one field
// the manifest deliberately omits. `oap install` completes every slot's
// membership to the CRD's own default before applying
// (completeAuthzSlotMembershipDefaults, pkg/platform/oap/install/apply.go),
// because spec.authz.slots is +listType=atomic: SSA replaces the whole list, so
// an applied slot that omits membership differs from the live one the apiserver
// has since defaulted and every re-apply records a change — a byte-identical
// re-apply that is not a no-op. Writing the value into the manifest too would
// be a second copy of a default that already has two owners.
func TestBuilderClass_DraftSlotLeavesMembershipToTheInstaller(t *testing.T) {
	class, _ := builderClassAndToolbox(t)
	slots := class.Spec.GetSlots()
	require.Len(t, slots, 1)
	assert.Empty(t, slots[0].Membership,
		"the manifest leaves membership to the installer's completion, which fills the CRD default %q",
		spiceboxv1alpha1.AuthzSlotMembershipDefault)

	// The one field beside it that IS the manifest's to write. A misspelled key
	// in the YAML decodes to "" and nothing local notices — the apiserver would,
	// at install, which is the worst place to learn it. The description is what
	// an operator reading `kubectl get agentclass -o yaml` is told the slot is
	// for, so an empty one is a silently worse manifest, not a cosmetic one.
	assert.NotEmpty(t, slots[0].Description,
		"the slot must carry the description the manifest declares for it")
}

// builderClassAndToolbox decodes the shipped bundle's AgentClass and
// SidecarToolbox. Typed rather than unstructured: these tests feed them to the
// reconciler's own helpers, which take the typed CRs.
func builderClassAndToolbox(t *testing.T) (*spiceboxv1alpha1.AgentClass, *spiceboxv1alpha1.SidecarToolbox) {
	t.Helper()
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	crs, err := b.CRs()
	require.NoError(t, err)

	var class *spiceboxv1alpha1.AgentClass
	var toolbox *spiceboxv1alpha1.SidecarToolbox
	for _, cr := range crs {
		switch cr.GetKind() {
		case "AgentClass":
			require.Nil(t, class, "exactly one AgentClass in the builder bundle")
			var typed spiceboxv1alpha1.AgentClass
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(cr.Object, &typed),
				"the bundle's AgentClass must decode")
			class = &typed
		case "SidecarToolbox":
			require.Nil(t, toolbox, "exactly one SidecarToolbox in the builder bundle")
			var typed spiceboxv1alpha1.SidecarToolbox
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(cr.Object, &typed),
				"the bundle's SidecarToolbox must decode")
			toolbox = &typed
		}
	}
	require.NotNil(t, class, "the builder bundle must ship an AgentClass")
	require.NotNil(t, toolbox, "the builder bundle must ship the workshop SidecarToolbox")
	return class, toolbox
}
