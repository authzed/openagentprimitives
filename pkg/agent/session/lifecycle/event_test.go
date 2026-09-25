package lifecycle

import "testing"

func TestEventsSealed(t *testing.T) {
	// Compile-time proof every event implements Event.
	evs := []Event{
		SettingsAccepted{}, PodReady{}, Unschedulable{}, ProvablyUnschedulable{},
		RunnerPodRefused{Message: "quota"},
		CredsMissing{}, CredsLinked{}, CredsTimeout{},
		IdentityChoicePending{}, IdentityChoiceResolved{Mode: "agent"},
		IdentityChoiceCancelled{}, IdentityChoiceTimeout{},
		RunnerClaimed{}, RunnerTerminal{Phase: PhaseSucceeded}, RunnerCrash{}, Stopped{},
		TurnCompleted{}, AgentWorkComplete{Kubectl: true},
		HookDeny{Post: true}, HookHalt{Reason: "tool_guard"},
		DecisionAsked{RequestID: "r1", Kind: DecisionToolCall},
		DecisionResolved{RequestID: "r1", Approved: true},
		ProviderError{}, RetryRequested{}, RetryTTLExpired{}, WakeRequested{},
		ArchiveSweep{}, Sleep{}, AwaitYieldEntered{}, AwaitResumed{}, IdleYield{}, ShareDeniedYield{},
		Revoked{}, ScopeMutated{}, RestartRequested{}, Expired{},
		Held{Reason: "manual review"}, Released{ApprovedBy: "canonical-user"},
	}
	if len(evs) == 0 {
		t.Fatal("unreachable")
	}
}
