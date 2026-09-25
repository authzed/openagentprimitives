//go:build integration || e2e

package testspicedb

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
)

// idleGrace is how long the shared container outlives its last user before it is
// purged.
//
// The refcount hits zero in every gap BETWEEN tests, because Go runs the tests
// in a package serially (nothing under test/e2e calls t.Parallel) and each test's
// t.Cleanup releases before the next test's SharedEndpoint acquires. Those gaps
// are sub-millisecond, so any non-trivial grace bridges them; purging at zero
// with no grace would restart a container per test and give back the entire
// point of sharing.
//
// Five seconds is the whole budget: long enough to bridge a gap by four orders
// of magnitude, short enough that a package which finishes does not sit on a
// live container while the rest of the suite competes for the machine.
const idleGrace = 5 * time.Second

var (
	sharedMu    sync.Mutex
	sharedAddr  string
	sharedRes   *dockertest.Resource
	sharedPool  *dockertest.Pool
	sharedUsers int
	sharedIdle  *time.Timer
	sharedFail  error
	sharedTried bool
)

// SharedEndpoint starts ONE spicedb container for the whole test BINARY (lazily,
// on first call) and returns its host:port.
//
// Per-test isolation is unchanged and does not come from the container: each
// test calls WriteSchema with its own UniqueToken, and serve-testing keys
// datastores by bearer token, so different tokens see different datastores on
// the same container. TestSharedEndpointTokenIsolation pins that.
//
// Lifetime is reference-counted: every caller's t.Cleanup releases, and the
// container is purged idleGrace after the LAST release. That makes the fixture
// self-cleaning — a package needs no TestMain for correctness, which matters
// because every one of the 39 test/e2e/scenarios packages is a separate test
// binary and only a handful define one. A package that DOES define TestMain
// should still call StopShared after m.Run() to purge immediately rather than
// wait out the grace period.
//
// If Docker is unreachable the calling test is skipped. If Docker IS reachable
// and the container still fails, the test FAILS — the same policy as Endpoint.
// Skipping the second case is how a suite reports green having run nothing.
func SharedEndpoint(t *testing.T) string {
	t.Helper()

	sharedMu.Lock()
	defer sharedMu.Unlock()

	// Cancel a pending idle purge before inspecting the container: if the timer
	// has already fired and is blocked on sharedMu, Stop returns false and the
	// purge still runs after we return — but it re-checks the refcount under the
	// lock and finds this caller registered, so it is a no-op.
	if sharedIdle != nil {
		sharedIdle.Stop()
		sharedIdle = nil
	}

	if !sharedTried {
		sharedTried = true
		addr, res, pool, err := startContainer()
		sharedAddr, sharedRes, sharedPool, sharedFail = addr, res, pool, err
		if err != nil {
			// Log so the skip below has context; the error is also carried in
			// sharedFail so every later caller reports the same cause rather
			// than a bare "unavailable".
			t.Logf("testspicedb: could not start shared spicedb container: %v", err)
		}
	}
	if errors.Is(sharedFail, ErrDockerUnavailable) {
		// t.Skipf calls runtime.Goexit; the deferred Unlock still runs.
		t.Skipf("skipping: %v", sharedFail)
	}
	if sharedFail != nil || sharedAddr == "" {
		// Docker IS reachable and the container still would not start: FAIL
		// rather than skip. Skipping here is how a suite reports green having
		// run nothing.
		t.Fatalf("testspicedb: docker is reachable but the shared spicedb container would not start: %v", sharedFail)
	}

	sharedUsers++
	t.Cleanup(releaseShared)
	return sharedAddr
}

// releaseShared drops one reference and arms the idle purge when the last one
// goes away.
func releaseShared() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	sharedUsers--
	if sharedUsers > 0 {
		return
	}
	sharedIdle = time.AfterFunc(idleGrace, purgeIfIdle)
}

// purgeIfIdle purges the container unless a new caller acquired it while the
// timer was pending.
func purgeIfIdle() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedUsers > 0 {
		return
	}
	purgeSharedLocked()
}

// StopShared purges the package's shared spicedb container immediately. Safe to
// call when none was started (no-op). Call from TestMain after m.Run() returns
// so the container is gone before the process exits, instead of lingering for
// idleGrace.
func StopShared() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedIdle != nil {
		sharedIdle.Stop()
		sharedIdle = nil
	}
	purgeSharedLocked()
}

// purgeSharedLocked purges the container and resets the fixture so a later
// SharedEndpoint call starts a fresh one. Callers must hold sharedMu.
func purgeSharedLocked() {
	if sharedRes == nil || sharedPool == nil {
		return
	}
	if err := sharedPool.Purge(sharedRes); err != nil {
		// Reachable from TestMain and from a timer goroutine, both outside any
		// *testing.T — stderr is the only surface. Never drop it silently
		// (AGENTS.md): a purge failure is exactly how a container graveyard
		// accumulates.
		fmt.Fprintf(os.Stderr, "testspicedb: purge shared container: %v\n", err)
	}
	sharedRes, sharedPool, sharedAddr = nil, nil, ""
	sharedTried, sharedFail = false, nil
}
