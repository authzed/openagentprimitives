//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// dockerPathMarkers are substrings that, if present in ANY file a candidate
// process holds open, identify it as Docker Desktop's own VM process rather
// than one of ours. Docker Desktop for Mac's own VM is ALSO a
// com.apple.Virtualization.VirtualMachine process (same as ours) and,
// through its virtiofs bind-mount of the user's home directory, can
// legitimately have OUR staged rootfs.img open too — indistinguishable from
// a genuine orphan by the rootfs check alone. Killing it crashes Docker
// Desktop entirely; this has happened twice in development. Any candidate
// that also touches one of these paths is never killed, no exceptions.
var dockerPathMarkers = []string{"Docker.raw", "Docker.app", "com.docker"}

// holdsPath reports whether openFiles (as lsof -Fpn reports them — plain
// paths, or "<path> (deleted)" for a stale handle to a since-removed or
// since-restaged inode — see ensureDiskImage/shouldRestageDiskImage, which
// is exactly what leaves an orphaned VM process holding a deleted rootfs.img
// handle) includes target.
func holdsPath(openFiles []string, target string) bool {
	for _, f := range openFiles {
		if strings.TrimSuffix(f, " (deleted)") == target {
			return true
		}
	}
	return false
}

// holdsAnyDockerPath reports whether openFiles includes any path matching
// dockerPathMarkers.
func holdsAnyDockerPath(openFiles []string) bool {
	for _, f := range openFiles {
		for _, marker := range dockerPathMarkers {
			if strings.Contains(f, marker) {
				return true
			}
		}
	}
	return false
}

// shouldKillOrphan is the pure, unit-testable safety decision behind the
// startup orphan-VM cleanup (see killOrphanVMs): a candidate process is safe
// to kill only if it holds OUR staged rootfs image open (ourRootfsPath, as
// it appears in that process's lsof -Fpn output — a stale "<path> (deleted)"
// entry counts) AND it holds NO Docker-associated path open. See
// dockerPathMarkers's doc comment for why the Docker guard is
// safety-critical and non-negotiable — it is what distinguishes a genuinely
// orphaned `oap desktop` VM from Docker Desktop's own VM incidentally
// surfacing the same rootfs path through its virtiofs /Users mount.
func shouldKillOrphan(openFiles []string, ourRootfsPath string) bool {
	return holdsPath(openFiles, ourRootfsPath) && !holdsAnyDockerPath(openFiles)
}

// orphanKillGrace bounds how long killOrphanVMs waits after SIGTERM before
// escalating a still-alive candidate to SIGKILL.
const orphanKillGrace = 3 * time.Second

// orphanLSOFTimeout bounds the lsof scan killOrphanVMs runs at startup —
// orphan cleanup is best-effort and must never hang `oap desktop` bring-up.
const orphanLSOFTimeout = 10 * time.Second

// killOrphanVMs finds and terminates VM processes left behind by a prior
// crashed `oap desktop` run, before this run provisions its own VM.
// Virtualization.framework runs the actual guest as a SEPARATE macOS
// process (com.apple.Virtualization.VirtualMachine) from the `oap` process
// hosting it, so a crash of the `oap` process (as opposed to a clean Quit,
// which already routes through eng.Down/vz.Provider.Stop) leaves that VM
// process running indefinitely, still holding the staged rootfs.img file
// open.
//
// Best-effort: a failure to enumerate VM processes or run lsof at all is
// logged and treated as "found nothing" — startup must not hard-fail just
// because orphan discovery couldn't run. Every candidate that holds
// ourRootfsPath open — whether killed or skipped under the Docker guard — is
// logged with its pid and the reason; processes that don't hold ourRootfsPath
// at all are not logged, there would be one line per unrelated process
// otherwise.
//
// A cheap pgrep gate runs FIRST (see virtualizationVMPIDs): an orphan is by
// definition a running Virtualization VM process, and a full-system
// `lsof -Fpn` scans tens of thousands of open files — >10s on a busy machine,
// which would blow orphanLSOFTimeout and skip cleanup entirely (defeating the
// whole point). With no VM process running there is nothing to clean up, so we
// return in milliseconds; when some exist, the lsof is scoped to just those
// PIDs (still fast, still sees deleted rootfs handles + Docker markers — see
// lsofOpenFilesByPID). The Docker guard and deleted-handle detection in
// shouldKillOrphan are unchanged.
//
// status, if non-nil, receives short human-readable phase labels
// ("Checking for existing VM…", "Removing stale VM…") for the setup UI to
// surface as sub-status on the "Initializing" timeline row. It is best-effort
// UI feedback, decoupled from logf (which is the durable operator record); a
// nil status is fine (used by tests and any non-desktop caller).
func killOrphanVMs(ctx context.Context, ourRootfsPath string, logf func(format string, args ...any), status func(string)) {
	if status == nil {
		status = func(string) {}
	}
	status("Checking for existing VM…")

	scanCtx, cancel := context.WithTimeout(ctx, orphanLSOFTimeout)
	defer cancel()

	// Fast gate: list Virtualization VM PIDs (~ms) before the far pricier lsof.
	pids, err := virtualizationVMPIDs(scanCtx)
	if err != nil {
		logf("orphan cleanup: listing VM processes failed, skipping: %v", err)
		return
	}
	if len(pids) == 0 {
		return // no VM process at all → no orphan possible; skip lsof entirely
	}

	byPID, err := lsofOpenFilesByPID(scanCtx, pids...)
	if err != nil {
		logf("orphan cleanup: lsof failed, skipping: %v", err)
		return
	}

	selfPID := os.Getpid()
	for pid, files := range byPID {
		if pid == selfPID || !holdsPath(files, ourRootfsPath) {
			continue
		}
		if !shouldKillOrphan(files, ourRootfsPath) {
			logf("orphan cleanup: SKIP pid %d — holds %s but also holds a Docker-associated path (Docker Desktop's own VM surfaces our files via virtiofs; never killed)", pid, ourRootfsPath)
			continue
		}
		logf("orphan cleanup: KILL pid %d — holds %s, no Docker path (orphaned VM from a prior crashed run)", pid, ourRootfsPath)
		status("Removing stale VM…")
		killProcessGracefully(pid, orphanKillGrace, logf)
	}
}

