// Package plangate holds the plan gate's value types: the phase model, the
// frozen approved plan, and the ceiling derived from it.
//
// This is a PURE VALUE package, like permsurface. It imports pkg/authz,
// pkg/authz/permsurface and stdlib — never pkg/agent/tool, pkg/memory or
// pkg/pipeline. The hook does the wiring and projects into these types.
//
// # What is trusted here, and what is not
//
// Every `Why` field is authored by the agent and is UNTRUSTED. It is carried so
// an approver and an auditor can read the agent's stated reasoning, and it is
// never parsed, never matched against, and never allowed to influence
// authorization. That is enforced structurally by Digest, which covers a plan's
// authority and deliberately excludes all narration — see Digest.
//
// The permissions, budgets and prerequisites ARE authority. They are still
// agent-authored, but they only take effect once a human approves the plan they
// belong to, and the plan is frozen at that moment.
package plangate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// MaxSpec bounds how many times a phase may be entered.
//
// Count defaults to 1: agents are told to plan for a single entry unless they
// explicitly justify more, so an approver can see "this phase runs once" as the
// norm and a request for more as a deliberate, explained exception.
type MaxSpec struct {
	Count int
	// Why is the agent's justification for a Count above the default.
	// Agent-authored and untrusted; display only.
	Why string
}

// BudgetSpec bounds how much work a phase may do inside its ceiling.
//
// The focus mechanism, and the only one that catches RABBIT-HOLING: an agent
// grinding forty calls out of a perfectly legal readonly ceiling breaks no
// permission rule, so no permission model can see it. A budget can.
//
// Counted in GOVERNED CALLS — tool calls that resolved to a permission handle.
// Turns would be the more intuitive unit and are deliberately not used: a turn
// is not attributable to a phase without new per-turn plumbing, whereas the
// gate already records one entry per governed call with its active phase, so a
// call budget is derivable from the log the ceiling test already reads. Nothing
// new to store, and nothing that can drift from what the gate actually saw.
//
// Meta and passthrough calls spend nothing (see State.CallsSpent): letting
// update_plan burn the budget would make re-planning the thing that exhausts
// the budget forcing the re-plan.
type BudgetSpec struct {
	// Calls bounds governed calls while this phase is active. ZERO MEANS
	// UNBOUNDED, not "no calls" — an undeclared budget must never read as a
	// denial, or every phase that did not opt in would refuse its first call.
	// The runtime supplies the default; this is the agent's request.
	Calls int
	// Why is the agent's justification for a budget above the default.
	// Agent-authored and untrusted; display only.
	Why string
}

// RequiresEdge declares that a phase may not be entered until another phase has
// been entered. The prerequisite is satisfied by runtime-recorded ENTRY, never
// by an agent asserting it finished something.
type RequiresEdge struct {
	// Phase is the prerequisite's index into Plan.Phases.
	Phase int
	// Why is the agent's justification for the ordering constraint.
	// Agent-authored and untrusted; display only.
	Why string
}

// Slot is one resource TYPE a phase asks to touch — the instance axis's half of
// a phase's authority.
//
// A slot request is not a narrowing the way a permission ceiling is. It is a
// REQUEST that, when the plan is approved, causes a SpiceDB slot grant to be
// written for the instances that fill it. Approving a plan is therefore
// approving its slots, and that is why Slot lands in the digest while every
// `Why` stays out: adding one changes what approval will grant.
//
// Only the type is frozen. The instances come from the fill sources the class
// declares (`AuthzSlot.fillFrom`, `autoGrantFrom`, `channel_thread`), and every
// fill already passes through a Check against the requester — an instance binds
// only when the user already had standing on it (see authz.checkBindable), so
// the instance axis can never be an escalation.
type Slot struct {
	// Type is the SpiceDB resource type, validated at freeze against the types
	// the AgentClass declares in spec.authz.slots. The agent's own spelling
	// never reaches the frozen plan unchecked.
	Type string

	// ID is the concrete instance, when the agent could name one. Empty means
	// "not known yet" — a legal state the card must surface, because a phase
	// that cannot name its target is not covered by this approval.
	//
	// Authority, not commentary: it is in the digest. See AuthoredSlot.ID.
	ID string

	// Why is the agent's stated reason for needing THIS resource, carried
	// through freeze for the card. Agent-authored and untrusted: display only,
	// rendered in the card's Why, and excluded from the digest and from
	// AuthorityKey.
	//
	// Carried rather than dropped because "which repository" and "why that
	// repository" are different questions, and an approver deciding a named
	// target wants both. Excluded from authority for the same reason Phase.Why
	// is: if prose were identity, rewording a justification would mint a new
	// plan and silently discard an approval already given — and an agent could
	// vary it deliberately to escape a phase it had been refused.
	Why string
}

