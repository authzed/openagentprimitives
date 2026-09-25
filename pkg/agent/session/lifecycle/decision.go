package lifecycle

// onTimeout controls what happens when a DecisionResolved arrives with TimedOut==true.
// The three values differ only in whether the session fails closed (scope_review) or
// continues (everything else). They are NOT used for human deny — a human deny is just
// Approved=false, TimedOut=false and always follows the same unpark path.
type onTimeout int

const (
	timeoutDeny       onTimeout = iota // tool_call, leakage_share, content_inspection: resume Running
	timeoutFail                        // scope_review: fail closed (cold-start gate cannot be bypassed)
	timeoutDenyExpire                  // join: deny + expire the request, return to Idle
)

// kindParams bundles the per-kind constants for the unified decision sub-machine.
// All five decision kinds share one path; they differ only in these parameters.
type kindParams struct {
	onTimeout onTimeout
	// preDecisionPhase is the phase to return to when the pending set empties after
	// a non-failing resolve. Running for everything except join, which stays Idle.
	preDecisionPhase Phase
	// setsScopeReview records the cold-start scope_review flag so the runner can
	// gate turn-0 execution until the scope is approved.
	setsScopeReview bool
}

var decisionParams = map[DecisionKind]kindParams{
	DecisionToolCall:       {onTimeout: timeoutDeny, preDecisionPhase: PhaseRunning},
	DecisionLeakageShare:   {onTimeout: timeoutDeny, preDecisionPhase: PhaseRunning},
	DecisionContentInspect: {onTimeout: timeoutDeny, preDecisionPhase: PhaseRunning},
	DecisionScopeReview:    {onTimeout: timeoutFail, preDecisionPhase: PhaseRunning, setsScopeReview: true},
	DecisionJoin:           {onTimeout: timeoutDenyExpire, preDecisionPhase: PhaseIdle},
}

// addPending appends a PendingDecision if it is not already present.
// Idempotent: if the requestID is already in the set (restart re-issue), returns s unchanged.
func addPending(s State, p PendingDecision) State {
	for _, e := range s.Pending {
		if e.RequestID == p.RequestID {
			return s // idempotent: restart re-issue safe
		}
	}
	s.Pending = append(append([]PendingDecision{}, s.Pending...), p)
	return s
}

// removePending removes the entry matching rid and returns the kind and a found flag.
// Returns the original State unmodified if rid is not found (late/duplicate event).
func removePending(s State, rid string) (State, DecisionKind, bool) {
	out := make([]PendingDecision, 0, len(s.Pending))
	var kind DecisionKind
	var found bool
	for _, e := range s.Pending {
		if e.RequestID == rid {
			kind, found = e.Kind, true
			continue
		}
		out = append(out, e)
	}
	s.Pending = out
	return s, kind, found
}

// FailsClosedOnTimeout reports whether a timed-out decision of kind k must fail
// the session closed rather than resume. It is the single source consulted when
// stamping pipeline.ApprovalAsk.OnTimeout, so the executor's timeout verdict and
// the session-phase projection share one authority.
func FailsClosedOnTimeout(k DecisionKind) bool {
	return decisionParams[k].onTimeout == timeoutFail
}

// anyScopeReview returns true if any entry in the pending set is a scope_review.
// Used to maintain the ScopeReviewPending flag accurately after partial resolves.
func anyScopeReview(s State) bool {
	for _, p := range s.Pending {
		if p.Kind == DecisionScopeReview {
			return true
		}
	}
	return false
}
