package agentclass

import (
	"fmt"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// validatePreconditionApprovers refuses a class whose slot precondition would
// raise a waiver card no one could answer.
//
// When a precondition evaluates Refused, plan 3 raises a human waiver card and
// routes it to an approver pool. The field's contract is: an UNSET approvers[]
// defaults to the slot type's resolved standing (the session's own #approve set
// for session-only, or the SpiceDB subjects holding a permission on the instance
// for required); a SET approvers[] is AUTHORITATIVE and overrides that default.
// So the two structurally-empty pools admission can catch are:
//
//   - a SET approvers[] that names no real subject (an explicit list of blanks):
//     the author overrode the standing default with a route to no one; AND
//   - an UNSET approvers[] whose slot type resolves to no standing at all: there
//     is no default pool to fall back to.
//
// Either way a Refused verdict would raise a card no subject is authorized to
// answer, dead-ending the appealable gate as a "no one has standing to approve"
// crash at first dispatch. Refuse it here, at reconcile, where the message can
// name the requires[j] entry an author wrote.
//
// This proves only the STRUCTURAL empty pool. It cannot, and does not, claim
// that SpiceDB currently holds zero subjects for a declared `required` standing:
// that is a runtime fact about the datastore, not a shape, and refusing on it
// would ground a class whose approver pool is merely unpopulated today.
//
// # Placement and the unset/undeclared branch
//
// Runs after resolveResourceStandings so the standing default is available. The
// UNSET-plus-undeclared-standing branch below is, at THIS call site, already
// pre-empted: resolveSlotValueKeying refuses any slot whose type declares no
// standing (with ReasonSlotDeclarationInvalid) earlier in the same reconcile, so
// no unset precondition reaches here with an undeclared standing. It is kept
// anyway — the function stays correct if it is ever moved ahead of that check,
// and it fails closed rather than assuming an upstream guard it does not own.
// The SET-but-blank branch is the one this call site actually exercises, because
// it is orthogonal to standing: a set list overrides the standing default, so a
// blank one is unroutable even on a slot whose type IS classified.
//
// approvers[] is read from the spec's SlotPrecondition rather than the compiled
// slot, because the compiled form carries only the predicate program, not the
// author's approver routing.
func validatePreconditionApprovers(
	ac *spiceboxv1alpha1.AgentClass,
	standings []spiceboxv1alpha1.ResolvedResourceStanding,
) (reason, msg string) {
	slots := ac.Spec.GetSlots()
	if len(slots) == 0 {
		return "", ""
	}

	// Types with a resolved standing default an unset approvers[] to a pool that
	// has a shape. A type absent here has no standing to fall back on.
	declaredStanding := make(map[string]struct{}, len(standings))
	for _, st := range standings {
		declaredStanding[st.ResourceType] = struct{}{}
	}

	for _, s := range slots {
		for j, p := range s.Requires {
			ref := fmt.Sprintf("authz.slots[%s].requires[%d]", s.ResourceType, j)

			// A SET approvers[] is authoritative — it overrides the standing
			// default — so it must name at least one real subject. A set list of
			// only blanks routes the waiver to no one.
			//
			// An explicit empty list (approvers: []) is NOT distinguishable from
			// unset here: the CRD field is omitempty, so an empty list serializes
			// away and reaches the reconciler as a nil slice. Only a non-empty
			// list of blank entries survives, and it is that shape this branch
			// refuses; an explicit [] is treated as unset and defaults to the
			// standing below.
			if p.Approvers != nil {
				if namesAnApprover(p.Approvers) {
					continue
				}
				return spiceboxv1alpha1.ReasonSlotPreconditionUnroutable,
					fmt.Sprintf(
						"%s: this precondition sets an approvers list that names no subject, so a "+
							"Refused verdict would raise a waiver card no subject is authorized to answer. "+
							"Set requires[%d].approvers to a subject-set expression (e.g. "+
							"\"agentsession:{ns}/{name}#approve\"), or remove it to default to the slot's standing.",
						ref, j)
			}

			// UNSET approvers[]: default to the slot type's resolved standing.
			if _, ok := declaredStanding[s.ResourceType]; ok {
				continue
			}
			return spiceboxv1alpha1.ReasonSlotPreconditionUnroutable,
				fmt.Sprintf(
					"%s: this precondition names no approvers and resource type %q resolves to no "+
						"standing, so a Refused verdict would raise a waiver card no subject is authorized "+
						"to answer. Declare a standing for %q on a tool schema fragment (so its owner pool "+
						"defaults the waiver), or set requires[%d].approvers to the subject-set that should "+
						"answer the risk question.",
					ref, s.ResourceType, s.ResourceType, j)
		}
	}
	return "", ""
}

// namesAnApprover reports whether the list holds at least one non-blank
// subject-set expression. A nil list, an explicit empty list, and a list of
// only whitespace all report false: none routes a waiver to a real subject.
func namesAnApprover(approvers []string) bool {
	for _, a := range approvers {
		if strings.TrimSpace(a) != "" {
			return true
		}
	}
	return false
}
