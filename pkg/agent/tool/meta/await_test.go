package meta

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// Every fake-clock test in this file has the same hazard, and these two
// conditions are the whole of the fix for it.
//
// yieldAndWait calls OnYield BEFORE it creates the timer (await.go), so a Step
// taken on the strength of the yield counter alone can advance a clock with no
// waiters registered. The step is then LOST — the timer that follows measures
// its deadline from the already-advanced fake now — and the wait never ends: a
// hang until the package timeout, ten minutes of the ship gate for a test that
// runs in a millisecond.
//
// awaitTimerArmed is the condition for the FIRST step: the yield happened (what
// these tests also assert) and a timer is actually waiting on the clock.
func awaitTimerArmed(fc *clocktesting.FakeClock, yields *int32) func() bool {
	return func() bool { return atomic.LoadInt32(yields) == 1 && fc.HasWaiters() }
}

// awaitTimerRearmed is the condition for a step AFTER a presence heartbeat.
//
// HasWaiters alone cannot tell the RESTARTED timer from the original it
// replaced — the original is a waiter too — and no poll can catch the instant
// between Stop and NewTimer where it could tell them apart. So the tests do not
// ask the clock: they send the heartbeat on an UNBUFFERED channel, which
// completes only when the wait loop has received it. That handshake is what
// makes the restart observable; this condition then just waits for a timer to
// be armed again, and BOTH of the timers it could be are safe to step:
//
//   - the original, if Stop has not run yet — the loop reassigns `timer`
//     before selecting again, so a value it fired into the old channel is
//     discarded, not read as an expiry;
//   - the restarted one, whose deadline is 30s past the heartbeat.
//
// And a step lost between the two (no waiters for an instant) only shortens
// the fake elapsed time, which never fires anything early.
func awaitTimerRearmed(fc *clocktesting.FakeClock) func() bool {
	return fc.HasWaiters
}

func TestAwaitYieldsOnInbound(t *testing.T) {
	inbound := make(chan struct{}, 1)
	tt := NewAwait(AwaitConfig{
		IdleTTL:   1 * time.Hour,
		InboundCh: inbound,
	})
	go func() { inbound <- struct{}{} }()
	start := time.Now()
	res, err := tt.Execute(context.Background(), json.RawMessage("{}"), &tool.SessionContext{})
	require.NoError(t, err, "Execute must not return a Go error")
	assert.Less(t, time.Since(start), 500*time.Millisecond, "must return fast on inbound signal")
	assert.False(t, res.IsError, "no error expected on inbound wake")
	assert.False(t, res.Terminal, "inbound wake must not terminate")
	assert.True(t, res.Trusted, "await_user_message is a framework meta tool and must opt out of content-guard inspection")
}

func TestAwaitOnYieldFiresOnceBeforeBlocking(t *testing.T) {
	inbound := make(chan struct{}, 1)
	var yields, resumes int32
	tt := NewAwait(AwaitConfig{
		IdleTTL:   time.Hour,
		InboundCh: inbound,
		Clock:     clocktesting.NewFakeClock(time.Now()),
		OnYield:   func(context.Context) { atomic.AddInt32(&yields, 1) },
		OnResume:  func(context.Context) { atomic.AddInt32(&resumes, 1) },
	})
	go func() { inbound <- struct{}{} }()
	res, err := tt.Execute(context.Background(), json.RawMessage("{}"), &tool.SessionContext{})
	require.NoError(t, err)
	assert.False(t, res.Terminal, "inbound wake must not terminate")
	assert.Equal(t, int32(1), atomic.LoadInt32(&yields), "OnYield fires exactly once, before blocking")
	assert.Equal(t, int32(1), atomic.LoadInt32(&resumes), "OnResume fires on inbound wake")
}

func TestAwaitTTLExpiryYieldsButDoesNotResume(t *testing.T) {
	var yields, resumes int32
	fc := clocktesting.NewFakeClock(time.Now())
	tt := NewAwait(AwaitConfig{
		IdleTTL:   30 * time.Second,
		InboundCh: make(chan struct{}, 1),
		Clock:     fc,
		OnYield:   func(context.Context) { atomic.AddInt32(&yields, 1) },
		OnResume:  func(context.Context) { atomic.AddInt32(&resumes, 1) },
	})
	done := make(chan tool.Result, 1)
	go func() {
		r, _ := tt.Execute(context.Background(), json.RawMessage("{}"), &tool.SessionContext{})
		done <- r
	}()
	// Let Execute reach the timer wait, then expire it.
	//
	// Waiting on the fake clock's own waiter set, not only on the yield
	// counter: yieldAndWait calls OnYield BEFORE it creates the timer
	// (await.go) — see awaitTimerArmed.
	require.Eventually(t, awaitTimerArmed(fc, &yields), time.Second, time.Millisecond)
	fc.Step(31 * time.Second)
	res := <-done
	assert.True(t, res.Terminal && res.IdleExit, "TTL expiry is a terminal idle exit")
	assert.Equal(t, int32(1), atomic.LoadInt32(&yields), "OnYield still fires on the TTL path")
	assert.Equal(t, int32(0), atomic.LoadInt32(&resumes), "OnResume must NOT fire on TTL expiry")
}

