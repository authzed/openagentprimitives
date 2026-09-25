package menubaricons

import (
	"sync"
	"time"
)

// Animator drives the menu-bar icon: static states set a single frame; animated
// states cycle their frames on a ticker until the state changes. It is
// platform-neutral — the icon sink and frame source are injected — so it
// unit-tests without systray or a real menu bar.
//
// The injected setIcon sink is only ever driven from one place at a time:
// SetState waits (via the done channel) for a prior animation's goroutine to
// fully exit before painting the next frame, so a switch can never leave a
// stale frame painted after the new one. setIcon therefore must not call back
// into the Animator (SetState holds a.mu while it paints the first frame).
type Animator struct {
	setIcon  func([]byte)
	frames   func(State) [][]byte
	interval time.Duration

	mu   sync.Mutex
	stop chan struct{} // closed to stop the current animation goroutine; nil when idle
	done chan struct{} // closed by the animation goroutine when it exits; nil when idle
	cur  State         // last state passed to SetState; guards idempotent re-asserts
}

// NewAnimator builds an Animator. setIcon receives the PNG bytes of the frame to
// show (on darwin: systray.SetTemplateIcon(b, b)). frames returns a state's
// frames (menubaricons/assets.Frames). interval is the per-frame dwell for
// animated states.
func NewAnimator(setIcon func([]byte), frames func(State) [][]byte, interval time.Duration) *Animator {
	return &Animator{setIcon: setIcon, frames: frames, interval: interval}
}

// SetState switches the icon to s: stops any running animation (waiting for its
// goroutine to exit), shows s's first frame immediately, and (for multi-frame
// states) starts cycling the rest. Safe to call from any goroutine.
func (a *Animator) SetState(s State) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Idempotent while animating: re-asserting the state that is already
	// animating is a no-op, so per-step callers (e.g. bring-up progress) don't
	// restart the animation and flash it back to its first frame. A static
	// state (a.stop == nil) falls through and harmlessly re-sets its one frame;
	// after Stop() (a.stop == nil) a re-assert also restarts, as intended.
	if a.cur == s && a.stop != nil {
		return
	}
	a.stopLocked()
	a.cur = s
	f := a.frames(s)
	if len(f) == 0 {
		return
	}
	a.setIcon(f[0])
	if len(f) == 1 {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	a.stop, a.done = stop, done
	go a.cycle(f, stop, done)
}

func (a *Animator) cycle(f [][]byte, stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(a.interval)
	defer t.Stop()
	i := 1
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			// A tick and a stop can be ready at the same instant; select picks
			// randomly, so re-check stop before painting to avoid a stale frame.
			select {
			case <-stop:
				return
			default:
			}
			a.setIcon(f[i%len(f)])
			i++
		}
	}
}

// Stop halts any running animation (waiting for its goroutine to exit), leaving
// the last-shown frame in place. Safe to call repeatedly and when idle.
func (a *Animator) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopLocked()
}

// stopLocked stops the current animation goroutine and waits for it to exit, so
// no further setIcon call can land after the caller proceeds. Must hold a.mu.
// The goroutine never acquires a.mu, so waiting here cannot deadlock.
func (a *Animator) stopLocked() {
	if a.stop != nil {
		close(a.stop)
		<-a.done
		a.stop, a.done = nil, nil
	}
}
