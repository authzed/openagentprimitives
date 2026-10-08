// Package slotspec is the ONLY place an authz.BoundEntitySpec is constructed
// from an AgentClass's declared slots.
//
// It exists because the conversion had grown three hand-maintained copies — one
// in the runner loop, one in the runner binary, one in authzd — and a field
// added to BoundEntitySpec had to be remembered at all three. That is not a
// theoretical hazard here: `requires` is the slot's PRECONDITION set, and a
// builder that omits it hands pkg/authz a slot with no gate. The failure is
// silent in the worst direction — the slot binds, the permissioned call
// succeeds, and nothing goes red — so the shape that produced it is removed
// rather than documented. A guard test (guard_test.go) refuses a second
// construction site.
//
// The package sits under pkg/authz/ rather than in pkg/authz itself because it
// must import pkg/apis/v1alpha1, and v1alpha1 imports pkg/authz (for Permission
// / PermissionVariant). A SUBpackage has no such cycle: nothing in v1alpha1
// imports this.
package slotspec

import (
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
)

// FromSlots adapts an AgentClass's declared slots to the []authz.BoundEntitySpec
// shape pkg/authz consumes.
//
// `transforms` is the per-resource-type value-transform chain from
// AgentClass.status.resolvedSlots, which only a caller holding the class's
// STATUS can supply. A caller that holds only the spec-side slot list — authzd,
// whose specs feed the extractor prefilter and never reach a binder — passes
// nil, and every slot is then treated as not value-keyed, exactly as that call
// site did before this package existed.
//
// # Why it returns an error rather than dropping what it cannot convert
//
// The only failure is a `requires[]` predicate that will not compile. The
// AgentClass reconciler already refuses such a class at admission, so reaching
// here means the object predates that validation or was written past it. Both
// answers to that are fail-closed — bind nothing — but they are not equally
// visible: silently returning the remaining slots would hand a caller a spec
// list that looks complete, and the slot whose gate vanished would be the one
// nobody could account for. Returning the error makes the caller say so.
func FromSlots(slots []spiceboxv1alpha1.AuthzSlot, transforms map[string][]string) ([]authz.BoundEntitySpec, error) {
	if len(slots) == 0 {
		return nil, nil
	}
	out := make([]authz.BoundEntitySpec, 0, len(slots))
	for _, et := range slots {
		afArgs := make([]authz.AutoFillArgSpec, 0, len(et.AutoFillArgs))
		for _, af := range et.AutoFillArgs {
			afArgs = append(afArgs, authz.AutoFillArgSpec{
				ArgName:         af.ArgName,
				ToolNamePattern: af.ToolNamePattern,
			})
		}
		requires, err := compileRequires(et.ResourceType, et.Requires)
		if err != nil {
			return nil, err
		}
		out = append(out, authz.BoundEntitySpec{
			ResourceType:    et.ResourceType,
			Permission:      et.Permission,
			Defaults:        et.Defaults,
			FillFrom:        et.FillFrom,
			AutoFillArgs:    afArgs,
			ValueTransforms: transforms[et.ResourceType],
			Requires:        requires,
			// Occupancy and Rebind are plain spec fields on AuthzSlot (not
			// status-derived like ValueTransforms), so they are copied straight
			// off the slot. Empty reads as single downstream.
			Occupancy: et.Occupancy,
			Rebind:    et.Rebind,
		})
	}
	return out, nil
}

// FromClass is the shorthand for the two callers that hold a whole AgentClass:
// its declared slots, gated by the transform chains its status resolved.
//
// It takes the transforms as an argument rather than reading status itself
// because that derivation lives in pkg/agent/runner (SlotTransformsOf) and is
// pinned by its own test there; duplicating it here would recreate, one layer
// down, exactly the several-copies problem this package removes.
func FromClass(class *spiceboxv1alpha1.AgentClass, transforms map[string][]string) ([]authz.BoundEntitySpec, error) {
	if class == nil {
		return nil, nil
	}
	return FromSlots(class.Spec.GetSlots(), transforms)
}

// compileRequires compiles every predicate a slot declares, keeping each one's
// two authored messages attached to it.
//
// Compilation happens per conversion rather than once per class because a
// BoundEntitySpec is a plain value with no cache to hang a compiled program on,
// and the cost is paid only by a slot that actually declares a precondition:
// the loop body never runs for the overwhelmingly common empty case, so no
// existing class pays anything for this at all.
//
// The messages are copied here rather than looked up later from the AgentClass,
// because the only place that could do the lookup is dispatch — which holds a
// BoundEntitySpec and no class — and a lookup by index into a list that has
// since been re-read is exactly the drift precondition.Rule exists to make
// unrepresentable. A message that reaches the agent describing the wrong rule
// is worse than no message: it names a fix for a gate that is not the one
// holding the slot shut.
func compileRequires(resourceType string, requires []spiceboxv1alpha1.SlotPrecondition) ([]precondition.Rule, error) {
	if len(requires) == 0 {
		return nil, nil
	}
	out := make([]precondition.Rule, 0, len(requires))
	for i, p := range requires {
		c, err := precondition.Compile(p.CEL)
		if err != nil {
			return nil, fmt.Errorf("slot %q requires[%d]: %w", resourceType, i, err)
		}
		out = append(out, precondition.Rule{
			Compiled:         c,
			UndeterminedHint: p.UndeterminedHint,
			RefusalMessage:   p.RefusalMessage,
			Approvers:        p.Approvers,
		})
	}
	return out, nil
}
