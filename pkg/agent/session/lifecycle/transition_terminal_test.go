package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// hasMarkPlanStopped is a sentinel for interrupted-termination: the runner sequencer
// must emit MarkPlanStopped to close the agent's plans state-kind on crash/SIGTERM.
// Clean completion (Succeeded, Idle archive) must NOT emit it.
func hasMarkPlanStopped(effs []Effect) bool {
	for _, e := range effs {
		if _, ok := e.(MarkPlanStopped); ok {
			return true
		}
	}
	return false
}

func TestTransitionTerminal(t *testing.T) {
	t.Run("provider error: → AwaitingRetry, attempts++", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(), ProviderError{})
		assert.Equal(t, PhaseAwaitingRetry, got.Phase)
		assert.Equal(t, 1, got.RetryAttempts)
	})
	t.Run("retry budget exhausted: ProviderError on attempt maxRetry+1 → Failed[RetryBudgetExhausted]", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner, RetryAttempts: maxRetry}.OrDefault(), ProviderError{})
		assert.Equal(t, PhaseFailed, got.Phase)
		assert.Equal(t, "RetryBudgetExhausted", got.FailureReason)
	})
	t.Run("retry TTL elapsed without RetryRequested → Failed[RetryTimeout]", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseAwaitingRetry, Region: RegionOperatorPost}.OrDefault(), RetryTTLExpired{})
		assert.Equal(t, PhaseFailed, got.Phase)
		assert.Equal(t, "RetryTimeout", got.FailureReason)
	})
	t.Run("retry requested: AwaitingRetry → Pending", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseAwaitingRetry, Region: RegionOperatorPost}.OrDefault(), RetryRequested{})
		assert.Equal(t, PhasePending, got.Phase)
		assert.Equal(t, RegionOperatorPre, got.Region)
	})
	t.Run("crash backstop clears pending decisions (kills strand)", func(t *testing.T) {
		in := State{Phase: PhaseAwaitingDecision, Region: RegionRunner,
			Pending: []PendingDecision{{RequestID: "r1", Kind: DecisionContentInspect}}}
		got, effs := Transition(in.OrDefault(), RunnerCrash{})
		assert.Empty(t, got.Pending, "backstop clears pending so nothing strands")
		assert.Equal(t, PhaseFailed, got.Phase)
		assert.True(t, hasMarkPlanStopped(effs), "crash also marks the plan stopped on interrupted termination")
	})
	// Interrupted termination marks the agent's plan stopped; clean completion does not.
	t.Run("Stopped → Failed AND MarkPlanStopped (interrupted termination closes plan items)", func(t *testing.T) {
		got, effs := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(), Stopped{})
		assert.Equal(t, PhaseFailed, got.Phase)
		assert.True(t, hasMarkPlanStopped(effs))
	})
	t.Run("terminal Failed (RunnerTerminal) emits MarkPlanStopped", func(t *testing.T) {
		_, effs := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(),
			RunnerTerminal{Phase: PhaseFailed, Reason: "Budget"})
		assert.True(t, hasMarkPlanStopped(effs))
	})
	t.Run("clean Succeeded does NOT emit MarkPlanStopped", func(t *testing.T) {
		_, effs := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(),
			RunnerTerminal{Phase: PhaseSucceeded})
		assert.False(t, hasMarkPlanStopped(effs), "clean completion leaves plan items done")
	})
	t.Run("runner terminal: phase set to Idle, region=OperatorPost", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(),
			RunnerTerminal{Phase: PhaseIdle})
		assert.Equal(t, PhaseIdle, got.Phase)
		assert.Equal(t, RegionOperatorPost, got.Region)
	})
	t.Run("archive sweep: Idle → Succeeded", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseIdle, Region: RegionOperatorPost}.OrDefault(), ArchiveSweep{})
		assert.Equal(t, PhaseSucceeded, got.Phase)
	})
	t.Run("archive sweep on non-Idle: phase unchanged", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(), ArchiveSweep{})
		assert.Equal(t, PhaseRunning, got.Phase)
	})
	t.Run("Idle + Sleep: stays Idle, Slept=true", func(t *testing.T) {
		got, effects := Transition(State{Phase: PhaseIdle}, Sleep{})
		assert.Equal(t, PhaseIdle, got.Phase)
		assert.True(t, got.Slept, "Sleep marks the session slept")
		assert.NotEmpty(t, effects, "Sleep still projects + logs")
	})
	t.Run("Running + Sleep: stale sweep is a no-op", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning}, Sleep{})
		assert.Equal(t, PhaseRunning, got.Phase)
		assert.False(t, got.Slept)
	})
	t.Run("revocation is in-flight: no phase change", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(), Revoked{})
		assert.Equal(t, PhaseRunning, got.Phase)
	})
	t.Run("scope mutated is in-flight: no phase change", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(), ScopeMutated{})
		assert.Equal(t, PhaseRunning, got.Phase)
	})
	t.Run("restart requested is in-flight: no phase change", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(), RestartRequested{})
		assert.Equal(t, PhaseRunning, got.Phase)
	})
	t.Run("expired: live Running → Failed[SessionExpired]", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(), Expired{})
		assert.Equal(t, PhaseFailed, got.Phase)
		assert.Equal(t, "SessionExpired", got.FailureReason)
	})
	t.Run("expired: Idle → Failed[SessionExpired]", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseIdle, Region: RegionOperatorPost}.OrDefault(), Expired{})
		assert.Equal(t, PhaseFailed, got.Phase)
		assert.Equal(t, "SessionExpired", got.FailureReason)
	})
	t.Run("expired on already-terminal (Succeeded): sticky, untouched", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseSucceeded}.OrDefault(), Expired{})
		assert.Equal(t, PhaseSucceeded, got.Phase)
	})
}
