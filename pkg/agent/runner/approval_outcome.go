package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
)

// approvalFailureContent builds the tool-result text the agent sees when a
// per-tool approval flow does NOT proceed (timeout, explicit deny, or a
// transport-level error inside Orchestrator.Await).
//
// Every branch leads with a structured sentinel (SYSTEM_TIMEOUT: /
// SYSTEM_DECISION_DENIED: / SYSTEM_APPROVAL_ERROR:) that the runner's system
// prompt teaches the LLM to match on. Prose alone does not hold: production saw
// the LLM confabulate "Alice denied the request" for a timeout whose message
// already said "TIMEOUT, not a denial". For the same reason the timeout branch
// never says "approver" — naming a role primes the model to fill in a person.
//
// d is the decision delivered by the orchestrator (zero-value when the await
// ended via ctx cancellation); awaitErr is nil when a decision was delivered,
// else a context-deadline timeout or a transport failure inside OnPublish.
func approvalFailureContent(toolName string, d approval.Decision, awaitErr error) string {
	// Timeout has TWO source paths, both of which MUST read as SYSTEM_TIMEOUT:
	//   (a) Local ctx-deadline: Await returned ctx.Err() because the per-await
	//       timer expired before any decision landed. The only shape an
	//       in-process await's lapse ever takes.
	//   (b) A DELIVERED "timeout" decision: a producer published an Applied
	//       envelope with Decision="deny" + Reason="timeout" that DeliverDecision
	//       routed here, so Await returns Approved:false with awaitErr == nil.
	//       Cross-process / redelivery only (e.g. a replay into a re-folding
	//       runner). Falling through to !d.Approved below would frame it as
	//       SYSTEM_DECISION_DENIED and the LLM would name an owner who never acted.
	if isTimeoutSignal(d, awaitErr) {
		return fmt.Sprintf(
			"SYSTEM_TIMEOUT: the approval workflow for tool %q reached its deadline with no decision. This is a TIMEOUT — the system expired the request. No person rejected or approved it; nobody acted on it. When responding to the user, describe this as the approval request expired without a response. Do NOT name any specific person as having denied the request. Do NOT use the words denied or rejected when describing a SYSTEM_TIMEOUT. You may offer to retry.",
			toolName,
		)
	}
	if awaitErr != nil {
		return fmt.Sprintf(
			"SYSTEM_APPROVAL_ERROR: the approval workflow for tool %q failed to publish: %v. No person was ever asked. Surface this as an internal error to the user; this is NOT a denial.",
			toolName, awaitErr,
		)
	}
	if !d.Approved {
		approver := d.ApproverID
		if approver == "" {
			approver = "(approver id not recorded)"
		}
		return fmt.Sprintf(
			"SYSTEM_DECISION_DENIED: the approval for tool %q was DENIED by %s. The approver explicitly rejected the request. Tell the user the request was denied; this is NOT a timeout.",
			toolName, approver,
		)
	}
	// Defensive: Approved=true with no awaitErr shouldn't reach this
	// helper, but if it does the message names that contradiction.
	return fmt.Sprintf("approval flow internal error for tool %q: helper called on an approved decision; this is a bug.", toolName)
}

// isTimeoutSignal reports whether either source path (local ctx-deadline
// OR a remote Applied envelope with Reason="timeout") indicates a
// timeout. Both must route to the SYSTEM_TIMEOUT branch — see the
// caller's comment for the regression.
func isTimeoutSignal(d approval.Decision, awaitErr error) bool {
	if awaitErr != nil && (errors.Is(awaitErr, context.DeadlineExceeded) || errors.Is(awaitErr, context.Canceled)) {
		return true
	}
	return d.Reason == "timeout"
}
