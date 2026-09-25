// Integration test for the core await-block invariant: while the agent is
// parked on await_user_message, the silence watchdog must stay silent (no
// "Taking longer" warn, no stall timeout) even after both the 60s warn and
// 120s stall thresholds pass. This is a plain unit test — no envtest, no
// build tag — because it runs fully in-process with a shared fake clock.
package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/watchdog"
)

// TestWatchdogSilentWhileAwaitBlocks is the missing invariant: once the agent
// has yielded via await_user_message, the silence watchdog must return
// ActionNone even past the 60s warn and 120s stall thresholds. It wires the
// await tool's OnYield directly to the watchdog (emulating the runner→channelsd
// path) and drives both with a shared fake clock so no real time elapses.
func TestWatchdogSilentWhileAwaitBlocks(t *testing.T) {
	fc := clocktesting.NewFakeClock(time.Unix(1_000, 0))
	wd := newStatusWatchdog(nil, nil)
	wd.clock = fc // drive the warn/timeout windows without wall-time sleeps

	ctx := context.Background()

	// Arm as if the agent had been working: a caption event creates tracking
	// state and stamps LastActivity with the fake clock's current time.
	wd.ApplyEvent(ctx, "ns", "s", watchdog.Event{
		Kind: watchdog.EvCaption, IsCaption: true, Seq: 10, UID: "u",
	})

	// Build the await tool sharing the same fake clock. OnYield fires exactly
	// once, before the tool blocks, and delivers the yield event to the watchdog.
	inbound := make(chan struct{}, 1) // never written to — the await stays parked
	tt := meta.NewAwait(meta.AwaitConfig{
		IdleTTL:   5 * time.Minute,
		InboundCh: inbound,
		Clock:     fc,
		OnYield: func(yieldCtx context.Context) {
			wd.ApplyEvent(yieldCtx, "ns", "s", watchdog.Event{
				Kind:   watchdog.EvTurnActivity,
				Active: false,
				Cause:  channelevents.PauseCauseReply,
				Seq:    20,
				UID:    "u",
			})
		},
	})
	go func() {
		_, _ = tt.Execute(ctx, json.RawMessage("{}"), &tool.SessionContext{})
	}()

	// Wait until the yield event has landed: the State must exist and Yielded
	// must be true before we advance the clock. Read under the lock.
	require.Eventually(t, func() bool {
		wd.mu.Lock()
		defer wd.mu.Unlock()
		st := wd.state["ns/s"]
		return st != nil && st.Yielded
	}, time.Second, time.Millisecond, "yield must land before clock advance")

	// Advance past both the 60s warn threshold and the 120s stall threshold.
	fc.Step(130 * time.Second)

	// The watchdog must stay completely silent: Yielded is the gate Decide reads
	// first, so no matter how much time has elapsed it must return ActionNone.
	wd.mu.Lock()
	st := wd.state["ns/s"]
	wd.mu.Unlock()

	require.NotNil(t, st, "tracking state must still exist after yield")
	assert.Equal(t, watchdog.ActionNone,
		st.Decide(watchdog.WaitActive, fc.Now(), watchdog.Default()),
		"watchdog must stay silent for the whole await block")
}
