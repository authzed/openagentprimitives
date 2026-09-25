package monitoring

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestDetectTransition(t *testing.T) {
	cases := []struct {
		name           string
		known          bool
		prev           bool
		isFailing      bool
		terminal       bool
		wantEmit       bool
		wantTransition string
	}{
		{"first observation healthy: no emit", false, false, false, false, false, ""},
		{"first observation failing: emit failed (restart re-announce)", false, false, true, false, true, channelevents.MonitoringTransitionFailed},
		{"healthy stays healthy: no emit", true, false, false, false, false, ""},
		{"failing stays failing: no emit (transitions-only dedup)", true, true, true, false, false, ""},
		{"healthy to failing: emit failed", true, false, true, false, true, channelevents.MonitoringTransitionFailed},
		{"failing to healthy: emit recovered", true, true, false, false, true, channelevents.MonitoringTransitionRecovered},
		// Terminal rules suppress ONLY the cold re-announce; every
		// transition observed while running behaves identically.
		{"terminal, first observation failing: no emit (past incident, not open problem)", false, false, true, true, false, ""},
		{"terminal, first observation healthy: no emit", false, false, false, true, false, ""},
		{"terminal, healthy to failing: emit failed (a live failure still reports)", true, false, true, true, true, channelevents.MonitoringTransitionFailed},
		{"terminal, failing stays failing: no emit", true, true, true, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emit, transition := detectTransition(tc.known, tc.prev, tc.isFailing, tc.terminal)
			assert.Equal(t, tc.wantEmit, emit)
			assert.Equal(t, tc.wantTransition, transition)
		})
	}
}

func TestTracker_ObserveAndForget(t *testing.T) {
	tr := newTracker()

	// First failing observation emits.
	emit, transition := tr.observe("default/bot/Refresh", true, false)
	assert.True(t, emit)
	assert.Equal(t, channelevents.MonitoringTransitionFailed, transition)

	// Same state again: no emit.
	emit, _ = tr.observe("default/bot/Refresh", true, false)
	assert.False(t, emit)

	// Recovery emits.
	emit, transition = tr.observe("default/bot/Refresh", false, false)
	assert.True(t, emit)
	assert.Equal(t, channelevents.MonitoringTransitionRecovered, transition)

	// forget drops the object's keys — a later observation is "first" again.
	tr.observe("default/bot/Refresh", false, false)
	tr.forget("default/bot/")
	emit, transition = tr.observe("default/bot/Refresh", true, false)
	assert.True(t, emit, "after forget, a failing observation is a fresh transition")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, transition)
}

// A suppressed cold observation must still SEED the tracker: if it did not, the
// next tick would read the same sticky condition as a fresh none→failed
// transition and emit anyway, reintroducing the flood one tick later.
func TestTracker_TerminalColdObservationSeedsWithoutEmitting(t *testing.T) {
	tr := newTracker()

	emit, _ := tr.observe("default/s1/Failed", true, true)
	assert.False(t, emit, "cold observation of a terminal failure is silent")

	emit, _ = tr.observe("default/s1/Failed", true, true)
	assert.False(t, emit, "and stays silent on every subsequent tick")
}
