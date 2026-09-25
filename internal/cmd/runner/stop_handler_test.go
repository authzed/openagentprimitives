package main

// Regression pins for the SIGTERM/SIGINT stop-handler path. The handler
// goroutine was inline in run() and therefore untestable without sending a
// real signal to the process. runStopHandler (extracted from the goroutine)
// is the smallest unit that captures the two observable effects:
//
//  1. A Stopped event is appended to the lifecycle log with the terminal-stop
//     ordering key (so it sorts after every in-loop event in the fold, keeping
//     a Succeeded-then-SIGTERM log at Succeeded rather than wrongly promoting
//     it to Failed).
//  2. MarkPlanStoppedBestEffort is called on the loop so plan items are flushed
//     to stopped before the pod exits (user-visible plan UI reflects the stop).

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// fakeStopLooper is a minimal stopLooper for testing runStopHandler without a
// real runner.Loop (which requires a k8s client, NATS, LLM provider, etc.).
type fakeStopLooper struct {
	markStopCalled bool
}

func (f *fakeStopLooper) MarkPlanStoppedBestEffort(_ context.Context) {
	f.markStopCalled = true
}

// TestRunStopHandler_AppendsStoppedWithTerminalKeyAndCallsLoop is the
// primary regression pin: when the stop handler fires it must (a) append a
// Stopped lifecycle event stamped with the terminal-stop ordering key and
// (b) call MarkPlanStoppedBestEffort on the loop. Both effects are required
// for a correct interrupted-termination: the Stopped event lets the operator
// fold to Failed rather than inferring a crash, and the plan flush ensures
// the user-visible plan UI reflects the stop before the pod exits.
func TestRunStopHandler_AppendsStoppedWithTerminalKeyAndCallsLoop(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	// Use a plain in-process memory backend. The lifecycle kind is append-only
	// but provenance verification is optional (nil verifier = no signature
	// check), so fresh appends with unique IDs work without a signing key.
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/test-sess"}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	loop := &fakeStopLooper{}

	runStopHandler(ctx, m, scope, loop, "uid-stopping", now)

	// Assert 1: exactly one Stopped event in the lifecycle log, stamped with
	// the terminal-stop sentinel key. If the key were zero, a racing Stopped
	// would sort at Seq 0 (before in-loop events) and wrongly override a
	// Succeeded terminal — the key is load-bearing, not cosmetic.
	events, err := lifecyclekind.ReadOrdered(ctx, m, scope)
	require.NoError(t, err, "ReadOrdered after runStopHandler")
	require.Len(t, events, 1, "exactly one lifecycle event must be appended")
	_, ok := events[0].Event.(lifecyclecore.Stopped)
	assert.True(t, ok, "appended event must be Stopped, got %T", events[0].Event)
	assert.Equal(t, lifecyclekind.TerminalStopKey("uid-stopping"), events[0].Key,
		"ordering key must be TerminalStopKey, naming the instance that stopped, so it "+
			"sorts after all in-loop events and folds only for that instance")

	// Assert 2: MarkPlanStoppedBestEffort was called so plan items are flushed.
	assert.True(t, loop.markStopCalled,
		"MarkPlanStoppedBestEffort must be called to flush plan state before pod exits")
}

// TestRunStopHandler_NilLoopAppendsStoppedWithoutPanic covers the race window
// where SIGTERM arrives before the Loop is fully built (loopRef is still nil).
// The Stopped lifecycle event must still be appended; the nil loop check must
// not panic. This is not an edge case: a pod eviction mid-boot triggers it.
func TestRunStopHandler_NilLoopAppendsStoppedWithoutPanic(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/early-stop"}

	require.NotPanics(t, func() {
		runStopHandler(ctx, m, scope, nil, "uid-stopping", time.Now().UTC())
	}, "nil loop must not panic")

	events, err := lifecyclekind.ReadOrdered(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, events, 1, "Stopped must be appended even when loop is nil")
	_, ok := events[0].Event.(lifecyclecore.Stopped)
	assert.True(t, ok, "event must be Stopped even with nil loop")
}
