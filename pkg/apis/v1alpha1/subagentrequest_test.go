package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSubagentRequest_IsTerminal(t *testing.T) {
	cases := []struct {
		phase string
		want  bool
	}{
		{SubagentRequestPhasePending, false},
		{SubagentRequestPhaseRunning, false},
		{SubagentRequestPhaseSucceeded, true},
		{SubagentRequestPhaseFailed, true},
		{SubagentRequestPhaseDenied, true},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			r := SubagentRequest{Status: SubagentRequestStatus{Phase: tc.phase}}
			assert.Equal(t, tc.want, r.IsTerminal())
		})
	}
}

func TestSubagentMode_Attended_Registered(t *testing.T) {
	assert.True(t, IsSubagentMode(SubagentModeAttended))
	assert.Contains(t, SubagentModesAll(), SubagentModeAttended)
	sr := &SubagentRequest{Spec: SubagentRequestSpec{Mode: SubagentModeAttended}}
	assert.True(t, sr.PermitsHumanInitiative(), "an attended child is human-directed")
}

func TestSubagentRequest_ExchangeBudget(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		wantLimit   int64
		wantBounded bool
	}{
		{"single_turn: headless, bounded at zero", SubagentModeSingleTurn, 0, true},
		{"task: one bounded question", SubagentModeTask, 1, true},
		{"chat: unbounded parent conversation", SubagentModeChat, 0, false},
		{"attended: unbounded human conversation, same shape as chat", SubagentModeAttended, 0, false},
		{"unrecognized mode: bounded at zero, fail closed", "conversation", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := SubagentRequest{Spec: SubagentRequestSpec{Mode: tc.mode}}
			limit, bounded := r.ExchangeBudget()
			assert.Equal(t, tc.wantLimit, limit)
			assert.Equal(t, tc.wantBounded, bounded)
		})
	}
}

func TestSubagentRequest_PermitsHumanInitiative(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"single_turn: no surface to be addressed on", SubagentModeSingleTurn, false},
		{"task: may ask, may not be driven", SubagentModeTask, false},
		{"chat: a person may open a turn", SubagentModeChat, true},
		{"attended: human-directed by definition", SubagentModeAttended, true},
		{"unrecognized mode: fail closed", "conversation", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := SubagentRequest{Spec: SubagentRequestSpec{Mode: tc.mode}}
			assert.Equal(t, tc.want, r.PermitsHumanInitiative())
		})
	}
}
