package installcmd

import (
	"context"
	"sync"
	"time"
)

// extendableTimeout is a context.Context whose timeout can be restarted via its
// reset func. It bounds install work like context.WithTimeout, but reset lets the
// caller EXCLUDE an interactive pause — the AI-fixer ([f]) session, which can run
// for many minutes while the user drives their own CLI — from the budget: after
// the pause we restart the clock so synchronous work resuming afterward gets a
// fresh window instead of inheriting an already-expired deadline.
//
// The bug this fixes: after a long [f] fix session, the overall --timeout had
// already elapsed, so the next synchronous step failed immediately with
// "ensure cluster pinning mode: create ClusterAgentSettings: client rate limiter
// Wait returned an error: context deadline exceeded".
//
// It reports context.DeadlineExceeded on timeout (matching WithTimeout, so the
// readiness loop keeps treating an elapsed budget as "offer keep-waiting" rather
// than a hard Ctrl-C stop) and propagates parent cancellation (SIGINT) verbatim.
type extendableTimeout struct {
	parent     context.Context
	d          time.Duration
	mu         sync.Mutex
	timer      *time.Timer
	deadline   time.Time
	done       chan struct{}
	err        error
	finishOnce sync.Once
}

// newExtendableTimeout returns the context, a reset func that restarts the
// timeout to its configured duration (a no-op once the context is done), and a
// cancel func the caller MUST defer (releases the timer + parent watcher).
func newExtendableTimeout(parent context.Context, d time.Duration) (ctx context.Context, reset func(), cancel func()) {
	e := &extendableTimeout{
		parent:   parent,
		d:        d,
		done:     make(chan struct{}),
		deadline: time.Now().Add(d),
	}
	// Assign e.timer under the lock: a tiny d can fire the callback (→ finish,
	// which reads e.timer under e.mu) before this assignment returns, so hold the
	// lock to establish happens-before and avoid a data race on the field.
	e.mu.Lock()
	e.timer = time.AfterFunc(d, func() { e.finish(context.DeadlineExceeded) })
	e.mu.Unlock()
	// Propagate parent cancellation (SIGINT). stopWatch unregisters the watcher
	// when we cancel/finish so it does not linger past this context's life.
	stopWatch := context.AfterFunc(parent, func() { e.finish(parent.Err()) })
	cancel = func() {
		stopWatch()
		e.finish(context.Canceled)
	}
	return e, e.reset, cancel
}

func (e *extendableTimeout) finish(err error) {
	e.finishOnce.Do(func() {
		e.mu.Lock()
		e.err = err
		e.timer.Stop()
		e.mu.Unlock()
		close(e.done)
	})
}

// reset restarts the timeout window unless the context is already done.
func (e *extendableTimeout) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.done:
		return // already timed out / canceled — nothing to extend
	default:
	}
	e.timer.Stop()
	e.deadline = time.Now().Add(e.d)
	e.timer.Reset(e.d)
}

func (e *extendableTimeout) Deadline() (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deadline, true
}
func (e *extendableTimeout) Done() <-chan struct{} { return e.done }
func (e *extendableTimeout) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// Value delegates to the parent so context values (e.g. the external-tool
// timeout, the deadline-reset handle) still resolve through this context.
func (e *extendableTimeout) Value(key any) any { return e.parent.Value(key) }

// deadlineResetKey carries the install's deadline-reset func on the context so
// any layer (the AI-fix hook) can reach it regardless of how the context was
// wrapped afterward (e.g. by cloud.WithExternalToolTimeout).
type deadlineResetKey struct{}

// withDeadlineReset returns a context carrying reset so resetInstallDeadline can
// find it downstream.
func withDeadlineReset(ctx context.Context, reset func()) context.Context {
	if reset == nil {
		return ctx
	}
	return context.WithValue(ctx, deadlineResetKey{}, reset)
}

// resetInstallDeadline restarts the install's overall timeout if the context
// carries an extendable one; a no-op otherwise. Called when returning from an
// interactive AI-fix session so the time spent there does not count against the
// install budget.
func resetInstallDeadline(ctx context.Context) {
	if reset, ok := ctx.Value(deadlineResetKey{}).(func()); ok {
		reset()
	}
}
