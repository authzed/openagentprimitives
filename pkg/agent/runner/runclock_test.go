package runner_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
)

// clockAt returns a now-func reading through a caller-advanced *time.Time, so a
// test controls exactly how much wall-clock each segment sees.
func clockAt(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestRunClock(t *testing.T) {
	t.Run("nil receiver is inert", func(t *testing.T) {
		var rc *runner.RunClock
		rc.Pause()
		rc.Resume()
		assert.Equal(t, time.Duration(0), rc.Elapsed())
	})

	t.Run("seed is the floor before any active time", func(t *testing.T) {
		now := time.Unix(1000, 0)
		rc := runner.NewRunClock(5*time.Minute, clockAt(&now))
		assert.Equal(t, 5*time.Minute, rc.Elapsed(), "no wall-clock advanced yet")
	})

	t.Run("active time accrues on top of seed", func(t *testing.T) {
		now := time.Unix(1000, 0)
		rc := runner.NewRunClock(5*time.Minute, clockAt(&now))
		now = now.Add(30 * time.Second)
		assert.Equal(t, 5*time.Minute+30*time.Second, rc.Elapsed())
	})

	t.Run("paused time does not accrue", func(t *testing.T) {
		now := time.Unix(1000, 0)
		rc := runner.NewRunClock(0, clockAt(&now))
		now = now.Add(10 * time.Second) // active
		rc.Pause()                      // seal 10s
		now = now.Add(1 * time.Hour)    // parked on a human — must not count
		assert.Equal(t, 10*time.Second, rc.Elapsed())
		rc.Resume()
		now = now.Add(5 * time.Second) // active again
		assert.Equal(t, 15*time.Second, rc.Elapsed())
	})

	t.Run("double pause and double resume are idempotent", func(t *testing.T) {
		now := time.Unix(1000, 0)
		rc := runner.NewRunClock(0, clockAt(&now))
		now = now.Add(10 * time.Second)
		rc.Pause()
		now = now.Add(1 * time.Hour)
		rc.Pause() // no-op: already paused, must not seal the idle hour
		assert.Equal(t, 10*time.Second, rc.Elapsed())
		rc.Resume()
		now = now.Add(3 * time.Second)
		rc.Resume() // no-op: already active, must not reset activeSince
		now = now.Add(2 * time.Second)
		assert.Equal(t, 15*time.Second, rc.Elapsed())
	})
}
