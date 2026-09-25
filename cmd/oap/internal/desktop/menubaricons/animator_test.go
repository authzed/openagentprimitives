package menubaricons

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func framesFn(m map[State][][]byte) func(State) [][]byte {
	return func(s State) [][]byte { return m[s] }
}

func TestSetStateStaticSetsSingleFrame(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	sink := func(b []byte) { mu.Lock(); got = append(got, b); mu.Unlock() }
	a := NewAnimator(sink, framesFn(map[State][][]byte{StateRunning: {{1}}}), time.Millisecond)
	a.SetState(StateRunning)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, [][]byte{{1}}, got)
}

func TestSetStateAnimatedCyclesFrames(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	sink := func(b []byte) { mu.Lock(); got = append(got, b); mu.Unlock() }
	a := NewAnimator(sink, framesFn(map[State][][]byte{StateSetup: {{0}, {1}, {2}}}), 5*time.Millisecond)
	a.SetState(StateSetup)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) >= 4 }, time.Second, time.Millisecond)
	a.Stop()
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []byte{0}, got[0], "first frame shown immediately")
	require.Equal(t, []byte{1}, got[1], "then cycles to frame 1")
}

func TestSetStateSwitchStopsPriorAnimation(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	sink := func(b []byte) { mu.Lock(); got = append(got, b); mu.Unlock() }
	a := NewAnimator(sink, framesFn(map[State][][]byte{StateSetup: {{0}, {1}}, StateRunning: {{9}}}), 5*time.Millisecond)
	a.SetState(StateSetup)
	time.Sleep(12 * time.Millisecond)
	a.SetState(StateRunning)
	mu.Lock()
	n := len(got)
	last := got[n-1]
	mu.Unlock()
	require.Equal(t, []byte{9}, last, "switch shows running immediately")
	time.Sleep(25 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, n, len(got), "no frames appended after switching to a static state")
}

func TestSetStateIdempotentWhileAnimating(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	sink := func(b []byte) { mu.Lock(); got = append(got, b); mu.Unlock() }
	// A huge interval means the ticker never fires during the test, isolating
	// the restart-vs-no-op behavior of a repeated SetState from real cycling.
	a := NewAnimator(sink, framesFn(map[State][][]byte{StateSetup: {{0}, {1}, {2}}}), time.Hour)
	a.SetState(StateSetup) // paints frame 0 once, starts an (idle) ticker
	a.SetState(StateSetup) // same animated state: must be a no-op, not a restart-repaint
	a.Stop()
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, [][]byte{{0}}, got, "re-asserting the animating state must not repaint frame 0")
}

func TestStopIdempotentAndIdle(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	sink := func(b []byte) { mu.Lock(); got = append(got, b); mu.Unlock() }
	a := NewAnimator(sink, framesFn(map[State][][]byte{StateSetup: {{0}, {1}}}), 5*time.Millisecond)
	a.Stop() // idle Stop: must not panic
	a.SetState(StateSetup)
	a.Stop()
	a.Stop() // double Stop: must not panic or deadlock
	mu.Lock()
	n := len(got)
	mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, n, len(got), "no frames painted after Stop")
}
