// Package suitelock serializes the heavyweight test suites across every git
// worktree of a checkout.
//
// `mage test:integration` and `mage test:e2e` both stand up envtest control
// planes and SpiceDB containers. Those are machine-wide resources, so two runs
// started from different worktrees do not merely take turns — they starve each
// other, and the resulting failures are TIMEOUT-shaped ("no X within 30s") in
// whichever tests happened to lose the race. Three consecutive full e2e runs of
// one green tree failed on three DISJOINT sets of tests, every one passing in
// isolation at a fraction of the runtime.
//
// That is worse than slow. A moving set of timeout failures is
// indistinguishable, without re-running each test alone, from a genuine
// regression — so the most expensive suite in the ship gate becomes its least
// trustworthy signal, and the natural reaction ("probably flaky, re-run it") is
// exactly the habit that lets a real break through.
//
// The fix is an advisory file lock held for the duration of a suite, so a
// second run waits instead of interfering.
//
// flock is used rather than a lock directory or a pid file precisely because
// the kernel drops it when the holding process dies. These suites take ten
// minutes and get interrupted; a lock that survived a Ctrl-C would wedge every
// other worktree on the machine until someone found and deleted it.
package suitelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ErrTimeout is returned when the lock could not be acquired within
// Options.Wait. It wraps a description of the current holder.
var ErrTimeout = errors.New("timed out waiting for the test-suite lock")

// pollInterval is how often a waiter retries the non-blocking flock. It is a
// poll rather than a blocking LOCK_EX so the waiter can report progress and
// honor its own deadline; at ten-minute suite durations the wasted wakeups are
// irrelevant.
const pollInterval = 250 * time.Millisecond

// Options configures a single Acquire call.
type Options struct {
	// Path is the lock file. It must be shared by every worktree that should
	// serialize — see DefaultPath.
	Path string

	// Holder describes who is taking the lock, for the benefit of whoever ends
	// up waiting on it. Include the worktree and the target, e.g.
	// "cluster-kind-registry test:e2e".
	Holder string

	// Wait bounds how long to block. On expiry Acquire returns ErrTimeout naming
	// the current holder. A bound rather than infinite patience is deliberate:
	// one wedged run must not be able to freeze every other session forever.
	Wait time.Duration

	// Disabled bypasses locking entirely and returns a no-op release. For CI,
	// where each run already owns its machine and the wait is pure cost.
	Disabled bool

	// Notify, when set, is called once per poll while blocked, with the current
	// holder record and how long this call has been waiting. Used by the mage
	// targets to make a waiting run visibly waiting rather than hung.
	Notify func(holder string, waited time.Duration)
}

// ReleaseFunc drops the lock. It is safe to call more than once.
type ReleaseFunc func() error

// DefaultPath returns the lock file shared by every worktree of one clone:
// a file inside the common git directory.
//
// The common git dir is the right scope because that is precisely the set of
// checkouts that share a machine's envtest and Docker state in practice — this
// repo routinely has twenty-odd worktrees against one clone. It also keeps the
// lock out of every working tree, so it can never be committed, and it
// disappears with the clone.
//
// gitCommonDir is injected so callers can supply `git rev-parse
// --git-common-dir` without this package shelling out to git.
func DefaultPath(gitCommonDir string) string {
	return filepath.Join(gitCommonDir, "ap-suite.lock")
}

// Acquire takes the exclusive suite lock, blocking up to opts.Wait.
//
// The lock file is created if absent and is NEVER unlinked — flock's exclusion
// lives on the open file description, not on the path, so deleting it would let
// a waiter open a fresh inode and take a lock that the current holder cannot
// see. A zero-length leftover file is the intended steady state.
func Acquire(opts Options) (ReleaseFunc, error) {
	if opts.Disabled {
		return func() error { return nil }, nil
	}
	if opts.Path == "" {
		return nil, errors.New("suitelock: Options.Path is required")
	}
	if opts.Wait <= 0 {
		opts.Wait = time.Minute
	}

	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o755); err != nil {
		return nil, fmt.Errorf("suitelock: prepare lock directory: %w", err)
	}
	f, err := os.OpenFile(opts.Path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("suitelock: open %s: %w", opts.Path, err)
	}

	deadline := time.Now().Add(opts.Wait)
	start := time.Now()
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			// Won the lock. Record who we are for the next waiter, best-effort:
			// failing to write the courtesy note must not fail the suite.
			writeHolder(f, opts.Holder)
			return releaser(f), nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("suitelock: flock %s: %w", opts.Path, err)
		}
		if time.Now().After(deadline) {
			held := readHolder(opts.Path)
			f.Close()
			return nil, fmt.Errorf("%w after %s (held by %s)", ErrTimeout, opts.Wait.Round(time.Second), held)
		}
		if opts.Notify != nil {
			opts.Notify(readHolder(opts.Path), time.Since(start))
		}
		time.Sleep(pollInterval)
	}
}

// releaser returns a ReleaseFunc that unlocks and closes exactly once.
func releaser(f *os.File) ReleaseFunc {
	released := false
	return func() error {
		if released {
			return nil
		}
		released = true
		// Closing the descriptor releases the flock on its own; unlocking first
		// makes the intent explicit and is harmless.
		unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
		closeErr := f.Close()
		if unlockErr != nil {
			return fmt.Errorf("suitelock: unlock: %w", unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("suitelock: close: %w", closeErr)
		}
		return nil
	}
}

// writeHolder records the current holder as readable text. Best-effort: a
// failure here costs a waiter some context, not the run.
func writeHolder(f *os.File, holder string) {
	if holder == "" {
		holder = "unknown"
	}
	rec := fmt.Sprintf("%s pid=%d since=%s\n", holder, os.Getpid(), time.Now().Format(time.RFC3339))
	if err := f.Truncate(0); err != nil {
		return
	}
	if _, err := f.WriteAt([]byte(rec), 0); err != nil {
		return
	}
	_ = f.Sync()
}

// readHolder reads the holder record for an error or progress message. It never
// fails: an unreadable or empty record just yields a placeholder, because this
// runs on the path where something has already gone slightly wrong.
func readHolder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "an unknown process"
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "an unknown process"
	}
	return s
}
