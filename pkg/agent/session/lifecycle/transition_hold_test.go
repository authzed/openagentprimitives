package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransition_Held(t *testing.T) {
	cases := []struct {
		name  string
		start Phase
		want  Phase
	}{
		{name: "Pending: Held parks the session", start: PhasePending, want: PhaseHeld},
		{name: "Running: Held parks the session", start: PhaseRunning, want: PhaseHeld},
		{name: "Idle: Held parks the session", start: PhaseIdle, want: PhaseHeld},
		{name: "AwaitingDecision: Held parks the session", start: PhaseAwaitingDecision, want: PhaseHeld},
		{name: "AwaitingRetry: Held parks the session", start: PhaseAwaitingRetry, want: PhaseHeld},
		{name: "Succeeded is sticky: Held does not move it", start: PhaseSucceeded, want: PhaseSucceeded},
		{name: "Failed is sticky: Held does not move it", start: PhaseFailed, want: PhaseFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, effects := Transition(State{Phase: tc.start}, Held{Reason: "denial-streak", TrippedBy: "user:alice", At: "2026-08-24T00:00:00Z"})
			assert.Equal(t, tc.want, got.Phase)
			require.NotEmpty(t, effects, "every transition logs")
		})
	}
}

func TestTransition_Released(t *testing.T) {
	cases := []struct {
		name  string
		start Phase
		want  Phase
	}{
		{name: "Held: Released returns the session to Pending", start: PhaseHeld, want: PhasePending},
		{name: "Running: Released is not accepted, phase unchanged", start: PhaseRunning, want: PhaseRunning},
		{name: "Idle: Released is not accepted, phase unchanged", start: PhaseIdle, want: PhaseIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Transition(State{Phase: tc.start}, Released{ApprovedBy: "user:alice", At: "2026-08-24T01:00:00Z"})
			assert.Equal(t, tc.want, got.Phase)
		})
	}
}

func TestTransition_Released_doesNotMarkFailed(t *testing.T) {
	held, _ := Transition(State{Phase: PhaseRunning}, Held{Reason: "denial-streak", At: "2026-08-24T00:00:00Z"})
	released, _ := Transition(held, Released{ApprovedBy: "user:alice", At: "2026-08-24T01:00:00Z"})
	assert.Equal(t, PhasePending, released.Phase)
	assert.Empty(t, released.FailureReason, "a released session is not failed")
}