// AuthoredSlot is a slot request as the agent wrote it, before validation.
type AuthoredSlot struct {
	Type string

	// ID is the concrete instance this phase will act on — the resource itself,
	// not its kind. Optional: an agent that genuinely cannot name its target yet
	// declares the type alone, and the card marks that phase as needing a later
	// approval.
	//
	// Naming it up front is what lets ONE approval cover the work. Without it a
	// plan could only ever say "I need a github_repo slot", so the approver was
	// agreeing to a category and every instance had to be decided later, one
	// prompt at a time.
	//
	// UNLIKE Why, this is AUTHORITY and enters the digest. Approving
	// "read foo/bar" must not be reusable for "read baz/qux".
	ID string

	// Why is the agent's stated reason for needing the slot. Agent-authored and
	// untrusted; display only, and excluded from the digest.
	Why string
}

// Phase is one step of a plan: what the agent intends to do, and the authority
// it needs to do it.
type Phase struct {
	// Label is the phase's human-readable name, for the approval card and the
	// audit trail. Agent-authored and untrusted; display only.
	//
	// Note what is NOT here: the agent's phase ID. Freezing drops it, so no
	// code can key authorization on a name the agent chose. Identity is
	// PhaseRef{digest, index} — a structural guarantee rather than a rule
	// someone has to remember.
	Label string

	// Why is the agent's stated purpose for this phase. Agent-authored and
	// untrusted; display only.
	Why string

	// Permissions is this phase's CLASS ceiling — the set of permission handles
	// a call may match while this phase is active. A pure narrowing: membership
	// can only fail to deny, never grant.
	Permissions []permsurface.Handle

	Max      MaxSpec
	Requires []RequiresEdge

	// Budget bounds the work this phase may do inside its ceiling. Zero means
	// the phase declared none, and the runtime default applies.
	Budget BudgetSpec

	// Slots are the resource types this phase asks to touch. Empty means the
	// phase does not constrain the instance axis — which is the shape every
	// plan had before slots existed, and stays valid.
	Slots []Slot
}

// Plan is an ordered, frozen list of phases.
//
// Order is significant and load-bearing: a phase's INDEX is its identity, and
// RequiresEdge names phases by index. Once approved a Plan is never mutated —
// superseding it is a new Plan and a new human decision.
type Plan struct {
	Phases []Phase
}

// SessionPlan synthesizes the SEED plan: a single phase whose ceiling is the
// entire permission surface.
//
// This is what makes turning the gate on provably behavior-neutral. Every handle the
// dispatcher could check is in phase 0's ceiling, so the membership test always
// passes and no call that succeeds today can be denied. The phase model, the
// audit records, the cards and the rendering are all exercised for real against
// a ceiling that cannot bite.
func SessionPlan(surface []permsurface.Descriptor) Plan {
	ph := Phase{
		Why:         "session scope, expressed as a single phase",
		Permissions: make([]permsurface.Handle, 0, len(surface)),
		Max:         MaxSpec{Count: 1},
	}
	for _, d := range surface {
		ph.Permissions = append(ph.Permissions, d.Handle)
	}
	return Plan{Phases: []Phase{ph}}
}

// Ceiling returns the set of handles the phase at index authorizes.
//
// An out-of-range index is an ERROR, not an empty set. Under enforcing an empty
// ceiling denies everything, so silently returning one would turn a lookup bug
// into a total outage that looks like a policy decision.
func (p Plan) Ceiling(index int) (map[permsurface.Handle]struct{}, error) {
	if index < 0 || index >= len(p.Phases) {
		return nil, fmt.Errorf("plangate: phase index %d out of range (plan has %d phases)", index, len(p.Phases))
	}
	ph := p.Phases[index]
	out := make(map[permsurface.Handle]struct{}, len(ph.Permissions))
	for _, h := range ph.Permissions {
		out[h] = struct{}{}
	}
	return out, nil
}

