package categories

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The presentation-only actionID→friendly-label mapping renderers apply to a
// resolved identity_choice prompt, without touching the wire OutcomeText that
// internal/cmd/runner's subscribeInteractionApplied reads back out as the raw
// approval.Decision.Action.
func TestIdentityChoiceOutcomeLabel(t *testing.T) {
	cases := []struct {
		name      string
		actionID  string
		wantLabel string
		wantOK    bool
	}{
		{"agent -> Running as the agent", "agent", "Running as the agent", true},
		{"userPassthrough -> Running as you", "userPassthrough", "Running as you", true},
		{"cancel -> Cancelled", "cancel", "Cancelled", true},
		{"unrecognized action id -> ok=false", "not-a-real-action", "", false},
		{"empty action id -> ok=false", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLabel, gotOK := IdentityChoiceOutcomeLabel(tc.actionID)
			assert.Equal(t, tc.wantOK, gotOK, "ok")
			assert.Equal(t, tc.wantLabel, gotLabel, "label")
		})
	}
}
