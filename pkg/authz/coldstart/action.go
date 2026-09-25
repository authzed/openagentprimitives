// Package coldstart holds the cold-start approval action constants and the
// approval.Decision → action mapping, in an importable package so both the
// authzd pipeline Host (pkg/authz/authzd/pipelinehost) and the cold-start hook can
// reference them without importing internal/cmd/authzd (package main). Pure mapping; no
// behavior change from the original internal/cmd/authzd location.
package coldstart

import "github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"

// The five cold-start approver actions. The channel reports the clicked button
// as approval.Decision.Action; ActionForDecision maps it onto one of these.
const (
	ActionApproveCleaned  = "approve_cleaned"
	ActionApproveOriginal = "approve_original"
	ActionRunWithoutScope = "run_without_scope"
	ActionDeny            = "deny"
)

// ActionForDecision maps an approval.Decision to one of the cold-start actions.
// When the channel reported an explicit Action (the cold-start block's buttons
// all carry one), that action wins. Otherwise the boolean is mapped: an approve
// → ActionApproveCleaned, a deny → ActionDeny. Pure so the mapping is
// unit-testable without a live NATS Await.
func ActionForDecision(dec approval.Decision) string {
	switch dec.Action {
	case ActionApproveCleaned, ActionApproveOriginal, ActionRunWithoutScope, ActionDeny:
		return dec.Action
	}
	// Fallback (no explicit action, e.g. a mid-session-style approve/deny):
	// map the bool.
	if dec.Approved {
		return ActionApproveCleaned
	}
	return ActionDeny
}
