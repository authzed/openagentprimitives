// Package lifecycle is the pure, I/O-free session state machine. It imports
// no k8s, NATS, SpiceDB, memory, or time package; time-dependent transitions
// arrive as events from a caller that owns a clock. See imports_test.go.
package lifecycle

// Phase is the externally-meaningful top-level state, projected to
// AgentSession.status.phase.
type Phase string

const (
	PhasePending             Phase = "Pending"
	PhaseAwaitingCredentials Phase = "AwaitingCredentials"
	PhaseRunning             Phase = "Running"
	PhaseAwaitingDecision    Phase = "AwaitingDecision"
	PhaseIdle                Phase = "Idle"
	PhaseAwaitingRetry       Phase = "AwaitingRetry"
	PhaseSucceeded           Phase = "Succeeded"
	PhaseFailed              Phase = "Failed"
	// PhaseHeld is a forensic hold: the session is parked with its workspace
	// preserved, pending a human decision to release it. Distinct from Failed
	// (which is terminal and un-releasable) and from Idle (which any inbound
	// message resumes). Entered by Held, left only by Released.
	PhaseHeld Phase = "Held"
	// PhaseAwaitingIdentityChoice parks an identityMode=ask|dynamic session while
	// the user picks agent|userPassthrough. Its string value MUST stay
	// byte-identical to v1alpha1.AgentSessionPhaseAwaitingIdentityChoice — this
	// package does not import v1alpha1 (it stays I/O-free), so the two constants
	// are kept in sync by convention, pinned by tests on both sides.
	PhaseAwaitingIdentityChoice = "AwaitingIdentityChoice"
	// PhaseAwaitingStartApproval parks a session an org non-member started
	// until a platform admin approves or denies it; no runner exists and no
	// standing is written while parked. Same byte-identical-by-convention
	// contract with v1alpha1.AgentSessionPhaseAwaitingStartApproval as the
	// constant above.
	PhaseAwaitingStartApproval Phase = "AwaitingStartApproval"
)

// DecisionKind enumerates the five human-in-the-loop decision kinds that share
// one sub-machine. They differ only by the parameters in decisionParams.
type DecisionKind string

const (
	DecisionToolCall       DecisionKind = "tool_call"
	DecisionLeakageShare   DecisionKind = "leakage_share"
	DecisionContentInspect DecisionKind = "content_inspection"
	DecisionScopeReview    DecisionKind = "scope_review"
	DecisionJoin           DecisionKind = "join"
)

// PendingDecision is one outstanding decision in the AwaitingDecision set.
type PendingDecision struct {
	RequestID string
	Kind      DecisionKind
}

// State is the complete folded lifecycle state. It is a value type; transitions
// return a new State.
type State struct {
	Phase Phase
	// Pending holds every outstanding decision. A non-empty set means the
	// projection derives AwaitingDecision (unless masked by a terminal phase).
	Pending []PendingDecision
	// Region records which authority owns the projection writer right now.
	Region Region
	// FailureReason is set when Phase==Failed.
	FailureReason string
	// Archived is true when Phase==Succeeded was reached by the operator's
	// long-idle archive sweep rather than by the agent finishing. The sweep
	// records this as a condition reason on the CR, not as FailureReason
	// (which it deliberately clears), so callers must set this bit explicitly.
	// A swept session is parked, not done: a follow-up resumes it.
	Archived bool
	// RetryAttempts counts AwaitingRetry entries (capped; budget exhaustion transitions to Failed[RetryBudgetExhausted]).
	RetryAttempts int
	// Gated counts hook-denied tool calls surfaced to status (no-silent-errors).
	Gated int
	// ScopeReviewPending is true while a cold-start scope_review decision blocks turn-0.
	ScopeReviewPending bool
	// AwaitingUserInput marks a within-Running await_user_message yield: the pod stays
	// alive and phase stays Running (distinct from Idle, where the pod exits). Projects to
	// the durable operator-classified-but-runner-written awaitingUserInputSince scalar.
	AwaitingUserInput bool
	// Slept marks an Idle session whose pods have been reaped (scaled to zero).
	// Set by the Sleep event, cleared by WakeRequested. The session stays Idle
	// and wakeable; the reconciler re-provisions pods lazily on wake.
	Slept bool
	// EffectiveIdentityMode is set by IdentityChoiceResolved; empty until then.
	EffectiveIdentityMode string
}

// Region is the authority partition; exactly one holder writes the projection.
type Region string

const (
	RegionOperatorPre  Region = "operator_pre"
	RegionRunner       Region = "runner"
	RegionOperatorPost Region = "operator_post"
)

// OrDefault returns a State whose zero Phase is normalized to Pending.
func (s State) OrDefault() State {
	if s.Phase == "" {
		s.Phase = PhasePending
	}
	if s.Region == "" {
		s.Region = RegionOperatorPre
	}
	return s
}

// Equal is a deep, order-sensitive comparison (Pending order is canonical:
// append-only by requestID).
func (s State) Equal(o State) bool {
	if s.Phase != o.Phase || s.Region != o.Region || s.FailureReason != o.FailureReason ||
		s.RetryAttempts != o.RetryAttempts || s.Gated != o.Gated || s.ScopeReviewPending != o.ScopeReviewPending ||
		s.AwaitingUserInput != o.AwaitingUserInput || s.Slept != o.Slept ||
		s.EffectiveIdentityMode != o.EffectiveIdentityMode || s.Archived != o.Archived {
		return false
	}
	if len(s.Pending) != len(o.Pending) {
		return false
	}
	for i := range s.Pending {
		if s.Pending[i] != o.Pending[i] {
			return false
		}
	}
	return true
}
