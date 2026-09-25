package lifecycle

// continuation.go answers a single question: when a new in-thread message
// arrives for a session that is already in some state, what should happen to
// it? This is the pure classifier that channelsd (and the operator's
// respawn/terminal guards) consult instead of an ad-hoc active/archived
// switch. Making it total over every phase is the whole point — a phase that
// falls into neither bucket is exactly how an inbound message strands at
// "is starting…" with no runner ever spawned.

// Disposition is the sealed result of classifying a continuation. The three
// singleton dispositions are package vars; DispRefuse carries the failure
// reason so the caller can explain why the session ended.
type Disposition interface{ isDisposition() }

// simpleDisposition backs the three reason-free dispositions. It is a distinct
// string type so equality is value equality (used directly by tests via
// assert.Equal against the exported vars).
type simpleDisposition string

func (simpleDisposition) isDisposition() {}

const (
	dispResume        simpleDisposition = "resume"
	dispQueue         simpleDisposition = "queue"
	dispNewInheriting simpleDisposition = "new_inheriting"
)

var (
	// DispResume: append the inbound and wake/route the same session. Its pod
	// is alive (Running/AwaitingDecision-adjacent) or the operator respawns it
	// from a parked phase (Idle/AwaitingRetry/AwaitingCredentials).
	DispResume Disposition = dispResume
	// DispQueue: a decision is mid-flight (AwaitingDecision). Append the
	// inbound so it is not dropped; the runner consumes it once the decision
	// resolves. Never route it into a tool or drop it silently.
	DispQueue Disposition = dispQueue
	// DispNewInheriting: the session is finished (Succeeded) or failed in a
	// way a clean retry can recover from. The continuation belongs in a fresh
	// session that inherits this one's history — never the terminal CR itself.
	DispNewInheriting Disposition = dispNewInheriting
)

// DispRefuse: the session failed in a way that a plain retry cannot fix
// (budget exhausted, a policy halt, a scope-review failure, a crashed
// runner). The caller posts a loud, user-visible notice naming Reason rather
// than silently routing the message into a dead session.
type DispRefuse struct{ Reason string }

func (DispRefuse) isDisposition() {}

// transientBootFailures are Failed reasons a fresh session can recover from:
// startup failures and transient dependency outages. A continuation for one of
// these inherits history into a new session; everything else in Failed refuses,
// because the fail-safe default is to explain loudly rather than silently re-run
// into the same wall.
//
// The strings mirror the ReasonAgentSession* constants in
// pkg/apis/v1alpha1/conditions.go, reproduced as literals because this package
// is deliberately free of k8s imports (imports_test.go).
var transientBootFailures = map[string]struct{}{
	"BundleFailed":              {},
	"SidecarBootFailed":         {},
	"SidecarToolboxMissing":     {},
	"SidecarToolboxInvalid":     {},
	"ContentGuardHalt":          {},
	"FederatedIdPSecretMissing": {},
	"CredentialLinkTimeout":     {},
	"MemoryUnavailable":         {},
}

// policyHaltReasons are terminal Failed reasons representing a deliberate policy
// or security decision to stop the session. A different-user takeover of such a
// session must start FRESH and never inherit the halted transcript. RunnerCrash
// (infra) and ContentGuardHalt (classified above as transient) are deliberately
// excluded — they are ordinary, inheriting terminal states.
//
// The strings mirror the FailureReason values set in transition.go. Like
// transientBootFailures this is a set you add a row to, not a branch a consumer
// switches on.
var policyHaltReasons = map[string]struct{}{
	"ToolGuardHalt":       {},
	"ScopeReviewFailed":   {},
	"Stopped":             {},
	"ForensicHold":        {},
	"NotAnAllowedStarter": {},
}

// IsPolicyHalt reports whether a terminal FailureReason is a policy/security
// halt whose transcript must not be inherited by a different-user takeover.
func IsPolicyHalt(reason string) bool {
	_, ok := policyHaltReasons[reason]
	return ok
}

// ContinuationDisposition classifies a new inbound message against the
// session's current State. It is pure, total, and deterministic: every phase
// maps to exactly one disposition, so no inbound can land in a "neither"
// bucket and strand.
func ContinuationDisposition(s State) Disposition {
	s = s.OrDefault()
	switch s.Phase {
	case PhasePending, PhaseRunning, PhaseIdle, PhaseAwaitingRetry, PhaseAwaitingCredentials:
		// Live or respawnable. Running also covers the within-turn
		// await_user_message yield (AwaitingUserInput flag): the pod stays
		// alive and a reply simply resumes it.
		return DispResume
	case PhaseAwaitingDecision:
		// A human-in-the-loop decision is outstanding. Queue the message; the
		// runner reads it after the decision resolves.
		return DispQueue
	case PhaseAwaitingStartApproval:
		// Parked pre-start on an admin's start-approval decision. Queue-shaped
		// (a decision is outstanding; never wake, never drop) — in practice
		// the pipeline's parked intercept answers every inbound before any
		// append, since nobody holds standing on the session yet.
		return DispQueue
	case PhaseSucceeded:
		if s.Archived {
			// Swept while idle, not finished. The user is still in the thread
			// and expects to continue; respawn this same session.
			return DispResume
		}
		// Finished cleanly. A follow-up continues in a fresh session that
		// inherits this one's transcript.
		return DispNewInheriting
	case PhaseFailed:
		if _, ok := transientBootFailures[s.FailureReason]; ok {
			return DispNewInheriting
		}
		return DispRefuse{Reason: s.FailureReason}
	case PhaseHeld:
		// A hold is a containment decision a human has not yet lifted. Resuming
		// on an inbound message would let anyone in the thread undo it by
		// typing, and queueing would strand the message behind a decision that
		// may never come. Refuse, and let the release card be the only way back.
		return DispRefuse{Reason: "ForensicHold"}
	default:
		// Unreachable for the closed phase set above; classify defensively as
		// Resume so an unexpected phase routes to its own session rather than
		// stranding the message with no destination.
		return DispResume
	}
}