// virtualizationVMProcName is the exact process name of the separate macOS
// helper Virtualization.framework runs each guest as — both ours and Docker
// Desktop's own VM appear under this name. pgrep -x matches it exactly.
const virtualizationVMProcName = "com.apple.Virtualization.VirtualMachine"

// virtualizationVMPIDs returns the PIDs of every running Virtualization VM
// helper process, the cheap pre-filter for killOrphanVMs's scoped lsof. pgrep
// exits 1 with no output when nothing matches — the common "no VM running"
// case — which is reported as an empty slice, not an error; only a genuine
// exec failure (or exit >1) is an error.
func virtualizationVMPIDs(ctx context.Context) ([]int, error) {
	out, err := exec.CommandContext(ctx, "pgrep", "-x", virtualizationVMProcName).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return nil, nil // pgrep's "no matches" — not an error
		}
		return nil, fmt.Errorf("pgrep -x %s: %w", virtualizationVMProcName, err)
	}
	return parsePgrepPIDs(string(out)), nil
}

// parsePgrepPIDs parses pgrep's newline-separated PID output; non-numeric or
// blank lines are skipped defensively.
func parsePgrepPIDs(out string) []int {
	var pids []int
	for _, line := range strings.Fields(out) {
		if pid, err := strconv.Atoi(line); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// lsofOpenFilesByPID shells out to `lsof -Fpn` and groups every open file's
// path by owning pid. When pids are supplied it scopes the scan to just those
// processes (`lsof -p <csv>`), which killOrphanVMs uses to avoid a full-system
// scan; with none it scans every process.
//
// Note the scoping is by PID, not by directory: a directory-scoped lsof (e.g.
// `+D`) would miss a deleted rootfs.img handle from an orphaned VM, since a
// deleted file no longer appears in any directory listing lsof could walk.
// PID scoping reads each process's open-file table directly, so it still
// surfaces "<path> (deleted)" handles — the whole point of the orphan check.
func lsofOpenFilesByPID(ctx context.Context, pids ...int) (map[int][]string, error) {
	args := []string{"-Fpn"}
	if len(pids) > 0 {
		args = append(args, "-p", joinInts(pids, ","))
	}
	out, err := exec.CommandContext(ctx, "lsof", args...).Output()
	if err != nil {
		// lsof commonly exits non-zero when it can't inspect every process on
		// the system (permissions), while still writing everything it COULD
		// inspect to stdout — only treat this as a hard failure when there's
		// no output to parse at all.
		if len(out) == 0 {
			return nil, fmt.Errorf("lsof %v: %w", args, err)
		}
	}
	return parseLsofPidNameOutput(string(out)), nil
}

// joinInts renders ints as sep-joined text (e.g. "12,34,56") for lsof -p.
func joinInts(nums []int, sep string) string {
	strs := make([]string, len(nums))
	for i, n := range nums {
		strs[i] = strconv.Itoa(n)
	}
	return strings.Join(strs, sep)
}

// parseLsofPidNameOutput parses `lsof -Fpn` field-mode output into
// pid -> open file paths. Format: each process starts with a "p<pid>" line;
// every "n<name>" line until the next "p" line is one of that process's open
// files.
func parseLsofPidNameOutput(out string) map[int][]string {
	result := map[int][]string{}
	var (
		curPID     int
		haveCurPID bool
	)
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, err := strconv.Atoi(line[1:])
			haveCurPID = err == nil
			if haveCurPID {
				curPID = pid
			}
		case 'n':
			if haveCurPID {
				result[curPID] = append(result[curPID], line[1:])
			}
		}
	}
	return result
}

// killProcessGracefully sends SIGTERM to pid, polls (rather than sleeping
// the full grace period) for it to exit, and escalates to SIGKILL if it's
// still alive once grace elapses. Every step that can fail is logged, per
// this repo's no-silent-errors convention — orphan cleanup runs unattended
// at startup, so a swallowed failure here would be invisible.
func killProcessGracefully(pid int, grace time.Duration, logf func(format string, args ...any)) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		logf("orphan cleanup: find pid %d: %v", pid, err)
		return
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		logf("orphan cleanup: SIGTERM pid %d: %v", pid, err)
		return
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return // exited
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return // exited during the final check
	}
	logf("orphan cleanup: pid %d still alive %s after SIGTERM, sending SIGKILL", pid, grace)
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		logf("orphan cleanup: SIGKILL pid %d: %v", pid, err)
	}
}
