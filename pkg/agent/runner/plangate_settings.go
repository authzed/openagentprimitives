package runner

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The plan gate's resolved-settings readers.
//
// They live HERE, exported, rather than in cmd/runner, because the e2e
// in-process factory needs the identical answers and cannot import package
// main. Keeping a private copy in each is how the harness silently diverged
// from production: PlanGateMaxAutoApprove was wired in cmd/runner and simply
// absent from the factory, so every e2e and bronze scenario ran with a tier-0
// budget of ZERO — meaning tier 0 never auto-approved anything, every phase
// looked like it needed a human, and the gradient the tier exists to create was
// untested end to end while both suites stayed green.
//
// That is the same shape as the plans→plangate projection that existed twice
// with a comment promising the copies were kept in lockstep. A mirror is a
// promise nothing enforces.

// defaultMaxAutoApprove mirrors the CRD marker on
// PlanRendering.MaxAutoApproveHandles. A kubebuilder default only applies once
// the Rendering object EXISTS, so an absent block would otherwise read as 0 —
// which disables tier 0 entirely and makes even a read-only recon phase cost a
// human. That is the opposite of what the tier exists for.
const defaultMaxAutoApprove = 8

// PlanGateMaxCardHandles reads the Routine-card fold threshold. Zero (no
// rendering block) means no folding, which is the safe direction: an over-long
// card is a legibility problem, while a silently folded one hides authority an
// approver is granting.
func PlanGateMaxCardHandles(es *spiceboxv1alpha1.EffectiveSettings) int {
	if es == nil || es.Authz.PlanGate == nil || es.Authz.PlanGate.Rendering == nil {
		return 0
	}
	return int(es.Authz.PlanGate.Rendering.MaxSingleCardHandles)
}

// PlanGateMaxAutoApprove reads the session-cumulative tier-0 budget.
func PlanGateMaxAutoApprove(es *spiceboxv1alpha1.EffectiveSettings) int {
	if es == nil || es.Authz.PlanGate == nil || es.Authz.PlanGate.Rendering == nil {
		return defaultMaxAutoApprove
	}
	if n := int(es.Authz.PlanGate.Rendering.MaxAutoApproveHandles); n > 0 {
		return n
	}
	return defaultMaxAutoApprove
}

// PlanGateRequirePlan resolves whether a plan is required before any
// permissioned call runs.
//
// UNSET derives from the mode: required whenever the gate runs. Turning the gate
// on is the intent to make agents plan, and an independent opt-in defaulting
// false let a cluster run `mode: logging`, look gated, and collect nothing.
//
// Explicit false is honoured as the observe-only rollout stage. Explicit true is
// NOT honoured under a disabled gate: the hook never evaluates, so promising a
// denial there would be a lie the code cannot keep.
func PlanGateRequirePlan(es *spiceboxv1alpha1.EffectiveSettings) bool {
	if es == nil || es.Authz.PlanGate == nil {
		return false
	}
	pg := es.Authz.PlanGate
	if pg.Mode == "" || pg.Mode == ToolAuthModeDisabled {
		return false
	}
	if pg.RequirePlan != nil {
		return *pg.RequirePlan
	}
	return true
}
