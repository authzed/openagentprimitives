// Package startup provides bounded-retry primitives for process startup steps
// that depend on other components becoming reachable. It exists to replace the
// os.Exit-on-first-failure pattern that turned a not-yet-ready dependency (a slow
// postgres, an operator still coming up) into a CrashLoopBackoff cascade during
// `oap install`.
package startup

import (
	"context"
	"fmt"
	"time"
)

const (
	defaultBackoffFloor = 500 * time.Millisecond
	defaultBackoffCap   = 5 * time.Second
)

// Retry runs fn repeatedly until it returns nil, ctx is cancelled, or the
// cumulative wait exceeds ceiling. Between failed attempts it sleeps with a
// capped exponential backoff (500ms doubling to 5s). Each failed attempt invokes
// onRetry (when non-nil) with the 1-based attempt number, the attempt's error,
// and the delay before the next attempt — callers wire this to their own logger
// so every failure is surfaced loudly (no silent retry). On ceiling, Retry
// returns the last error wrapped with op and elapsed (caller should os.Exit-loud);
// on cancellation it returns the ctx error. fn MUST honor ctx cancellation.
//
// The bounded ceiling preserves fail-closed semantics: a dependency that never
// comes up still terminates the process loudly, with CrashLoopBackoff as the
// last-resort backstop rather than the first line of defense.
func Retry(ctx context.Context, op string, ceiling time.Duration,
	fn func(context.Context) error,
	onRetry func(attempt int, err error, next time.Duration)) error {
	return retryWithBackoff(ctx, op, ceiling, defaultBackoffFloor, defaultBackoffCap, fn, onRetry)
}

// retryWithBackoff is the parameterized core; tests drive it with tiny delays so
// they never sleep on the production ceilings.
func retryWithBackoff(ctx context.Context, op string, ceiling, minDelay, maxDelay time.Duration,
	fn func(context.Context) error,
	onRetry func(attempt int, err error, next time.Duration)) error {
	start := time.Now()
	deadline := start.Add(ceiling)
	delay := minDelay
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: not ready after %s (%d attempts): %w",
				op, time.Since(start).Round(time.Second), attempt, err)
		}
		if onRetry != nil {
			onRetry(attempt, err, delay)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, maxDelay)
	}
}
