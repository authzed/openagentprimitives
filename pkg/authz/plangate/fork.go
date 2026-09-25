package plangate

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// ForkMode is how a child session relates to its parent.
type ForkMode string

const (
	// ForkInherit covers restart / inherit: the SAME owner continues, the
	// agentsession#fork gate ran, so approvals and the baseline carry forward.
	ForkInherit ForkMode = "inherit"

	// ForkTakeover is a DIFFERENT user continuing a terminal session, becoming
	// the child's owner. Nothing is inherited.
	ForkTakeover ForkMode = "takeover"
)

// ForkInput is what deriving a child's authorization root needs.
type ForkInput struct {
	Mode ForkMode

	// Parent is the parent scope ID, recorded so the child's root names where
	// its authority came from and `ap audit verify` can hop across.
	Parent string

	Plan    Plan
	Records []plangateaudit.Content
}

// DeriveForFork produces the SINGLE record a child session's plan-gate log
// starts from.
//
// The parent's records are never copied. They are a chain — meaning comes from
// order and from the per-(scope, publisher) provenance sequence — and copying
// them into a new scope produces seq and prevHash values referring to a history
// the child does not have. Worse, memcopy re-signs copies under one publisher,
// collapsing N chains into one and losing the original interleaving; since
// "a denial survives a later supersede" is order-dependent, a child could
// replay an approval and its revocation backwards and resurrect revoked
// authority.
//
// Deriving instead makes ordering irrelevant, because there is nothing left to
// order: the child inherits the resolved OUTCOME, not the steps that produced
// it. The parent's chain stays intact and independently verifiable in its own
// scope, and the child's chain begins at seq 0 with one honest root.
//
// Two cases inherit nothing, and both fail toward "re-plan and re-approve":
//
//   - takeover, because a different user becomes the owner. Inheriting would
//     hand them the previous owner's human-approved ceilings on resources they
//     never had standing on — and the agentsession#fork gate does not run for
//     this mode, so nothing else stops it.
//   - a DOUBTFUL parent fold, because a state the parent could not establish
//     must not become a confident-looking root in the child.
func DeriveForFork(in ForkInput) (plangateaudit.Content, error) {
	switch in.Mode {
	case ForkInherit, ForkTakeover:
	default:
		return plangateaudit.Content{}, fmt.Errorf("plangate: unknown fork mode %q", in.Mode)
	}

	root := plangateaudit.Content{
		Event:      plangateaudit.EventPlanApproved,
		Provenance: "fork:" + string(in.Mode) + " from " + in.Parent,
	}

	if in.Mode == ForkTakeover {
		// Deliberately empty: a taken-over session holds nothing until it plans
		// and is approved under its new owner.
		return root, nil
	}

	st, err := Fold(in.Plan, in.Records)
	if err != nil {
		return plangateaudit.Content{}, fmt.Errorf("plangate: fold parent state: %w", err)
	}
	if st.Doubtful {
		// Same reasoning as ActiveCeiling's empty-on-doubt: the recoverable
		// failure is "the child re-plans", not "the child inherits a guess".
		return root, nil
	}

	root.PlanDigest = in.Plan.Digest()
	idx := int32(st.ActivePhase)
	root.PhaseIndex = &idx

	if ceiling, cerr := st.ActiveCeiling(); cerr == nil {
		for h := range ceiling {
			root.Ceiling = append(root.Ceiling, h.String())
		}
		sortStrings(root.Ceiling)
	}

	// Denials travel, or forking becomes a laundering step: deny, fork, and the
	// child no longer knows a human said no.
	for _, denied := range st.deniedCeilings {
		for _, h := range denied {
			root.DeniedCeiling = append(root.DeniedCeiling, h.String())
		}
	}
	sortStrings(root.DeniedCeiling)

	// The envelope is the pre-exposure baseline for the TASK, and the task
	// continues. Re-deriving one now would launder post-exposure reach into a
	// "pre-exposure" baseline.
	if st.envelopeSet {
		for h := range st.envelope {
			root.Envelope = append(root.Envelope, h)
		}
		sortStrings(root.Envelope)
		// An empty-but-declared envelope must stay distinguishable from an
		// absent one; a non-nil zero-length slice carries that.
		if root.Envelope == nil {
			root.Envelope = []string{}
		}
	}

	return root, nil
}

// PlanFromRecords reconstructs the frozen plan from the log.
//
// FreezeAndRecordPhases writes one approval record per phase carrying that
// phase's index and full ceiling, so the plan's authority is entirely present
// in the log. That is what lets a reader with nothing but the records — the
// fork path in the operator, an offline auditor — rebuild what the gate was
// enforcing without holding the runner's in-memory copy.
//
// Only the LATEST plan is reconstructed: records for superseded plans are
// skipped, since one approved plan governs at a time.
//
// Labels and justifications are not recovered, and deliberately: they are
// display, they do not enter the digest, and inventing them would produce a
// plan that renders differently from the one a human approved. The returned
// plan is for AUTHORITY questions only.
func PlanFromRecords(records []plangateaudit.Content) (Plan, bool) {
	latest := ""
	for _, r := range records {
		if r.Event == plangateaudit.EventPlanApproved && r.PlanDigest != "" {
			latest = r.PlanDigest
		}
	}
	if latest == "" {
		return Plan{}, false
	}
	return PlanForDigest(records, latest)
}

// PlanForDigest rebuilds a SPECIFIC plan from the log, by digest.
//
// PlanFromRecords answers "what is in force"; this answers "what did that one
// say", which is the question a re-plan asks: deciding whether a new ceiling is
// covered by an approval a human already gave means reading the plan they gave
// it for, and that plan is by definition superseded.
//
// Same authority-only contract: labels and justifications are not recovered.
func PlanForDigest(records []plangateaudit.Content, digest string) (Plan, bool) {
	if digest == "" {
		return Plan{}, false
	}
	latest := digest

	phaseByIndex := map[int]plangateaudit.Content{}
	maxIdx := -1
	for _, r := range records {
		if r.Event != plangateaudit.EventPlanApproved || r.PlanDigest != latest || r.PhaseIndex == nil {
			continue
		}
		idx := int(*r.PhaseIndex)
		if idx < 0 {
			continue
		}
		phaseByIndex[idx] = r
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	if maxIdx < 0 {
		return Plan{}, false
	}

	out := Plan{Phases: make([]Phase, maxIdx+1)}
	for i := 0; i <= maxIdx; i++ {
		p := phaseByIndex[i]
		maxCount := p.MaxCount
		if maxCount < 1 {
			maxCount = 1
		}
		ph := Phase{
			Permissions: parseHandles(p.Ceiling),
			Max:         MaxSpec{Count: maxCount},
			Budget:      BudgetSpec{Calls: p.BudgetCalls},
		}
		for _, req := range p.Requires {
			ph.Requires = append(ph.Requires, RequiresEdge{Phase: req})
		}
		ph.Slots = slotsFromRecord(p)
		out.Phases[i] = ph
	}
	return out, true
}
