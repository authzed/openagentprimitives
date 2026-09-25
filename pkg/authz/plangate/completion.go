package plangate

import (
	"fmt"
	"sort"
	"strings"
)

// BlockedByIncompletePrerequisite reports whether opening `target` must wait on
// a phase that has not finished.
//
// # The rule
//
// Leaving a phase unfinished is ALLOWED. An agent that opens a new phase with
// work outstanding elsewhere is not doing anything wrong — the unfinished phase
// is simply marked as such. Strict sequencing would be worse than useless: an
// agent that discovers phase 1 is unnecessary would be wedged into completing
// it, and every abandoned branch would end the session.
//
// The single exception is a DEPENDENCY. If the target's plan says it requires
// phase P, then opening it before P is finished runs work whose prerequisite
// the approver was told would come first. That ordering is part of what they
// cleared, so it is the one case that blocks.
//
// # Why an agent-asserted signal is sound here
//
// Completion derives from item status, which the agent sets — and the design
// otherwise forbids authorization state the agent determines (`Item.Status` is
// named in the spec as a selector already caught failing that bar). It is sound
// here for one specific reason: **requiring completion is strictly stronger
// than the entry requirement it adds to.**
//
// Without this, an edge is satisfied by P having been ENTERED. With it, P must
// be entered AND reported finished. An agent that lies about finishing has done
// strictly more than yesterday's agent did to reach the same phase — it lands
// exactly where the entry-only rule already put it, never further. There is no
// new reach on offer, so a false assertion buys nothing.
//
// That argument is load-bearing. If a future change ever makes completion grant
// something rather than withhold it, this stops being safe and needs a
// runtime-observed signal instead.
//
// # Unknown means incomplete
//
// `complete` carries what the runtime could establish. A phase absent from it
// is treated as unfinished, because absence means nothing could be established
// — and reading that as "finished" would release a dependent phase on no
// evidence at all.
//
// An out-of-range target is NOT this function's problem: the caller's own
// bounds check owns that error, and answering here would replace a precise
// message with a misleading one about prerequisites.
func BlockedByIncompletePrerequisite(plan Plan, target int, complete map[int]bool) (bool, string) {
	if target < 0 || target >= len(plan.Phases) {
		return false, ""
	}

	var outstanding []string
	seen := map[int]struct{}{}
	for _, edge := range plan.Phases[target].Requires {
		if edge.Phase < 0 || edge.Phase >= len(plan.Phases) {
			// A dangling edge cannot be satisfied by anything, but freeze already
			// drops those — so reaching here means the plan was built by another
			// path. Skip rather than block forever on a phase that does not exist.
			continue
		}
		if complete[edge.Phase] {
			continue
		}
		if _, dup := seen[edge.Phase]; dup {
			continue
		}
		seen[edge.Phase] = struct{}{}
		outstanding = append(outstanding, phaseName(plan, edge.Phase))
	}
	if len(outstanding) == 0 {
		return false, ""
	}
	sort.Strings(outstanding)

	return true, fmt.Sprintf(
		"this phase requires %s to be finished first. Complete the outstanding work "+
			"there and state its outcome, then select this phase again — or call "+
			"update_plan if the dependency no longer holds.",
		strings.Join(outstanding, " and "))
}

// phaseName renders a phase for a message: its label when it has one, else its
// index. An unlabelled phase still has to be nameable, or the agent is told to
// go finish something it cannot identify.
func phaseName(plan Plan, index int) string {
	if index >= 0 && index < len(plan.Phases) {
		if l := plan.Phases[index].Label; l != "" {
			return fmt.Sprintf("%q", l)
		}
	}
	return fmt.Sprintf("phase %d", index)
}
