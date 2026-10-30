package tokenrestore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/controllers/tokenrestore"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

type syncedCache struct{}

func (syncedCache) WaitForCacheSync(context.Context) bool { return true }

// fakeRestorer fails its first failures calls, then succeeds, and counts
// every call.
type fakeRestorer struct {
	mu       sync.Mutex
	failures int
	calls    int
	onCall   func()
}

func (f *fakeRestorer) RestoreTokens(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.onCall != nil {
		f.onCall()
	}
	if f.calls <= f.failures {
		return 0, errors.New("transient")
	}
	return 1, nil
}

func newRunnable(reg *tokens.Registry, rs ...tokenrestore.Restorer) *tokenrestore.Runnable {
	reg.BeginRestore()
	return &tokenrestore.Runnable{Registry: reg, Cache: syncedCache{}, Restorers: rs}
}

func TestRunnable_RunsEveryRestorerThenFinishes(t *testing.T) {
	reg := tokens.NewRegistry()
	a, b := &fakeRestorer{}, &fakeRestorer{}
	run := newRunnable(reg, a, b)
	assert.False(t, run.NeedLeaderElection(), "every replica serves the memory API, so every replica restores")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, run.Start(ctx))
	assert.False(t, reg.Restoring(), "the restoring state ends once every restorer is done")
	assert.Equal(t, 1, a.calls)
	assert.Equal(t, 1, b.calls)
}

func TestRunnable_OneFailingRestorerKeepsTheRegistryRestoringUntilItSucceeds(t *testing.T) {
	reg := tokens.NewRegistry()
	ok := &fakeRestorer{}
	flaky := &fakeRestorer{failures: 1}
	flaky.onCall = func() {
		if flaky.calls == 1 {
			assert.True(t, reg.Restoring(), "still restoring while a restorer is failing")
		}
	}
	run := newRunnable(reg, ok, flaky)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, run.Start(ctx))
	assert.False(t, reg.Restoring(), "finished once the retry succeeded")
	assert.Equal(t, 2, flaky.calls, "the failing restorer was retried")
	assert.Equal(t, 2, ok.calls, "the whole pass is retried; each restorer skips what it already holds")
}

func TestRunnable_CancelledMidPassNeverDeclaresTheRegistryComplete(t *testing.T) {
	reg := tokens.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	// The restorer itself succeeds, but the manager shuts down during the
	// pass: a pass cut short must not end the restoring state.
	r := &fakeRestorer{onCall: cancel}
	run := newRunnable(reg, r)

	require.NoError(t, run.Start(ctx))
	assert.True(t, reg.Restoring(), "a partial registry is never declared complete")
}

// panicRestorer panics on every call.
type panicRestorer struct{}

func (panicRestorer) RestoreTokens(context.Context) (int, error) { panic("unadopted Secret") }

func TestRunnable_PanickingRestorerIsSkippedNotRetried(t *testing.T) {
	reg := tokens.NewRegistry()
	ok := &fakeRestorer{}
	run := newRunnable(reg, panicRestorer{}, ok)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	require.NotPanics(t, func() { require.NoError(t, run.Start(ctx)) })
	assert.False(t, reg.Restoring(), "a panic does not hold the registry restoring")
	assert.Equal(t, 1, ok.calls, "one pass: the panic neither failed it nor stopped the next restorer")
	assert.Less(t, time.Since(start), time.Second, "no retry backoff was waited out")
}

func TestRunnable_GivesUpAfterTheDeadlineSoReconcilesAreNotHeldForever(t *testing.T) {
	reg := tokens.NewRegistry()
	stuck := &fakeRestorer{failures: 1 << 30}
	run := newRunnable(reg, stuck)
	run.GiveUpAfter = 1500 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waited := make(chan error, 1)
	go func() { waited <- reg.WaitRestored(ctx) }()

	require.NoError(t, run.Start(ctx))
	assert.False(t, reg.Restoring(), "a restore that never succeeds still ends, so WaitRestored returns")
	require.NoError(t, <-waited)
	assert.GreaterOrEqual(t, stuck.calls, 2, "it retried before giving up")
}

// hangingRestorer blocks until its ctx is done, like a live GET on a stalled
// apiserver connection.
type hangingRestorer struct{}

func (hangingRestorer) RestoreTokens(ctx context.Context) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestRunnable_AHungPassIsCutOffAtTheGiveUpDeadline(t *testing.T) {
	reg := tokens.NewRegistry()
	run := newRunnable(reg, hangingRestorer{})
	run.GiveUpAfter = 500 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	require.NoError(t, run.Start(ctx))
	assert.False(t, reg.Restoring(), "a pass that never returns still ends the restoring state at the deadline")
	assert.Less(t, time.Since(start), 5*time.Second, "cut off at the give-up deadline, not at the test's timeout")
}
