package plangate

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// ChildRootInput is what deriving a DELEGATED child's plan-gate root needs.
//
// Distinct from ForkInput because a delegation is not a fork: a fork continues
// the SAME owner and inherits the whole active ceiling, while a child is a
// DIFFERENT actor that inherits only ITS PORTION of the parent's plan — the
// phase(s) the parent chose to hand it. Overloading DeriveForFork would inherit
// too much (see the "its portion" rule the per-recipient projection enforces).
type ChildRootInput struct {
	// Parent is the parent scope ID, recorded so the child's root names where
	// its authority came from and `ap audit verify` can hop across.
	Parent string

	// Plan is the parent's FROZEN plan. The child's ceiling is built from the
	// visible phases of it; the child never sees the phases it did not inherit.
	Plan Plan

	// VisiblePhases are the TRUE (0-based) indices of the parent's phases this
	// child inherits — normally the single phase the parent was in when it
	// delegated. Empty means the child inherits nothing and holds no authority
	// until it requests some (request_approval / request_plan_amendment).
	VisiblePhases []int

	// Denied are the parent's denied ceilings, from Fold. Carried forward
	// because denials travel: deny, delegate, and without this the child no
	// longer knows a human said no — delegation would launder the refusal.
	Denied [][]permsurface.Handle
}

// DeriveForChild produces the SINGLE record a delegated child's plan-gate log
// starts from.
//
// Like DeriveForFork it derives the resolved OUTCOME rather than copying the
// parent's chain (see DeriveForFork for why copying is unsafe): the child's
// chain begins at seq 0 with one honest root carrying an EXPLICIT ceiling, so
// the child's gate enforces it without needing the parent's plan in the child's
// scope.
//
// The ceiling is the union of the VISIBLE phases' Permissions — the child's
// portion, never the whole plan. A phase the child did not inherit confers
// nothing, which is what stops a read-only debug child from holding the fix
// phase's write reach.
func DeriveForChild(in ChildRootInput) (plangateaudit.Content, error) {
	// PhaseIndex is 0, NOT the parent's absolute phase number, and this is
	// load-bearing for ENFORCEMENT rather than display. Fold resets ActivePhase
	// to 0 on a plan_approved root (a fork/derived root is a fresh start, not a
	// mid-plan position), and ActiveCeiling reads the ceiling from THAT phase of
	// the reconstructed plan. So the inherited reach has to live at phase 0 or
	// the child folds to an empty ceiling and holds nothing — which is exactly
	// what an earlier version did until the round-trip test caught it. The
	// child's enforcement plan is single-phase: phase 0 IS its inherited portion.
	//
	// The parent's absolute phase number is a CARD concern (the projection keeps
	// it absolute for what an approver reads), carried in Provenance here, not a
	// position the child's own gate folds against.
	zero := int32(0)
	root := plangateaudit.Content{
		Event:      plangateaudit.EventPlanApproved,
		Provenance: fmt.Sprintf("delegated from %s phase(s) %v", in.Parent, in.VisiblePhases),
		PlanDigest: in.Plan.Digest(),
		PhaseIndex: &zero,
		// The same parent, as a FIELD, because a decision hangs off it:
		// HasInheritedCeiling raises the child's plan-gate mode to enforcing on
		// this, so the ceiling is enforced even when the child's own class
		// resolved the gate to disabled. Provenance above says the same thing
		// in prose for a human to read; enforcement must not depend on that
		// sentence keeping its shape.
		DelegatedFrom: in.Parent,
	}

	// Ceiling = union of the visible phases' Permissions, placed at phase 0.
	// Deduplicated by wire form so a handle two visible phases share appears once.
	seen := map[string]struct{}{}
	for _, i := range in.VisiblePhases {
		c, err := in.Plan.Ceiling(i)
		if err != nil {
			return plangateaudit.Content{}, fmt.Errorf("plangate: derive child ceiling for phase %d: %w", i, err)
		}
		for h := range c {
			if _, ok := seen[h.String()]; ok {
				continue
			}
			seen[h.String()] = struct{}{}
			root.Ceiling = append(root.Ceiling, h.String())
		}
	}
	sortStrings(root.Ceiling)

	// Denials travel.
	for _, denied := range in.Denied {
		for _, h := range denied {
			root.DeniedCeiling = append(root.DeniedCeiling, h.String())
		}
	}
	sortStrings(root.DeniedCeiling)

	return root, nil
}

// DeriveChildPhaseClearance builds the tier-0 clearance record for a child's
// inherited phase — the delegation-side analogue of the auto-approval
// FreezeAndRecordPhases writes when an agent declares a read-only phase directly.
//
// # Why a child needs this at all
//
// A declared read-only phase is auto-cleared (tier 0) at freeze time, so it runs
// with no human. A DELEGATED child's phase is not declared — it is projected by
// DeriveForChild — so nothing runs that auto-approval, and the child's own gate
// folds the inherited phase as UNAPPROVED. An enforcing child then raises a
// plan_phase it can neither answer (it is headless) nor self-clear (select_phase
// is withheld from a delegated child), and its inherited read ceiling is
// unusable. This record restores the parity: a phase that would auto-clear if
// declared, auto-clears when inherited.
//
// # Why it is the CALLER's decision to emit it
//
// This function only builds the record; WriteChildRoot decides whether to write
// it, and only when the parent auto-cleared that phase at tier 0. Two properties
// make that safe:
//
//   - It clears the PHASE, never the ceiling. The gate's ceiling check is
//     independent and runs first, so a call outside the inherited ceiling is
//     still refused however cleared the phase is — clearance authorizes running
//     the ceiling, not widening it.
//   - It is scoped to tier 0 (all-readonly). A human's approval of a non-tier-0
//     parent phase is NOT laundered into an automatic child clearance; such a
//     child re-asks, routed to the parent's approvers, exactly as before.
//
// # Why the record is key-bearing
//
// The child's root carries the PARENT's plan digest, so the child's own log folds
// it against a DIFFERENT (reconstructed, single-phase) digest and skips it as
// foreign for everything except the records Fold handles ahead of that skip. A
// key-bearing EventPhaseApproved is one of those: it folds into approvedKeys
// regardless of digest, and PhaseApproved reads the child phase's authority key
// back out of exactly that map. A digest+index clearance would be skipped and
// clear nothing.
//
// ok is false when the root carries no reconstructable phase (an empty
// projection — a child that inherited nothing has nothing to clear).
func DeriveChildPhaseClearance(root plangateaudit.Content) (plangateaudit.Content, bool) {
	childPlan, ok := PlanFromRecords([]plangateaudit.Content{root})
	if !ok || len(childPlan.Phases) == 0 {
		return plangateaudit.Content{}, false
	}
	// PhaseAuthorityRecord carries the phase's authority key — the same key
	// PhaseApproved looks up — so this clearance matches the child phase it
	// clears. nil standing: a tier-0 phase is all-readonly with no slot requests,
	// so there is no per-slot standing to record.
	clearance := PhaseAuthorityRecord(childPlan, 0, nil)
	clearance.Event = plangateaudit.EventPhaseApproved
	clearance.PlanDigest = childPlan.Digest()
	clearance.Provenance = "delegated:tier0"
	return clearance, true
}