func TestAwait_ResumeSetsAwaitResumed(t *testing.T) {
	inbound := make(chan struct{}, 1)
	inbound <- struct{}{} // a reply is already waiting
	tt := NewAwait(AwaitConfig{
		IdleTTL:   time.Hour, // long, so the reply wins the select
		InboundCh: inbound,
	})
	res, err := tt.Execute(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.True(t, res.AwaitResumed, "a real reply must set AwaitResumed")
	assert.False(t, res.Terminal, "a reply resumes (non-terminal)")
	// The resume result is a bare acknowledgment — NOT an instruction to go read
	// memory. The runner's drain splices the real message in as the next user
	// turn, so the model needs no lookup; a "check memory" instruction here is a
	// fossil from before the drain existed and lures the model into a
	// full-transcript query_memory loop when the drain misses.
	assert.Equal(t, "acknowledged", res.Content, "resume result must be a neutral ack, no memory instruction")
}

func TestAwait_IdleExitDoesNotSetAwaitResumed(t *testing.T) {
	tt := NewAwait(AwaitConfig{IdleTTL: 0}) // 0 → immediate idle exit
	res, err := tt.Execute(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.False(t, res.AwaitResumed, "a TTL idle-exit must NOT set AwaitResumed")
	assert.True(t, res.Terminal, "idle exit is terminal")
	assert.True(t, res.IdleExit)
}

// TestAwaitTerminalIdleExits collapses the three exit-with-Terminal+IdleExit
// cases (TTL expiry, ctx cancel, TTL=0) that share construct + execute shape
// and differ only in how they trigger the idle exit.
func TestAwaitTerminalIdleExits(t *testing.T) {
	cases := []struct {
		name string
		cfg  AwaitConfig
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{
			name: "IdleTTL expiry: Terminal+IdleExit",
			cfg:  AwaitConfig{IdleTTL: 50 * time.Millisecond, InboundCh: make(chan struct{}, 1)},
			ctx:  func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
		},
		{
			name: "context cancel: Terminal+IdleExit",
			cfg:  AwaitConfig{IdleTTL: 1 * time.Hour, InboundCh: make(chan struct{}, 1)},
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					time.Sleep(50 * time.Millisecond)
					cancel()
				}()
				return ctx, cancel
			},
		},
		{
			name: "IdleTTL=0 (disabled): Terminal+IdleExit immediately",
			cfg:  AwaitConfig{IdleTTL: 0, InboundCh: make(chan struct{}, 1)},
			ctx:  func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tt := NewAwait(tc.cfg)
			ctx, cancel := tc.ctx()
			defer cancel()
			res, err := tt.Execute(ctx, json.RawMessage("{}"), &tool.SessionContext{})
			require.NoError(t, err, "Execute must not return a Go error")
			assert.True(t, res.Terminal, "Terminal must be true on idle exit")
			assert.True(t, res.IdleExit, "IdleExit must be true on idle exit")
			assert.True(t, res.Trusted, "await_user_message is a framework meta tool and must opt out of content-guard inspection")
		})
	}
}

