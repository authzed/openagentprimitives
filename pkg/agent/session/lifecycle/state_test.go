package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestZeroStateIsPending(t *testing.T) {
	var s State
	assert.Equal(t, PhasePending, s.OrDefault().Phase, "zero State defaults to Pending")
}

func TestStateEqualIgnoresNothing(t *testing.T) {
	a := State{Phase: PhaseRunning, Pending: []PendingDecision{{RequestID: "r1", Kind: DecisionToolCall}}}
	b := State{Phase: PhaseRunning, Pending: []PendingDecision{{RequestID: "r1", Kind: DecisionToolCall}}}
	assert.True(t, a.Equal(b), "identical states are Equal")
	b.Pending[0].Kind = DecisionLeakageShare
	assert.False(t, a.Equal(b), "differing decision kind is not Equal")
}

func TestStateEqualArchivedBit(t *testing.T) {
	a := State{Phase: PhaseRunning, Pending: []PendingDecision{{RequestID: "r1", Kind: DecisionToolCall}}}
	b := State{Phase: PhaseRunning, Pending: []PendingDecision{{RequestID: "r1", Kind: DecisionToolCall}}}
	assert.True(t, a.Equal(b), "identical states are Equal")
	b.Archived = true
	assert.False(t, a.Equal(b), "differing Archived: not equal")
}
