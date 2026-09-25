package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTransitionLive(t *testing.T) {
	cases := []struct {
		name      string
		in        State
		ev        Event
		wantPhase Phase
		wantStop  bool
		wantGated int
	}{
		{"hook Halt → Failed AND stops the loop (Halt must stop)",
			State{Phase: PhaseRunning, Region: RegionRunner}, HookHalt{Reason: "tool_guard"}, PhaseFailed, true, 0},
		{"pre-deny: stays Running, surfaces Gated",
			State{Phase: PhaseRunning, Region: RegionRunner}, HookDeny{Post: false}, PhaseRunning, false, 1},
		{"post-deny: stays Running, surfaces Gated (side-effect already happened)",
			State{Phase: PhaseRunning, Region: RegionRunner}, HookDeny{Post: true}, PhaseRunning, false, 1},
		{"share-denied: yields to Idle, not Failed",
			State{Phase: PhaseRunning, Region: RegionRunner}, ShareDeniedYield{}, PhaseIdle, false, 0},
		{"work complete (kubectl): → Succeeded",
			State{Phase: PhaseRunning, Region: RegionRunner}, AgentWorkComplete{Kubectl: true}, PhaseSucceeded, false, 0},
		{"work complete (channel): → Idle",
			State{Phase: PhaseRunning, Region: RegionRunner}, AgentWorkComplete{Kubectl: false}, PhaseIdle, false, 0},
		{"await yield entered: stays Running (NOT Idle)",
			State{Phase: PhaseRunning, Region: RegionRunner}, AwaitYieldEntered{}, PhaseRunning, false, 0},
		{"await resumed: stays Running",
			State{Phase: PhaseRunning, Region: RegionRunner, AwaitingUserInput: true}, AwaitResumed{}, PhaseRunning, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, effs := Transition(tc.in.OrDefault(), tc.ev)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, tc.wantGated, got.Gated)
			// phases returns (append, project, stop, notify); we only need stop here.
			_, _, stop, _ := phases(effs)
			assert.Equal(t, tc.wantStop, stop, "StopLoop effect")
		})
	}

	// await_user_message is a within-Running sub-state (pod stays alive), not Idle (pod exits).
	t.Run("await yield sets the within-Running flag; resume clears it; phase never changes", func(t *testing.T) {
		y, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(), AwaitYieldEntered{})
		assert.True(t, y.AwaitingUserInput)
		assert.Equal(t, PhaseRunning, y.Phase, "await is within-Running, NOT Idle")
		r, _ := Transition(y, AwaitResumed{})
		assert.False(t, r.AwaitingUserInput)
		assert.Equal(t, PhaseRunning, r.Phase)
	})

	t.Run("Slept Idle + WakeRequested: Pending, Slept cleared", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseIdle, Slept: true}, WakeRequested{})
		assert.Equal(t, PhasePending, got.Phase)
		assert.False(t, got.Slept, "wake clears the slept marker so provisioning resumes")
	})
}
