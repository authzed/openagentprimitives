package plangate

import (
	"fmt"
	"sort"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// AuthoredPhase is one phase as the AGENT wrote it, projected by the caller.
//
// plangate stays a pure value package, so it does not import the plans state
// kind; the runner projects plans.Phase into this shape, the same contract
// permsurface.Candidate uses. Everything here is untrusted input to FreezeFrom.
type AuthoredPhase struct {
	ID          string
	Label       string
	Why         string
	Requires    []AuthoredRequires
	Max         *AuthoredMax
	Budget      *AuthoredBudget
	Permissions []AuthoredPermission
	// Slots are the instance-axis request: the resource types this phase
	// intends to touch. Validated at freeze against the class's declared slot
	// types, because approving one writes a SpiceDB grant.
	Slots []AuthoredSlot
}

type AuthoredRequires struct {
	Phase string // the agent's id for the prerequisite
	Why   string
}

type AuthoredMax struct {
	Count int
	Why   string
}

// AuthoredBudget is a phase's governed-call budget as the agent asked for it.
type AuthoredBudget struct {
	Calls int
	Why   string
}

type AuthoredPermission struct {
	Handle string // the agent's spelling; validated against the live surface
	Why    string
}

// FreezeProblem is one thing FreezeFrom refused to carry into the frozen plan.
//
// Problems are surfaced, never silently swallowed: a dropped handle narrows
// what the agent can do (safe) but a dropped ordering constraint would widen
// what it may do without anyone noticing (not safe). Both are reported so the
// approval card and the operator log can show what was discarded.
type FreezeProblem struct {
	PhaseIndex int
	Detail     string
}

// PhaseRef identifies a phase for approvals, denials, budgets and audit
// records.
//
// Identity is (frozen-plan digest, index) and never the agent's own id. That is
// what makes an approval untransplantable: re-submitting a differently-shaped
// plan whose phase happens to sit at the same index produces a different
// digest, so an approval granted against the old plan does not resolve against
// the new one.
type PhaseRef struct {
	PlanDigest string
	Index      int
}

// SamePlan reports whether two refs belong to the same frozen plan.
func (r PhaseRef) SamePlan(other PhaseRef) bool { return r.PlanDigest == other.PlanDigest }

func (r PhaseRef) String() string { return fmt.Sprintf("%s#%d", r.PlanDigest, r.Index) }

// FreezeFrom converts an authored phase list into the frozen plan a human
// approves and the gate reads.
//
// Three things happen here, exactly once, so they never happen at decision
// time:
//
//   - The agent's phase IDs are DROPPED. The frozen Phase has no id field at
//     all, so no future code can key authorization on a name the agent chose —
//     a structural guarantee rather than a rule someone has to remember.
//
//   - Requires edges resolve from ids to INDICES, after the whole id set is
//     known (so forward references are legal). An edge naming a phase that
//     does not exist is reported and dropped rather than guessed at.
//
//   - Declared handles are validated against the LIVE surface. The surface is
//     the authority on what exists: a handle absent from it is one the
//     dispatcher will never check, so carrying it into a ceiling would put a
//     meaningless entry in front of an approver. Unparseable handles are
//     rejected the same way — never sanitized into something adjacent, which
//     is the collision permsurface exists to prevent.
//
// A nil surface skips the on-surface check (handles are still parsed), which is
// what tests and the no-tools case want; production always passes the live one.
// declaredSlotTypes is the set of resource types the AgentClass declares in
// spec.authz.slots — the instance axis's analogue of the surface. A request for
// a type not in it is dropped, because a slot request becomes a SpiceDB grant
// on approval and a class that never declared the type never opted into having
// grants written against it. Nil means the class does not use the instance
// axis, so every slot request is undeclared.
func FreezeFrom(
	authored []AuthoredPhase, surface []permsurface.Descriptor, declaredSlotTypes []string,
) (Plan, []FreezeProblem) {
	var problems []FreezeProblem

	onSurface := make(map[permsurface.Handle]struct{}, len(surface))
	for _, d := range surface {
		onSurface[d.Handle] = struct{}{}
	}

	declaredSlot := make(map[string]struct{}, len(declaredSlotTypes))
	for _, s := range declaredSlotTypes {
		declaredSlot[s] = struct{}{}
	}

	// Index the agent's ids first so requires can resolve forward references.
	// A duplicate id keeps the FIRST declaration: update_plan already refuses
	// duplicates, and picking deterministically beats picking arbitrarily if
	// one ever reaches here.
	indexByID := make(map[string]int, len(authored))
	for i, p := range authored {
		if _, dup := indexByID[p.ID]; !dup {
			indexByID[p.ID] = i
		}
	}

	out := Plan{Phases: make([]Phase, 0, len(authored))}
	for i, p := range authored {
		ph := Phase{Label: p.Label, Why: p.Why, Max: MaxSpec{Count: 1}}
		if p.Max != nil {
			ph.Max = MaxSpec{Count: p.Max.Count, Why: p.Max.Why}
		}
		if p.Budget != nil {
			ph.Budget = BudgetSpec{Calls: p.Budget.Calls, Why: p.Budget.Why}
		}

		for _, perm := range p.Permissions {
			h, err := permsurface.ParseHandle(perm.Handle)
			if err != nil {
				problems = append(problems, FreezeProblem{
					PhaseIndex: i,
					Detail:     fmt.Sprintf("permission %q is not a valid handle: %v", perm.Handle, err),
				})
				continue
			}
			if len(surface) > 0 {
				if _, ok := onSurface[h]; !ok {
					problems = append(problems, FreezeProblem{
						PhaseIndex: i,
						Detail: fmt.Sprintf("permission %q is not on this session's permission surface; "+
							"no tool can reach it, so it was dropped", perm.Handle),
					})
					continue
				}
			}
			ph.Permissions = append(ph.Permissions, h)
		}

		// Slots, deduped and sorted so the frozen plan is a pure function of the
		// request — the property that makes an identical re-submit digest
		// identically instead of re-asking a human.
		seenSlot := map[string]struct{}{}
		for _, s := range p.Slots {
			if _, ok := declaredSlot[s.Type]; !ok {
				problems = append(problems, FreezeProblem{
					PhaseIndex: i,
					Detail: fmt.Sprintf("slot type %q is not declared in this agent's authz.slots; "+
						"approving it would write a grant the class never opted into, so it was dropped", s.Type),
				})
				continue
			}
			// Dedup on (type, VALUE), not type alone. Two slots of the same
			// type naming DIFFERENT resources are two distinct grants —
			// collapsing them would silently drop one target the agent declared
			// and the approver read.
			key := s.Type + "\x1f" + s.ID
			if _, dup := seenSlot[key]; dup {
				continue
			}
			seenSlot[key] = struct{}{}
			// The value is carried VERBATIM, and the card shows this form rather
			// than the object id the grant lands on. That is honest only because
			// the chain is injective: an expr-keyed check may use none but
			// injective transforms (validatePermissionShape enforces it, for
			// toolkits as well as MCP tools), so exactly one value maps to the
			// granted id and the value shown is the only one that can spend the
			// approval. Were a lossy transform admitted, the displayed value
			// would be a claim that is not true and the card would have to
			// render the post-transform form instead.
			//
			// Why rides along for display and is excluded from the digest, so
			// rewording it cannot change what was approved.
			ph.Slots = append(ph.Slots, Slot{Type: s.Type, ID: s.ID, Why: s.Why})
		}
		sort.Slice(ph.Slots, func(a, b int) bool {
			if ph.Slots[a].Type != ph.Slots[b].Type {
				return ph.Slots[a].Type < ph.Slots[b].Type
			}
			return ph.Slots[a].ID < ph.Slots[b].ID
		})

		for _, r := range p.Requires {
			target, ok := indexByID[r.Phase]
			if !ok {
				problems = append(problems, FreezeProblem{
					PhaseIndex: i,
					Detail:     fmt.Sprintf("requires names phase %q, which is not declared; the edge was dropped", r.Phase),
				})
				continue
			}
			ph.Requires = append(ph.Requires, RequiresEdge{Phase: target, Why: r.Why})
		}

		out.Phases = append(out.Phases, ph)
	}

	return out, problems
}
