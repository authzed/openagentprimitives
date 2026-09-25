package runner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type progEmit struct {
	in, out      int64
	elapsed, seq int
}

func TestProgressReporter_GatesAndAccumulates(t *testing.T) {
	var emits []progEmit
	publish := func(in, out int64, elapsed, seq int) {
		emits = append(emits, progEmit{in, out, elapsed, seq})
	}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := newProgressReporter(publish, 4*time.Second, 4*time.Second, clk.now)

	// Before activation delay: usage arrives but nothing emits.
	r.observeUsage(100, 50)
	require.Empty(t, emits, "no emit before activation delay")

	// Past activation, output advanced: first emit, seq=1, elapsed=5.
	clk.advance(5 * time.Second)
	r.observeUsage(100, 200)
	require.Len(t, emits, 1, "first emit after activation")
	assert.Equal(t, int64(200), emits[0].out)
	assert.Equal(t, int64(100), emits[0].in)
	assert.Equal(t, 1, emits[0].seq)
	assert.Equal(t, 5, emits[0].elapsed)

	// Within the min interval (2s < 4s): suppressed even though advanced.
	clk.advance(2 * time.Second)
	r.observeUsage(100, 300)
	require.Len(t, emits, 1, "suppressed within min interval")

	// Interval satisfied + advanced: second emit, seq=2.
	clk.advance(3 * time.Second) // now t=1010, last emit t=1005 → 5s ≥ 4s
	r.observeUsage(100, 350)
	require.Len(t, emits, 2)
	assert.Equal(t, 2, emits[1].seq)
	assert.Equal(t, int64(350), emits[1].out)

	// Output unchanged: no emit (this is the pause/idle behavior).
	clk.advance(10 * time.Second)
	r.observeUsage(100, 350)
	require.Len(t, emits, 2, "no emit when output did not advance")

	// commitCall rolls the finished call into the base; the next call's
	// usage accumulates on top rather than resetting the visible counter.
	r.commitCall(100, 350)
	clk.advance(10 * time.Second)
	r.observeUsage(40, 60) // new call: total out = 350 base + 60 = 410
	require.Len(t, emits, 3)
	assert.Equal(t, int64(410), emits[2].out, "output accumulates across calls")
	assert.Equal(t, int64(140), emits[2].in, "input accumulates across calls")
	assert.Equal(t, 3, emits[2].seq)
}

func TestProgressReporter_NilReceiverNoOps(t *testing.T) {
	var r *progressReporter
	// Must not panic — Loop calls these unconditionally on a possibly-nil reporter.
	r.observeUsage(1, 2)
	r.commitCall(1, 2)
	r.tick()
	r.pause()
	r.resume()
}

func TestProgressReporter_HeartbeatTicksWithoutTokenAdvance(t *testing.T) {
	var emits []progEmit
	publish := func(in, out int64, elapsed, seq int) { emits = append(emits, progEmit{in, out, elapsed, seq}) }
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := newProgressReporter(publish, 5*time.Second, 5*time.Second, clk.now)

	// Indicator becomes active with some output.
	clk.advance(5 * time.Second)
	r.observeUsage(100, 200)
	require.Len(t, emits, 1)

	// No token change, but the heartbeat ticks after the interval: the
	// elapsed clock advances while the token counts hold steady. This is
	// the "show progress during tool/IO" behavior.
	clk.advance(5 * time.Second)
	r.tick()
	require.Len(t, emits, 2, "heartbeat emits without token advance")
	assert.Equal(t, int64(200), emits[1].out, "tokens unchanged across the heartbeat")
	assert.Equal(t, 10, emits[1].elapsed, "elapsed clock advanced")
	assert.Equal(t, 2, emits[1].seq)

	// A heartbeat inside the interval is suppressed by the rate-limit floor.
	clk.advance(2 * time.Second)
	r.tick()
	require.Len(t, emits, 2, "heartbeat respects the min interval")
}

func TestProgressReporter_HeartbeatRespectsActivationDelay(t *testing.T) {
	var emits []progEmit
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := newProgressReporter(
		func(in, out int64, e, s int) { emits = append(emits, progEmit{in, out, e, s}) },
		5*time.Second, 5*time.Second, clk.now)
	r.commitCall(0, 100) // output exists, but the turn is still young
	clk.advance(3 * time.Second)
	r.tick()
	require.Empty(t, emits, "no heartbeat before the activation delay")
}

func TestProgressReporter_FrozenWhilePaused(t *testing.T) {
	var emits []progEmit
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := newProgressReporter(
		func(in, out int64, e, s int) { emits = append(emits, progEmit{in, out, e, s}) },
		5*time.Second, 5*time.Second, clk.now)
	clk.advance(5 * time.Second)
	r.observeUsage(50, 100)
	require.Len(t, emits, 1)

	// Parked on a human approval: neither the heartbeat nor a token advance
	// emits — the clock freezes for the duration of the wait.
	r.pause()
	clk.advance(30 * time.Second)
	r.tick()
	r.observeUsage(50, 999)
	require.Len(t, emits, 1, "no emits while parked on a human")

	// The human responds: the heartbeat resumes.
	r.resume()
	clk.advance(5 * time.Second)
	r.tick()
	require.Len(t, emits, 2, "heartbeat resumes after the approval clears")
	assert.Equal(t, 2, emits[1].seq)
}

func TestLoop_streamSink_FeedsReporterAndDelegates(t *testing.T) {
	var emits int
	var delegated []llm.StreamEventType
	clk := &fakeClock{t: time.Unix(1000, 0)}
	l := &Loop{
		OnStreamEvent: func(e llm.StreamEvent) { delegated = append(delegated, e.Type) },
		// activation/interval = 0 so any output-advancing usage event emits at once.
		progress: newProgressReporter(func(_, _ int64, _, _ int) { emits++ }, 0, 0, clk.now),
	}

	l.streamSink(llm.StreamEvent{Type: llm.StreamEventUsage, Usage: &llm.Usage{InputTokens: 10, OutputTokens: 20}})
	l.streamSink(llm.StreamEvent{Type: llm.StreamEventTextDelta, Text: "hi"})

	assert.Equal(t, 1, emits, "a usage event drives exactly one progress emit")
	assert.Equal(t, []llm.StreamEventType{llm.StreamEventUsage, llm.StreamEventTextDelta}, delegated,
		"every event still delegates to OnStreamEvent")
}