// A ui_presence heartbeat means "someone is looking at this session's
// agent-defined UI". It must extend the idle wait without pretending anyone
// spoke — the runner has to stay up to answer declared data bindings, which
// involve no conversation at all.
func TestAwaitPresenceExtendsTheIdleWait(t *testing.T) {
	var yields, resumes int32
	fc := clocktesting.NewFakeClock(time.Now())
	presence := make(chan struct{}) // unbuffered: the send IS the handshake — see awaitTimerRearmed
	tt := NewAwait(AwaitConfig{
		IdleTTL:    30 * time.Second,
		InboundCh:  make(chan struct{}, 1),
		PresenceCh: presence,
		Clock:      fc,
		OnYield:    func(context.Context) { atomic.AddInt32(&yields, 1) },
		OnResume:   func(context.Context) { atomic.AddInt32(&resumes, 1) },
	})
	done := make(chan tool.Result, 1)
	go func() {
		r, _ := tt.Execute(context.Background(), json.RawMessage("{}"), &tool.SessionContext{})
		done <- r
	}()
	require.Eventually(t, awaitTimerArmed(fc, &yields), time.Second, time.Millisecond)

	// Advance most of the way, then heartbeat. The timer must restart, so the
	// SAME advance that would otherwise have expired it now does not.
	fc.Step(25 * time.Second)
	presence <- struct{}{}
	// Wait for the heartbeat to be consumed AND the new timer armed, otherwise
	// the Step below could race the restart and this test would pass for the
	// wrong reason — or, worse, expire the ORIGINAL timer at 50s and report a
	// working extension as broken.
	require.Eventually(t, awaitTimerRearmed(fc), time.Second, time.Millisecond)
	fc.Step(25 * time.Second)

	select {
	case r := <-done:
		t.Fatalf("presence must extend the wait, but the tool returned early: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	// And it is an EXTENSION, not a cancellation: with no further heartbeats
	// the timer still expires on its own.
	fc.Step(31 * time.Second)
	res := <-done
	assert.True(t, res.Terminal && res.IdleExit, "with heartbeats stopped, the idle exit still happens")
	assert.Equal(t, int32(0), atomic.LoadInt32(&resumes),
		"a heartbeat is not a user reply: OnResume must never fire for one")
}

// The extension must be repeatable. A single-shot reset would keep a runner
// alive for exactly one heartbeat past the first, which looks correct in a
// short test and fails a viewer who leaves a dashboard open.
func TestAwaitPresenceExtendsRepeatedly(t *testing.T) {
	var yields int32
	fc := clocktesting.NewFakeClock(time.Now())
	presence := make(chan struct{}) // unbuffered: the send IS the handshake — see awaitTimerRearmed
	tt := NewAwait(AwaitConfig{
		IdleTTL:    30 * time.Second,
		InboundCh:  make(chan struct{}, 1),
		PresenceCh: presence,
		Clock:      fc,
		OnYield:    func(context.Context) { atomic.AddInt32(&yields, 1) },
	})
	done := make(chan tool.Result, 1)
	go func() {
		r, _ := tt.Execute(context.Background(), json.RawMessage("{}"), &tool.SessionContext{})
		done <- r
	}()
	require.Eventually(t, awaitTimerArmed(fc, &yields), time.Second, time.Millisecond)

	for i := 0; i < 5; i++ {
		fc.Step(25 * time.Second)
		presence <- struct{}{}
		require.Eventually(t, awaitTimerRearmed(fc), time.Second, time.Millisecond)
	}
	select {
	case r := <-done:
		t.Fatalf("five heartbeats must hold the wait open; returned %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	fc.Step(31 * time.Second)
	assert.True(t, (<-done).IdleExit, "still exits once the heartbeats stop")
}

// A real user message must still win while a dashboard is being watched: a
// session someone is looking at is exactly one they may also talk to.
func TestAwaitInboundStillResumesWhilePresenceIsFlowing(t *testing.T) {
	var yields, resumes int32
	fc := clocktesting.NewFakeClock(time.Now())
	presence := make(chan struct{}) // unbuffered: the send IS the handshake — see awaitTimerRearmed
	inbound := make(chan struct{}, 1)
	tt := NewAwait(AwaitConfig{
		IdleTTL:    30 * time.Second,
		InboundCh:  inbound,
		PresenceCh: presence,
		Clock:      fc,
		OnYield:    func(context.Context) { atomic.AddInt32(&yields, 1) },
		OnResume:   func(context.Context) { atomic.AddInt32(&resumes, 1) },
	})
	done := make(chan tool.Result, 1)
	go func() {
		r, _ := tt.Execute(context.Background(), json.RawMessage("{}"), &tool.SessionContext{})
		done <- r
	}()
	require.Eventually(t, awaitTimerArmed(fc, &yields), time.Second, time.Millisecond)

	presence <- struct{}{}
	require.Eventually(t, awaitTimerRearmed(fc), time.Second, time.Millisecond)
	inbound <- struct{}{}

	res := <-done
	assert.True(t, res.AwaitResumed, "an inbound message still resumes the loop")
	assert.False(t, res.Terminal)
	assert.Equal(t, int32(1), atomic.LoadInt32(&resumes))
}

// A nil PresenceCh is the ordinary shape for every caller that has no browser
// behind it (kubectl-driven sessions, tests). A nil channel blocks forever in
// a select, which is exactly right — but only if the arm was added without
// disturbing the others, so this pins the old behaviour explicitly.
func TestAwaitNilPresenceChBehavesAsBefore(t *testing.T) {
	var yields int32
	fc := clocktesting.NewFakeClock(time.Now())
	tt := NewAwait(AwaitConfig{
		IdleTTL:   30 * time.Second,
		InboundCh: make(chan struct{}, 1),
		Clock:     fc,
		OnYield:   func(context.Context) { atomic.AddInt32(&yields, 1) },
	})
	done := make(chan tool.Result, 1)
	go func() {
		r, _ := tt.Execute(context.Background(), json.RawMessage("{}"), &tool.SessionContext{})
		done <- r
	}()
	require.Eventually(t, awaitTimerArmed(fc, &yields), time.Second, time.Millisecond)
	fc.Step(31 * time.Second)
	assert.True(t, (<-done).IdleExit, "with no presence channel the TTL expires exactly as it always did")
}
