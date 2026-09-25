package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentityChoiceTransitions(t *testing.T) {
	cases := []struct {
		name      string
		from      State
		event     Event
		wantPhase Phase // typed to match got.Phase: assert.Equal uses reflect.DeepEqual,
		// which treats string and Phase as distinct types even for equal values.
		wantMode   string
		wantReason string
	}{
		{"pending→park: IdentityChoicePending ⇒ AwaitingIdentityChoice",
			State{Phase: PhasePending}, IdentityChoicePending{}, PhaseAwaitingIdentityChoice, "", ""},
		{"park→agent: Resolved{agent} ⇒ Pending, E=agent",
			State{Phase: PhaseAwaitingIdentityChoice}, IdentityChoiceResolved{Mode: "agent"}, PhasePending, "agent", ""},
		{"park→passthrough: Resolved{userPassthrough} ⇒ Pending, E=userPassthrough",
			State{Phase: PhaseAwaitingIdentityChoice}, IdentityChoiceResolved{Mode: "userPassthrough"}, PhasePending, "userPassthrough", ""},
		{"park→cancel: Cancelled ⇒ Failed/IdentityChoiceCancelled",
			State{Phase: PhaseAwaitingIdentityChoice}, IdentityChoiceCancelled{}, PhaseFailed, "", "IdentityChoiceCancelled"},
		{"park→timeout: Timeout ⇒ Failed/IdentityChoiceTimeout",
			State{Phase: PhaseAwaitingIdentityChoice}, IdentityChoiceTimeout{}, PhaseFailed, "", "IdentityChoiceTimeout"},
		{"terminal-sticky: Resolved after Failed ⇒ unchanged",
			State{Phase: PhaseFailed, FailureReason: "x"}, IdentityChoiceResolved{Mode: "agent"}, PhaseFailed, "", "x"},
		{"terminal-sticky: Resolved after Succeeded ⇒ unchanged",
			State{Phase: PhaseSucceeded}, IdentityChoiceResolved{Mode: "agent"}, PhaseSucceeded, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, effects := Transition(tc.from, tc.event)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, tc.wantMode, got.EffectiveIdentityMode)
			assert.Equal(t, tc.wantReason, got.FailureReason)
			// Every non-terminal-sticky transition emits AppendLog + ProjectStatus.
			if tc.from.Phase != PhaseSucceeded && tc.from.Phase != PhaseFailed {
				assert.Len(t, effects, 2)
			}
		})
	}
}

// TestIdentityChoiceResolvedDoesNotWhitelistMode documents, with a real
// assertion, that the fold copies Mode verbatim and does not validate it
// against the known "agent" | "userPassthrough" values. Mode validation is
// the emit site's responsibility (the runner only ever constructs
// IdentityChoiceResolved with one of the two valid strings) — Transition
// stays a total, unopinionated projection so an unrecognized string here is a
// bug upstream, not a fold-level failure.
func TestIdentityChoiceResolvedDoesNotWhitelistMode(t *testing.T) {
	got, effects := Transition(State{Phase: PhaseAwaitingIdentityChoice}, IdentityChoiceResolved{Mode: "not-a-real-mode"})
	assert.Equal(t, PhasePending, got.Phase)
	assert.Equal(t, "not-a-real-mode", got.EffectiveIdentityMode)
	assert.Len(t, effects, 2)
}
