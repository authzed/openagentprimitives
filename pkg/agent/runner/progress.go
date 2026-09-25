package runner

import (
	"context"
	"sync"
	"time"
)

// Default cadence knobs for the live turn-progress indicator. The same
// value serves three roles: the activation delay (no indicator on fast
// turns), the minimum interval between emits (the channel-side rate-limit
// floor), and the heartbeat period — so the elapsed clock advances at this
// cadence during active work even when no tokens are flowing.
const (
	progressActivationDelay = 5 * time.Second
	progressMinInterval     = 5 * time.Second
)

// progressReporter throttles live token/time progress for one Run. It is
// fed the in-flight LLM call's cumulative usage (observeUsage) and each
// call's final usage as it completes (commitCall), and emits at most one
// snapshot per minInterval — never before activationDelay — via publish.
// Emission is gated on output-token advance, so quiescent periods (tool
// execution, approval waits) produce no updates and the elapsed clock the
// user sees freezes until generation resumes.
//
// observeUsage/commitCall are mutex-guarded: the llm.Request.OnEvent
// contract permits a provider to invoke callbacks from a non-loop
// goroutine. All methods no-op on a nil receiver so the Loop can call
// them unconditionally.
type progressReporter struct {
	publish         func(inputTokens, outputTokens int64, elapsedSeconds, seq int)
	activationDelay time.Duration
	minInterval     time.Duration
	now             func() time.Time

	mu          sync.Mutex
	turnStart   time.Time
	baseIn      int64 // committed totals from completed LLM calls
	baseOut     int64
	curIn       int64 // in-flight totals for the current LLM call
	curOut      int64
	lastEmit    time.Time
	lastEmitOut int64
	seq         int
	paused      bool // true while parked on a human approval/input wait
}

func newProgressReporter(
	publish func(inputTokens, outputTokens int64, elapsedSeconds, seq int),
	activationDelay, minInterval time.Duration,
	now func() time.Time,
) *progressReporter {
	return &progressReporter{
		publish:         publish,
		activationDelay: activationDelay,
		minInterval:     minInterval,
		now:             now,
		turnStart:       now(),
	}
}

// observeUsage records the latest cumulative usage for the in-flight LLM
// call and may emit a snapshot. Providers report cumulative (not delta)
// usage per call, so in/out here are the current call's running totals.
func (r *progressReporter) observeUsage(inputTokens, outputTokens int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.curIn, r.curOut = inputTokens, outputTokens
	r.maybeEmit()
}

// commitCall rolls the just-finished LLM call's final usage into the
// committed base and resets the in-flight totals, so the next call's usage
// accumulates on top rather than resetting the visible counter.
func (r *progressReporter) commitCall(inputTokens, outputTokens int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.baseIn += inputTokens
	r.baseOut += outputTokens
	r.curIn, r.curOut = 0, 0
}

// maybeEmit is the token-driven path: it publishes only when output has
// advanced since the last emit (responsive to generation). Caller holds r.mu.
func (r *progressReporter) maybeEmit() { r.emitLocked(true) }

// tick is the heartbeat path: it publishes the current snapshot on the
// minInterval floor even when no tokens advanced, so the elapsed clock keeps
// moving during active tool/IO work. No-ops while paused (parked on a human).
func (r *progressReporter) tick() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emitLocked(false)
}

// pause freezes all progress emits — used while the turn is parked waiting on
// a human (tool approval / input). The elapsed clock the user sees stops until
// resume, so a wait doesn't read as "progress". No-op on a nil receiver.
func (r *progressReporter) pause() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.paused = true
	r.mu.Unlock()
}

// resume re-enables emits after a human responds. No-op on a nil receiver.
func (r *progressReporter) resume() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.paused = false
	r.mu.Unlock()
}

// emitLocked publishes a snapshot when every gate passes. requireAdvance is
// true for the token-driven path (suppress when output didn't grow) and false
// for the heartbeat (time-based liveness). Caller holds r.mu.
func (r *progressReporter) emitLocked(requireAdvance bool) {
	if r.paused {
		return // parked on a human → freeze the clock
	}
	now := r.now()
	if now.Sub(r.turnStart) < r.activationDelay {
		return // activation delay: fast turns never show a counter
	}
	totalOut := r.baseOut + r.curOut
	if requireAdvance && totalOut <= r.lastEmitOut {
		return // token path: no output progress since last emit → suppress
	}
	if !r.lastEmit.IsZero() && now.Sub(r.lastEmit) < r.minInterval {
		return // rate-limit floor (shared by both paths)
	}
	r.seq++
	r.lastEmit = now
	r.lastEmitOut = totalOut
	r.publish(r.baseIn+r.curIn, totalOut, int(now.Sub(r.turnStart).Seconds()), r.seq)
}

// runHeartbeat drives time-based emits every interval until ctx is done, so
// the elapsed clock advances during active tool/IO work even when no LLM
// tokens are flowing. tick() itself no-ops while paused (parked on a human)
// and respects the minInterval floor, so this never out-paces the token path.
func (r *progressReporter) runHeartbeat(ctx context.Context, interval time.Duration) {
	if r == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick()
		}
	}
}
