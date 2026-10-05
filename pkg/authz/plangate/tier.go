package plangate

import (
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// Tier is how much human gating a phase needs.
//
// This is the incentive that makes narrow declaration rational without any
// honesty policing: a phase that asks for little is approved cheaply or
// automatically, while one that asks for a lot costs the human a careful
// decision. The agent is never asked to be honest — it is made cheaper to be
// specific.
type Tier int

const (
	// Tier0 auto-approves: all-readonly, at least one handle declared, and
	// within the session's cumulative auto-approve budget.
	Tier0 Tier = 0
	// Tier1 needs one human approval of the phase.
	Tier1 Tier = 1
	// Tier2 needs a human approval of the phase AND leaves the per-call
	// external approval fully intact.
	Tier2 Tier = 2
)

// AutoApproves reports whether a phase at this tier runs without a human.
func (t Tier) AutoApproves() bool { return t == Tier0 }

// HandleImpact pairs a declared handle with the severity the surface assigned
// it. The caller resolves impact from the surface; ComputeTier does not look
// anything up, so it stays a pure function of its input.
type HandleImpact struct {
	Handle      permsurface.Handle
	StateImpact authz.StateImpact
}

// TierInput is everything tiering needs. All of it is computed by the runtime
// from the frozen plan and the live surface — nothing here is agent-authored,
// so an agent cannot talk its way into a cheaper tier.
type TierInput struct {
	Handles []HandleImpact

	// MaxAutoApproveHandles bounds the UNION of handles auto-approved without
	// a human across the whole session. Zero means no auto-approval at all,
	// never "unlimited".
	MaxAutoApproveHandles int

	// AlreadyAutoApproved is how much of that session budget is spent.
	AlreadyAutoApproved int

	// MaxSingleCardHandles bounds what may be approved from a summary card.
	// Past it a phase renders per-handle. Zero disables the check.
	MaxSingleCardHandles int

	// UngrantedSlots is how many of the phase's slot requests the session does
	// NOT already hold a grant for.
	//
	// Resolved by the caller, because whether a grant exists is a SpiceDB
	// question and ComputeTier is a pure function of its input — the same reason
	// AlreadyAutoApproved is passed in rather than looked up.
	//
	// Slots the session already holds count for nothing: the approval that
	// created those grants already happened, and re-pricing a phase for reach it
	// demonstrably has is the fatigue the gradient exists to avoid.
	UngrantedSlots int

	// PendingConsents are exact requests included in the phase's human
	// approval. Readonly permissions never waive those decisions.
	PendingConsents int
}

// severityTier maps the worst StateImpact in a phase onto a tier.
//
// An UNRECOGNIZED impact prices as the most severe. A new impact level added
// upstream must not silently auto-approve until somebody remembers to update
// this switch — the failure direction has to be "too much friction", which an
// operator notices, not "silently permitted", which nobody does.
func severityTier(si authz.StateImpact) Tier {
	switch si {
	case authz.Readonly:
		return Tier0
	case authz.Readwrite:
		return Tier1
	case authz.External:
		return Tier2
	case authz.Stateless, authz.Passthrough:
		// Outside the surface entirely; contributes no severity.
		return Tier0
	default:
		return Tier2
	}
}

// ComputeTier prices a phase on BOTH axes: how severe its handles are, and how
// many of them there are.
//
// Breadth matters as much as severity, and leaving it out makes the whole
// feature vacuous — see TestComputeTier_breadthEscalatesIndependentlyOfSeverity
// for the argument. In short: if forty readwrite handles cost the same one
// click as a single handle, the dominant strategy is one phase holding the
// entire surface, the active phase never changes, and the effective
// authorization is today's session scope plus a card.
func ComputeTier(in TierInput) Tier {
	// A phase declaring nothing has no ceiling to auto-approve, and letting it
	// through at tier 0 would make "declare nothing" the cheapest plan of all.
	if len(in.Handles) == 0 {
		return Tier1
	}

	worst := Tier0
	for _, h := range in.Handles {
		if t := severityTier(h.StateImpact); t > worst {
			worst = t
		}
	}
	if worst > Tier0 {
		return worst
	}

	// A slot request the session does not already hold costs a human, whatever
	// the handles say. Tier 0 prices the CLASS axis — "these handles only read"
	// — but approving a slot request MINTS a SpiceDB grant on somebody's
	// resource, which no amount of readonly-ness makes free. Without this the
	// cheapest possible phase (all-readonly, inside the budget) would acquire an
	// instance grant with nobody asked.
	if in.UngrantedSlots > 0 || in.PendingConsents > 0 {
		return Tier1
	}

	// All-readonly. It still only auto-approves inside the session-cumulative
	// budget: breadth escalates on its own.
	if in.MaxAutoApproveHandles <= 0 {
		return Tier1
	}
	if in.AlreadyAutoApproved+len(in.Handles) > in.MaxAutoApproveHandles {
		return Tier1
	}
	return Tier0
}

// RendersPerHandle reports whether this phase is too wide to approve from a
// summary card.
//
// Past the threshold the card renders each handle with its own justification
// and the approver acknowledges the breadth explicitly. This does not prevent
// approving a wide plan; it stops a wide plan from being CHEAPER than a narrow
// one, which is the property the incentive depends on.
func RendersPerHandle(in TierInput) bool {
	return in.MaxSingleCardHandles > 0 && len(in.Handles) > in.MaxSingleCardHandles
}

// StillNeedsPerCallApproval reports whether the per-call external approval
// remains in force after this phase is approved.
//
// It always does when the phase holds an `external` handle. Tier 2 is not a
// shortcut: `external` means "irreversible side effects, re-approve per call",
// and no amount of phase-level approval consumes that.
func StillNeedsPerCallApproval(in TierInput) bool {
	for _, h := range in.Handles {
		if h.StateImpact == authz.External {
			return true
		}
	}
	return false
}
