package plangate

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// WriteChildRoot derives a delegated child's plan-gate root from its PARENT's
// log and writes it into the CHILD's scope — the whole read→fold→derive→write,
// in one place so the operator and the e2e harness (the two wiring sites) cannot
// drift. parent and child are scope IDs ("<namespace>/<name>").
//
// # Why the child MUST already have this when its runner starts
//
// The child runner reads its plan-gate log exactly ONCE, at startup
// (Loop.PlanGateRecords). A plan-gated child with no root there fails EVERY
// permissioned call with "requires an approved plan before any" and cannot
// recover — it has no update_plan to declare one. So the caller writes this
// BEFORE the child AgentSession CR exists: the runner starts only once the CR
// appears, and writing to a memory scope is keyed by the id string, not gated on
// a live object, so the root is guaranteed present by the time anything reads it.
// A write racing the runner AFTER create loses — the child reads an empty log
// and fails every permissioned call. The plangate-child-amends-its-own-ceiling
// bundle exercises a child running under, and then WIDENING (via an approved
// plan_amendment), that inherited root, so it stands on the root being present
// before the runner starts.
//
// # Idempotent, per record
//
// plan_gate_audit is append-only, and this runs on every reconcile of the
// request (including an AlreadyExists retry). It writes up to two records — the
// plan_approved root and, for an auto-cleared read-only phase, a tier-0
// clearance — and skips whichever already exists, so a re-reconcile stacks
// neither a second root nor a second clearance. Tracking both (rather than
// short-circuiting on the root alone) is what lets a partial first write, or a
// child written before this parity fix existed, still gain its missing clearance
// on the next reconcile instead of being stranded needing a human to clear a
// phase it cannot clear itself.
//
// Writes nothing (nil) when the parent ran no plan gate (empty log), when the
// log carries no reconstructable plan, or when the parent's fold is doubtful:
// each is a state the child correctly inherits as "no inherited ceiling", which
// the enforcing gate then treats as fail-closed rather than as authority.
func WriteChildRoot(ctx context.Context, mem memory.Memory, parent, child string) error {
	ctx = memory.WithSystemApproval(ctx, "delegation_plan_root")
	cscope := memory.Scope{Kind: "session", ID: child}

	// Idempotency, per RECORD rather than "a root exists". A root already written
	// short-circuits deriving another, but the tier-0 clearance is a SECOND record
	// that a partial write (root landed, clearance did not) or a first run
	// predating this parity fix can leave missing — and returning on the root
	// alone would strand the child needing a human to clear a read-only phase it
	// cannot clear itself. So track both, and below write only what is absent.
	haveRoot, haveClearance := false, false
	if existing, err := plangateaudit.List(ctx, mem, cscope); err == nil {
		for _, r := range existing {
			switch r.Event {
			case plangateaudit.EventPlanApproved:
				haveRoot = true
			case plangateaudit.EventPhaseApproved:
				haveClearance = true
			}
		}
	}

	if haveRoot && haveClearance {
		return nil // both records already present; nothing to re-derive.
	}

	pscope := memory.Scope{Kind: "session", ID: parent}
	records, err := plangateaudit.List(ctx, mem, pscope)
	if err != nil {
		return fmt.Errorf("read parent plan-gate log: %w", err)
	}
	if len(records) == 0 {
		return nil // parent ran no plan gate; the child inherits none
	}
	plan, ok := PlanFromRecords(records)
	if !ok {
		return nil
	}
	st, err := Fold(plan, records)
	if err != nil {
		return fmt.Errorf("fold parent plan-gate state: %w", err)
	}
	if st.Doubtful {
		return nil
	}
	// Derived unconditionally: DeriveForChild is a deterministic function of the
	// parent's log, so a root re-derived on a retry is byte-identical to one
	// already stored — which is what lets the clearance below be derived from it
	// even when the root itself was written on an earlier pass.
	root, err := DeriveForChild(ChildRootInput{
		Parent:        parent,
		Plan:          plan,
		VisiblePhases: []int{st.ActivePhase},
		Denied:        st.DeniedCeilings(),
	})
	if err != nil {
		return fmt.Errorf("derive child root: %w", err)
	}
	if !haveRoot {
		if err := plangateaudit.Record(ctx, mem, cscope, root); err != nil {
			return err
		}
	}

	// Tier-0 parity: if the parent AUTO-CLEARED the inherited phase (a read-only
	// phase that would clear itself if the child had declared it), carry that
	// clearance into the child's log too. Without it the child's own gate folds
	// the inherited phase UNAPPROVED and raises a plan_phase the delegated child
	// can neither answer nor self-clear, leaving its inherited read ceiling
	// unusable. Scoped to tier 0 on purpose: a phase the parent needed a HUMAN to
	// clear is NOT laundered into an automatic child clearance — that child
	// re-asks, routed to the parent's approvers, exactly as before.
	if !haveClearance && parentAutoClearedPhase(records, st.ActivePhase) {
		if clearance, ok := DeriveChildPhaseClearance(root); ok {
			if err := plangateaudit.Record(ctx, mem, cscope, clearance); err != nil {
				return fmt.Errorf("record child tier-0 clearance: %w", err)
			}
		}
	}
	return nil
}

// parentAutoClearedPhase reports whether the parent auto-approved this phase at
// tier 0 — read straight off the parent's own EventPlanApproved record, which
// FreezeAndRecordPhases stamped with the tier it priced the phase at. Tier "0"
// is the all-readonly, within-budget disposition that clears without a human, and
// it is exactly the disposition a delegated child should inherit as already
// cleared. Any other tier (or a phase with no such record) means "a human was
// involved, or would be" — the child does not auto-clear.
//
// The in-force plan's digest is read from the records themselves (the latest
// EventPlanApproved), NOT recomputed from a rebuilt plan: PlanForDigest
// normalizes fields like MaxCount (0 becomes 1), so a plan rebuilt from the log
// can hash differently than the digest the log was written under. Matching on the
// recorded digest is what keeps this tied to the plan the records actually
// describe. The phase INDEX is stable across that normalization, so
// st.ActivePhase (folded against the rebuilt plan) still names the right row.
func parentAutoClearedPhase(records []plangateaudit.Content, phase int) bool {
	inForce := ""
	for _, r := range records {
		if r.Event == plangateaudit.EventPlanApproved && r.PlanDigest != "" {
			inForce = r.PlanDigest
		}
	}
	if inForce == "" {
		return false
	}
	for _, r := range records {
		if r.Event != plangateaudit.EventPlanApproved || r.PlanDigest != inForce || r.PhaseIndex == nil {
			continue
		}
		if int(*r.PhaseIndex) == phase {
			return r.Tier == "0"
		}
	}
	return false
}
