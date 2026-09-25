package suitelock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helperHold is the env var that turns this test binary into a lock-holding
// child process (see TestMain-free re-exec pattern in TestBlocksUntilReleased).
const helperHold = "SUITELOCK_TEST_HOLD"

// lockPath gives each test its own lock file so tests never contend with each
// other — or with a real suite run on the developer's machine.
func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "suite.lock")
}

func TestAcquireCreatesTheLockFileAndReleaseCleansUp(t *testing.T) {
	path := lockPath(t)

	rel, err := Acquire(Options{Path: path, Holder: "unit-test", Wait: time.Second})
	require.NoError(t, err, "an uncontended lock must be acquired immediately")
	require.NotNil(t, rel)

	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "the lock file must exist while held")

	assert.NoError(t, rel())
	// The file deliberately OUTLIVES the release: flock's exclusion lives on the
	// descriptor, not the path, so unlinking it would let a waiter open a fresh
	// inode and acquire a lock nobody else can see.
	_, statErr = os.Stat(path)
	assert.NoError(t, statErr, "the lock file is reused, not deleted, across runs")
}

func TestSecondAcquireInTheSameProcessDoesNotDeadlock(t *testing.T) {
	// flock is per-open-file-description, so a second Acquire in the same process
	// opens its own descriptor and genuinely contends. This pins that we do not
	// silently succeed, which would defeat the whole purpose within one process.
	path := lockPath(t)

	rel, err := Acquire(Options{Path: path, Holder: "first", Wait: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rel() })

	_, err = Acquire(Options{Path: path, Holder: "second", Wait: 300 * time.Millisecond})
	require.Error(t, err, "a second acquire must not succeed while the first is held")
	assert.ErrorIs(t, err, ErrTimeout)
}

func TestTimeoutErrorNamesTheHolderSoAWaiterKnowsWhoToBlame(t *testing.T) {
	path := lockPath(t)

	rel, err := Acquire(Options{Path: path, Holder: "worktree-alpha test:e2e", Wait: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rel() })

	_, err = Acquire(Options{Path: path, Holder: "worktree-beta", Wait: 200 * time.Millisecond})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree-alpha test:e2e",
		"the timeout must name the holder — a waiter that cannot see who holds the lock cannot act on it")
}

func TestBlocksUntilReleasedRatherThanFailingImmediately(t *testing.T) {
	path := lockPath(t)

	rel, err := Acquire(Options{Path: path, Holder: "holder", Wait: time.Second})
	require.NoError(t, err)

	// Release shortly; the waiter below must succeed by blocking, not by failing.
	go func() {
		time.Sleep(250 * time.Millisecond)
		_ = rel()
	}()

	start := time.Now()
	rel2, err := Acquire(Options{Path: path, Holder: "waiter", Wait: 10 * time.Second})
	require.NoError(t, err, "the waiter must block until the holder releases, not give up")
	t.Cleanup(func() { _ = rel2() })

	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond,
		"acquiring must actually have waited — a zero-wait success means exclusion is not working")
}

func TestOSReleasesTheLockWhenTheHolderProcessDies(t *testing.T) {
	// The reason for flock over a lockdir: a suite killed with Ctrl-C must not
	// wedge every other worktree behind a stale lock file.
	if os.Getenv(helperHold) != "" {
		return // child mode handled by the helper binary below
	}
	path := lockPath(t)

	// Re-exec this test binary as a child that grabs the lock and sleeps.
	child := exec.Command(os.Args[0], "-test.run=TestHelperHoldsLockForever")
	child.Env = append(os.Environ(), helperHold+"="+path)
	require.NoError(t, child.Start())

	// Wait for the child to actually hold it. Note the probe MUST release when it
	// wins, or it becomes the holder itself and the assertion below fails against
	// our own leaked lock rather than the child's.
	deadline := time.Now().Add(5 * time.Second)
	childHolds := false
	for time.Now().Before(deadline) {
		rel, err := Acquire(Options{Path: path, Holder: "probe", Wait: 50 * time.Millisecond})
		if errors.Is(err, ErrTimeout) {
			childHolds = true
			break
		}
		if err == nil {
			_ = rel() // child not up yet — give the lock straight back
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, childHolds, "child never acquired the lock; the kill below would prove nothing")

	require.NoError(t, child.Process.Kill())
	_, _ = child.Process.Wait()

	rel, err := Acquire(Options{Path: path, Holder: "after-kill", Wait: 5 * time.Second})
	require.NoError(t, err, "the kernel must drop the flock when the holder dies; a stale lock would wedge every worktree")
	assert.NoError(t, rel())
}

// TestHelperHoldsLockForever is not a real test: it is the child-process body
// for TestOSReleasesTheLockWhenTheHolderProcessDies. It exits immediately unless
// re-exec'd with the helper env var set.
func TestHelperHoldsLockForever(t *testing.T) {
	path := os.Getenv(helperHold)
	if path == "" {
		t.Skip("child-process helper; not run directly")
	}
	if _, err := Acquire(Options{Path: path, Holder: "child", Wait: 5 * time.Second}); err != nil {
		os.Exit(1)
	}
	time.Sleep(2 * time.Minute) // parent kills us
}

func TestDisabledSkipsLockingEntirely(t *testing.T) {
	path := lockPath(t)

	rel, err := Acquire(Options{Path: path, Holder: "first", Wait: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rel() })

	// With Disabled set, a second acquire succeeds despite the held lock — the
	// CI escape hatch, where runs are already isolated per machine.
	rel2, err := Acquire(Options{Path: path, Holder: "ci", Disabled: true, Wait: time.Millisecond})
	require.NoError(t, err, "Disabled must bypass the lock rather than wait")
	assert.NoError(t, rel2())
}

func TestHolderDescriptionIsWrittenForHumansToRead(t *testing.T) {
	path := lockPath(t)

	rel, err := Acquire(Options{Path: path, Holder: "worktree-gamma test:integration", Wait: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rel() })

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(b)
	assert.Contains(t, body, "worktree-gamma test:integration")
	assert.Contains(t, body, "pid=", "the holder record must carry a pid so a wedged run can be found and killed")
	assert.False(t, strings.HasPrefix(body, "\x00"), "holder record must be readable text, not binary")
}
