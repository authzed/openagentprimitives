// Package envtestreap kills envtest apiserver/etcd processes that outlived the
// test binary that started them.
//
// controller-runtime's envtest starts a kube-apiserver + etcd pair per
// Environment and relies on Environment.Stop to kill them. Stop never runs when
// the test binary dies without unwinding — a `go test -timeout` kill, a panic in
// another goroutine, a Ctrl-C — and the pair is then reparented to PID 1 and
// keeps running until the machine reboots.
//
// They are not idle. Each pair burns CPU continuously, and because nothing ever
// reports them they accumulate across runs: we found 21 of them on one
// development machine, the oldest 28 days old. Long before anyone notices the
// load, poll-based tests start missing deadlines they clear comfortably in
// isolation — which is indistinguishable from flakiness and is what "just re-run
// it isolated" has really been papering over.
//
// Reaping is therefore a precondition of a trustworthy suite, not housekeeping:
// `mage test:integration` / `test:e2e` sweep before running so a suite always
// starts on an unloaded machine.
package envtestreap

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// assetMarker identifies an envtest-managed binary by the asset directory
// setup-envtest installs into. Matching the PATH rather than the process name
// is deliberate: plain "etcd" or "kube-apiserver" would also match a real etcd
// or a developer's own cluster, and this code sends SIGKILL.
const assetMarker = "io.kubebuilder.envtest"

// Orphans returns the PIDs in `ps -ax -o pid,ppid,command` output that are
// envtest binaries reparented to PID 1.
//
// Two conditions, both required. PPID 1 means the test binary that owned the
// process is already gone, so nothing can still be using it — an envtest process
// whose parent is alive belongs to a RUNNING suite and must be left alone, or
// the reaper would kill the very run it is trying to protect. The asset-path
// marker keeps the sweep to processes envtest itself installed.
//
// Rows that do not parse are skipped rather than fatal: this runs before every
// suite, so it must not be able to break the build.
func Orphans(psOutput string) []int {
	var pids []int
	for _, line := range strings.Split(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue // header row, or output we do not understand
		}
		if ppid, err := strconv.Atoi(fields[1]); err != nil || ppid != 1 {
			continue
		}
		if !strings.Contains(line, assetMarker) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// Reap kills every orphan and reports how many it killed.
//
// Best-effort by design: an unavailable `ps`, or a PID that exits between the
// listing and the kill, returns what was reaped rather than failing the caller —
// a suite must still run on a machine where the sweep could not. The error is
// returned (never swallowed) so the caller can report it.
func Reap() (int, error) {
	out, err := exec.Command("ps", "-ax", "-o", "pid,ppid,command").Output()
	if err != nil {
		return 0, fmt.Errorf("list processes: %w", err)
	}
	killed := 0
	for _, pid := range Orphans(string(out)) {
		// SIGKILL, not SIGTERM: these are already unparented and unreachable by
		// the code that knows how to shut them down gracefully.
		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed++
		}
	}
	return killed, nil
}