// Digest identifies a plan by its AUTHORITY.
//
// It covers the ordered phase list and, per phase, the permission set, the max
// count, and the requires edges. It deliberately excludes every Why field.
//
// Both halves of that matter:
//
//   - Excluding narration means an agent re-wording a justification produces
//     the same digest, so a supersede check does not fire on cosmetic edits and
//     train approvers to click through them.
//
//   - Including all authority means an agent cannot widen a permission set,
//     raise a budget, or drop a prerequisite while presenting an unchanged
//     digest.
//
// Permissions are sorted before hashing because a phase ceiling is a SET —
// re-declaring the same handles in a different order authorizes identically and
// must digest identically. Phases are NOT sorted: their order is authority.
// AuthorityKey identifies a phase by WHAT IT MAY DO — its permission set and
// the instances it names — independent of which plan it sits in or where.
//
// This is what an approval is keyed on. Approval used to key on
// PhaseRef{PlanDigest, Index}, so ANY re-plan invalidated EVERY approval and a
// mere reorder invalidated them too. Re-planning re-charged the user for phases
// they had already read and approved, which is the opposite of the goal: approve
// as much as possible, as early as possible, and only ask again when something
// actually changed.
//
// It covers AUTHORITY and nothing else. Label and Why are agent-authored and
// excluded, deliberately: an agent may re-word its justification freely, and
// re-asking for the same authority under a new story is exactly how users are
// trained to click through. It may not re-word its way into new authority,
// because everything that widens what the phase can do IS in here.
//
// Distinct from Plan.Digest, which identifies the whole plan including phase
// ORDER (order is authority: Requires depends on it). A phase key is
// deliberately order-free.
func (p Phase) AuthorityKey() string {
	sum := sha256.New()

	perms := make([]string, 0, len(p.Permissions))
	for _, h := range p.Permissions {
		perms = append(perms, h.String())
	}
	sort.Strings(perms)
	for _, s := range perms {
		io.WriteString(sum, "perm\x00")
		io.WriteString(sum, s)
		io.WriteString(sum, "\x00")
	}

	// Type AND value, same as the plan digest: "read foo/bar" and "read
	// baz/qux" are different authority and must never share an approval.
	slots := make([]string, 0, len(p.Slots))
	for _, sl := range p.Slots {
		slots = append(slots, sl.Type+"\x1f"+sl.ID)
	}
	sort.Strings(slots)
	for _, s := range slots {
		io.WriteString(sum, "slot\x00")
		io.WriteString(sum, s)
		io.WriteString(sum, "\x00")
	}

	// The call budget is authority over TIME — the same ceiling exercised more
	// often reaches further — so raising it must re-key the phase exactly as
	// widening the handle set does. Same reason Plan.Digest covers it.
	io.WriteString(sum, "budget\x00")
	io.WriteString(sum, strconv.Itoa(p.Budget.Calls))
	io.WriteString(sum, "\x00")

	// The entry count is the OTHER axis of authority over time, and it is
	// exactly what the approver's card states ("this phase runs once"). Same
	// argument as the budget, and Plan.Digest already covers it.
	io.WriteString(sum, "max\x00")
	io.WriteString(sum, strconv.Itoa(p.Max.Count))
	io.WriteString(sum, "\x00")

	// Prerequisites are authority: an edge is a promise that another phase runs
	// FIRST, and dropping one lets the agent enter this phase immediately.
	// Without this here, a re-plan that removed the edge kept a byte-identical
	// key, approvedKeys hit, and PhaseApproved returned true before CarriesOver
	// — which would have caught it through droppedRequires — was ever
	// consulted. Only the phase numbers, sorted: a key is deliberately
	// order-free, and Why is narration.
	reqs := make([]int, 0, len(p.Requires))
	for _, r := range p.Requires {
		reqs = append(reqs, r.Phase)
	}
	sort.Ints(reqs)
	for _, r := range reqs {
		io.WriteString(sum, "req\x00")
		io.WriteString(sum, strconv.Itoa(r))
		io.WriteString(sum, "\x00")
	}

	return hex.EncodeToString(sum.Sum(nil))
}

func (p Plan) Digest() string {
	sum := sha256.New()
	for i, ph := range p.Phases {
		io.WriteString(sum, "phase\x00")
		io.WriteString(sum, strconv.Itoa(i))
		io.WriteString(sum, "\x00")

		perms := make([]string, 0, len(ph.Permissions))
		for _, h := range ph.Permissions {
			perms = append(perms, h.String())
		}
		sort.Strings(perms)
		for _, s := range perms {
			io.WriteString(sum, "perm\x00")
			io.WriteString(sum, s)
			io.WriteString(sum, "\x00")
		}

		io.WriteString(sum, "max\x00")
		io.WriteString(sum, strconv.Itoa(ph.Max.Count))
		io.WriteString(sum, "\x00")

		// The call budget is authority over TIME: the same ceiling exercised
		// more often reaches more, so raising it must re-key the plan exactly as
		// widening the handle set does.
		io.WriteString(sum, "budget\x00")
		io.WriteString(sum, strconv.Itoa(ph.Budget.Calls))
		io.WriteString(sum, "\x00")

		reqs := make([]int, 0, len(ph.Requires))
		for _, r := range ph.Requires {
			reqs = append(reqs, r.Phase)
		}
		sort.Ints(reqs)
		for _, r := range reqs {
			io.WriteString(sum, "req\x00")
			io.WriteString(sum, strconv.Itoa(r))
			io.WriteString(sum, "\x00")
		}

		// Slots are authority: approving a plan that requests one writes the
		// grant that makes the instance axis pass. Sorted for the same reason
		// permissions are — a slot set authorizes identically whatever order it
		// was written in, so a reorder must not revoke an approval.
		// Type AND value. A slot's value is the resource authority lands on, so
		// "read foo/bar" and "read baz/qux" must be different plans — otherwise
		// an approval could be obtained for one target and spent on another.
		// The separator is \x1f so a type containing the delimiter cannot forge
		// a different pair.
		slots := make([]string, 0, len(ph.Slots))
		for _, s := range ph.Slots {
			slots = append(slots, s.Type+"\x1f"+s.ID)
		}
		sort.Strings(slots)
		for _, s := range slots {
			io.WriteString(sum, "slot\x00")
			io.WriteString(sum, s)
			io.WriteString(sum, "\x00")
		}
	}
	return hex.EncodeToString(sum.Sum(nil))
}
