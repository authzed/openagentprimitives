package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestContinuationDisposition pins the per-phase continuation mapping. The
// load-bearing property is totality: every phase (and every Failed reason
// class) resolves to a concrete disposition, so no inbound can fall into a
// "neither active nor archived" bucket and strand at "is starting…".
func TestContinuationDisposition(t *testing.T) {
	cases := []struct {
		name string
		s    State
		want Disposition
	}{
		{"pending routes to its own starting session", State{Phase: PhasePending}, DispResume},
		{"running routes to the live session", State{Phase: PhaseRunning}, DispResume},
		{"within-turn await_user_message resumes the live pod", State{Phase: PhaseRunning, AwaitingUserInput: true}, DispResume},
		{"idle respawns and resumes", State{Phase: PhaseIdle}, DispResume},
		{"awaiting-retry resumes on a plain message too", State{Phase: PhaseAwaitingRetry}, DispResume},
		{"awaiting-credentials resumes (append+reprompt)", State{Phase: PhaseAwaitingCredentials}, DispResume},
		{"mid-decision queues, never drops", State{Phase: PhaseAwaitingDecision}, DispQueue},
		{"awaiting-start-approval queues (parked pre-start; the pipeline's parked intercept answers)", State{Phase: PhaseAwaitingStartApproval}, DispQueue},
		{"zero-value normalizes to pending → resume", State{}, DispResume},

		{"archived-succeeded resumes the same session", State{Phase: PhaseSucceeded, Archived: true}, DispResume},
		{"agent-complete succeeded continues in a fresh inheriting session", State{Phase: PhaseSucceeded}, DispNewInheriting},

		{"boot bundle failure inherits into a fresh session", State{Phase: PhaseFailed, FailureReason: "BundleFailed"}, DispNewInheriting},
		{"sidecar boot failure inherits into a fresh session", State{Phase: PhaseFailed, FailureReason: "SidecarBootFailed"}, DispNewInheriting},
		{"transient memory outage inherits into a fresh session", State{Phase: PhaseFailed, FailureReason: "MemoryUnavailable"}, DispNewInheriting},
		{"credential-link timeout inherits into a fresh session", State{Phase: PhaseFailed, FailureReason: "CredentialLinkTimeout"}, DispNewInheriting},

		{"budget-failed refuses loudly (no silent starting…)", State{Phase: PhaseFailed, FailureReason: "BudgetExceeded"}, DispRefuse{Reason: "BudgetExceeded"}},
		{"toolguard-halt refuses loudly", State{Phase: PhaseFailed, FailureReason: "ToolGuardHalt"}, DispRefuse{Reason: "ToolGuardHalt"}},
		{"scope-review failure refuses loudly", State{Phase: PhaseFailed, FailureReason: "ScopeReviewFailed"}, DispRefuse{Reason: "ScopeReviewFailed"}},
		{"runner crash refuses loudly", State{Phase: PhaseFailed, FailureReason: "RunnerCrashed"}, DispRefuse{Reason: "RunnerCrashed"}},
		{"retry budget exhausted refuses loudly", State{Phase: PhaseFailed, FailureReason: "RetryBudgetExhausted"}, DispRefuse{Reason: "RetryBudgetExhausted"}},
		{"stopped refuses loudly", State{Phase: PhaseFailed, FailureReason: "Stopped"}, DispRefuse{Reason: "Stopped"}},
		{"unknown Failed reason refuses (fail-safe, no silent re-run)", State{Phase: PhaseFailed, FailureReason: "SomethingNew"}, DispRefuse{Reason: "SomethingNew"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ContinuationDisposition(tc.s))
		})
	}
}

// TestIsPolicyHalt pins which terminal Failed reasons are policy/security halts.
// A different-user takeover of a policy-halted session must start FRESH (never
// inherit the halted transcript); ordinary terminal states inherit. The load-
// bearing property is that RunnerCrash and ContentGuardHalt stay ordinary.
func TestIsPolicyHalt(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"ToolGuardHalt", true},
		{"ScopeReviewFailed", true},
		{"Stopped", true},
		{"NotAnAllowedStarter", true},
		{"CredentialLinkTimeout", false}, // ordinary: inherits
		{"SessionExpired", false},        // ordinary
		{"RunnerCrash", false},           // infra, not a policy decision
		{"ContentGuardHalt", false},      // keeps its transientBootFailures classification
		{"", false},                      // clean Succeeded
	}
	for _, tc := range cases {
		verdict := "ordinary"
		if tc.want {
			verdict = "policy-halt"
		}
		t.Run(tc.reason+"/"+verdict, func(t *testing.T) {
			assert.Equal(t, tc.want, IsPolicyHalt(tc.reason))
		})
	}
}
