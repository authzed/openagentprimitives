package runner

import (
	"sync"
	"time"
)

// RunClock accumulates a session's active run-time: wall-clock while the agent
// is working a turn, excluding time parked waiting on a human (waiting for the
// next user message, or for a tool-call approval). It is the basis for
// budget.maxDuration, which is a run-time budget — not a wall-clock cap.
//
// It is seeded at pod start from status.runDuration so run-time accumulates
// across the sleep/resume boundary rather than resetting each pod. The nil
// receiver is inert (every method no-ops; Elapsed → 0), mirroring
// progressReporter, so the Loop can hold a possibly-nil *RunClock and call it
// unconditionally. All methods are mutex-guarded: pause/resume fire from the
// loop while Elapsed may be read from a flush goroutine.
type RunClock struct {
	now func() time.Time

	mu          sync.Mutex
	accumulated time.Duration // sealed run-time from prior segments + seed
	activeSince *time.Time    // set while active; nil while parked on a human
}

// NewRunClock returns a clock seeded with prior accumulated run-time, started
// active as of now() — the pod woke to process an inbound.
func NewRunClock(seed time.Duration, now func() time.Time) *RunClock {
	t := now()
	return &RunClock{now: now, accumulated: seed, activeSince: &t}
}

// Pause seals the in-progress active segment into accumulated and stops the
// clock. Idempotent: a Pause with no intervening Resume is a no-op (so a
// double-pause can never seal idle time).
func (c *RunClock) Pause() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeSince == nil {
		return
	}
	c.accumulated += c.now().Sub(*c.activeSince)
	c.activeSince = nil
}

// Resume restarts the clock. Idempotent: a Resume while already active is a
// no-op (so it never discards an in-progress segment).
func (c *RunClock) Resume() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeSince != nil {
		return
	}
	t := c.now()
	c.activeSince = &t
}

// Elapsed is total accumulated run-time, including the in-progress active
// segment when the clock is running.
func (c *RunClock) Elapsed() time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeSince == nil {
		return c.accumulated
	}
	return c.accumulated + c.now().Sub(*c.activeSince)
}
