package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestShouldSelfHeal guards the self-heal rule for the live-view status stream:
// correct ONLY a stuck-working state (authoritative phase parked, but the stream
// still shows working). A phase Get that lags a freshly-started turn must NEVER
// flip a genuinely-working session back to paused — the live turn_activity
// events own that transition.
func TestShouldSelfHeal(t *testing.T) {
	assert.True(t, shouldSelfHeal(true, false), "phase parked + stream working → heal to not-working")
	assert.False(t, shouldSelfHeal(true, true), "both parked → nothing to correct")
	assert.False(t, shouldSelfHeal(false, false), "both working → nothing to correct")
	assert.False(t, shouldSelfHeal(false, true), "a lagging active phase must NOT override a parked stream")
}
