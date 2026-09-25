package slack

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestFormatElapsedSuffix_Characterization pins the exact "…elapsed." caption
// the awaiting-approval ticker appends (interaction_ticker.go).
func TestFormatElapsedSuffix_Characterization(t *testing.T) {
	cases := []struct {
		name    string
		elapsed time.Duration
		want    string
	}{
		{"under a minute: just now", 30 * time.Second, "_just now._"},
		{"one minute: 1m elapsed", 65 * time.Second, "_1m elapsed._"},
		{"nine minutes: 9m elapsed", 9 * time.Minute, "_9m elapsed._"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, formatElapsedSuffix(tc.elapsed)) })
	}
}

// TestAwaitingTickCadence_Characterization pins the ticker's interval and
// max-duration consts.
func TestAwaitingTickCadence_Characterization(t *testing.T) {
	assert.Equal(t, 30*time.Second, awaitingTickInterval)
	assert.Equal(t, 10*time.Minute, awaitingMaxDuration)
}
