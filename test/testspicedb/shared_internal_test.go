//go:build integration

package testspicedb

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProcessAlive is the whole correctness of reapAbandoned in one predicate:
// answer "alive" for a dead PID and the reaper never reaps, silently, forever —
// which is what happened when it compared only against syscall.ESRCH, because
// os.Process.Signal translates that to os.ErrProcessDone. Answer "dead" for a
// live PID and it removes a container another suite is using.
func TestProcessAlive(t *testing.T) {
	// A real, definitely-exited process: spawn one and wait for it. Its PID is
	// then gone, and this is reaped-owner shaped — not a made-up number.
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	deadPID := cmd.Process.Pid
	require.NoError(t, cmd.Wait())

	cases := []struct {
		name string
		pid  int
		want bool
	}{
		{name: "own pid: alive", pid: os.Getpid(), want: true},
		{name: "pid 1 (init, not ours — EPERM): alive", pid: 1, want: true},
		{name: "exited child pid: dead, so its containers are reapable", pid: deadPID, want: false},
		{name: "zero pid: dead (an unset owner label parses to 0)", pid: 0, want: false},
		{name: "negative pid: dead, never signalled as a process group", pid: -1, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, processAlive(tc.pid))
		})
	}
}

// containerID returns the shared container's docker ID, or "" when none is
// running. Reads the fixture globals under the same lock the fixture uses.
func containerID() string {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedRes == nil {
		return ""
	}
	return sharedRes.Container.ID
}

// TestSharedContainerSurvivesTheGapBetweenTests is the invariant that makes
// sharing worth anything: nothing under test/e2e calls t.Parallel, so the
// reference count hits zero in every gap between two tests. If the fixture
// purged at zero rather than after a grace period, each test would start its own
// container and the shared model would be per-test containers wearing a
// different name.
func TestSharedContainerSurvivesTheGapBetweenTests(t *testing.T) {
	var first, firstID string
	t.Run("first holder acquires the container", func(t *testing.T) {
		first = SharedEndpoint(t)
		firstID = containerID()
		require.NotEmpty(t, firstID, "a shared container must be running while a test holds it")
	})
	// The subtest's t.Cleanup has run, so the refcount is zero and the idle
	// purge is armed — but not yet fired.

	var second, secondID string
	t.Run("next holder reuses the same container", func(t *testing.T) {
		second = SharedEndpoint(t)
		secondID = containerID()
	})

	assert.Equal(t, first, second, "consecutive tests must share one endpoint")
	assert.Equal(t, firstID, secondID, "consecutive tests must share one container, not two")
}

// TestSharedContainerIsPurgedAfterTheLastReleaser is the other half: the fixture
// must clean up on its own, WITHOUT a TestMain. Every one of the 39
// test/e2e/scenarios packages is a separate test binary and only a handful define
// one; if the container outlived its users, adopting SharedEndpoint across the
// suite would trade many short-lived containers for many long-lived ones, which
// is strictly worse for a machine running the tiers in parallel.
func TestSharedContainerIsPurgedAfterTheLastReleaser(t *testing.T) {
	var held string
	t.Run("holder acquires the container", func(t *testing.T) {
		SharedEndpoint(t)
		held = containerID()
		require.NotEmpty(t, held)
	})

	// Wait out the grace period with margin for a loaded docker daemon.
	deadline := time.Now().Add(idleGrace + 25*time.Second)
	for time.Now().Before(deadline) && containerID() != "" {
		time.Sleep(100 * time.Millisecond)
	}
	assert.Empty(t, containerID(),
		"the shared container must be purged within the idle grace period after its last user releases")

	// And a later caller transparently gets a NEW container rather than a dead
	// endpoint — the reason purging is safe in the first place.
	revived := SharedEndpoint(t)
	assert.NotEmpty(t, revived)
	assert.NotEqual(t, held, containerID(), "a post-purge acquire must start a fresh container")
}
