package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFoldReplayEquivalence(t *testing.T) {
	evs := []Event{
		SettingsAccepted{}, RunnerClaimed{},
		DecisionAsked{RequestID: "r1", Kind: DecisionToolCall},
		DecisionResolved{RequestID: "r1", Approved: true},
		TurnCompleted{},
	}
	a := Fold(evs)
	b := Fold(evs)
	assert.True(t, a.Equal(b), "fold is deterministic")
	assert.Equal(t, PhaseRunning, a.Phase)
	assert.Empty(t, a.Pending)
}

func TestFoldWithReissueReArmsOpenDecisions(t *testing.T) {
	// A runner died mid-approval: the log has the Ask but no Resolve.
	evs := []Event{
		SettingsAccepted{}, RunnerClaimed{},
		DecisionAsked{RequestID: "r1", Kind: DecisionContentInspect},
	}
	s, effs := FoldWithReissue(evs)
	assert.Equal(t, PhaseAwaitingDecision, s.Phase)
	var reissued bool
	for _, e := range effs {
		if r, ok := e.(ReissuePending); ok && len(r.Pending) == 1 && r.Pending[0].RequestID == "r1" {
			reissued = true
		}
	}
	assert.True(t, reissued, "open decision is re-armed on restart re-fold (kills strand)")
}
