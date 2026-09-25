// This file carries NO build tag, like labels.go, because two callers that
// never link each other have to reap by exactly the same rules: the fixture
// itself (tagged `integration || e2e`, sweeping on every container start) and
// the magefile (untagged, sweeping after a suite finishes). A second copy of
// these rules is how the one safety property that matters — never touch a
// container this fixture did not start — silently stops holding on one path.

package testspicedb

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

// ErrDockerUnavailable marks the one condition these tests may legitimately
// skip on: no reachable Docker daemon, so the machine cannot run them at all.
// Any other startup failure is a real defect and fails the test. Reap returns
// it too, so a sweep on a machine with Docker stopped stays quiet instead of
// warning about containers that cannot exist.
var ErrDockerUnavailable = errors.New("testspicedb: no reachable docker daemon")

// Container is the slice of a docker container listing the reaper reasons
// about. Taking a struct rather than docker.APIContainers keeps the decision
// rules testable without a daemon, which is what lets the safety properties be
// asserted at all.
type Container struct {
	ID     string
	Name   string
	Labels map[string]string
}

// OwnerStatus is what the reaper could establish about the test process that
// started a container. Only OwnerGone and OwnerRecycled permit removal; both
// of the other two mean "someone may still be using this", and the reaper
// leaves those alone.
type OwnerStatus string

const (
	// OwnerLive: the PID is held by the very process that started the
	// container. A suite running right now, possibly in another worktree.
	OwnerLive OwnerStatus = "live"
	// OwnerGone: no process holds the PID. The container is abandoned.
	OwnerGone OwnerStatus = "gone"
	// OwnerRecycled: a process holds the PID, but it started at a different
	// time than the one that created the container — so the creator is dead
	// and the kernel handed its PID to someone else. Abandoned.
	OwnerRecycled OwnerStatus = "recycled"
	// OwnerUnknown: a process holds the PID and its identity could not be
	// confirmed (no start token on the container, or no readable process
	// table). Treated as live: every unresolvable case must err toward
	// leaving a container in place.
	OwnerUnknown OwnerStatus = "unknown"
)

// OwnerChecker reports the state of the process that started a container,
// given the PID and start token stamped onto it. Injectable so the decision
// rules can be tested against every owner state without arranging real
// processes in those states.
type OwnerChecker func(pid int, startedAt string) OwnerStatus

// Decision records what the reaper resolved to do about one container and why.
// The reason is carried even for containers left in place, so a caller that
// still sees a leak can say which rule spared it rather than reporting a bare
// count nobody can act on.
type Decision struct {
	ID     string
	Name   string
	Reap   bool
	Reason string
	// Err is set when the reaper decided to remove this container and the
	// removal itself failed. Such a decision is reported as kept, because the
	// container is still there.
	Err error
}

// Classify decides, for each listed container, whether it may be removed.
//
// The rules, in the order they are applied and roughly in order of how badly
// each would go wrong if it were dropped:
//
//   - A container without this fixture's own label is NEVER a candidate,
//     whatever its image or name. Developers run their own SpiceDB on the same
//     machine and off the same image; the label is the only thing that
//     distinguishes a test leftover from someone's working stack. This is
//     checked here as well as in the daemon-side list filter, so the property
//     holds even if a caller lists containers some other way.
//   - A container owned by THIS process is never a candidate.
//   - A container carrying ownRunID was started by the caller's own, now
//     finished, `go test` invocation: every binary that could still be using
//     it has exited, so it is removable without consulting owner liveness at
//     all. Pass "" to disable this (the in-process fixture has no finished run
//     to speak for).
//   - Otherwise the owner process decides, and anything short of a definite
//     "that process is gone" leaves the container alone.
func Classify(containers []Container, self int, ownRunID string, owner OwnerChecker) []Decision {
	out := make([]Decision, 0, len(containers))
	for _, c := range containers {
		out = append(out, classifyOne(c, self, ownRunID, owner))
	}
	return out
}

func classifyOne(c Container, self int, ownRunID string, owner OwnerChecker) Decision {
	d := Decision{ID: c.ID, Name: c.Name}

	if c.Labels[LabelFixture] != LabelFixtureValue {
		d.Reason = fmt.Sprintf("not a fixture container: no %s=%s label", LabelFixture, LabelFixtureValue)
		return d
	}

	rawPID, hasPID := c.Labels[LabelOwnerPID]
	pid, pidErr := strconv.Atoi(rawPID)
	if hasPID && pidErr == nil && pid == self {
		d.Reason = fmt.Sprintf("owner pid %d is this process", pid)
		return d
	}

	if ownRunID != "" && c.Labels[LabelRunID] == ownRunID {
		d.Reap = true
		d.Reason = fmt.Sprintf("started by this run (%s=%s), which has finished", LabelRunID, ownRunID)
		return d
	}

	if !hasPID {
		d.Reason = fmt.Sprintf("no %s label: cannot tell an abandoned container from one in use", LabelOwnerPID)
		return d
	}
	if pidErr != nil {
		d.Reason = fmt.Sprintf("owner pid label %q does not parse: %v", rawPID, pidErr)
		return d
	}

	startedAt := c.Labels[LabelOwnerStart]
	switch owner(pid, startedAt) {
	case OwnerGone:
		d.Reap = true
		d.Reason = fmt.Sprintf("owner pid %d is gone", pid)
	case OwnerRecycled:
		d.Reap = true
		d.Reason = fmt.Sprintf("owner pid %d was reused: it now holds a process started at a different time than %q", pid, startedAt)
	case OwnerUnknown:
		d.Reason = fmt.Sprintf("owner pid %d holds a process whose identity could not be confirmed", pid)
	default: // OwnerLive
		d.Reason = fmt.Sprintf("owner pid %d is still running", pid)
	}
	return d
}

// DockerAPI is the slice of the docker client the reaper uses. *docker.Client
// satisfies it; a test can supply a fake and assert on exactly which
// containers were asked to be removed.
type DockerAPI interface {
	ListContainers(docker.ListContainersOptions) ([]docker.APIContainers, error)
	RemoveContainer(docker.RemoveContainerOptions) error
}

// ReapResult separates what the sweep actually removed from what it left in
// place. Kept is not noise: a caller that still observes a leak reports these
// reasons, so "declined to remove" is never indistinguishable from "did not
// notice".
type ReapResult struct {
	Removed []Decision
	Kept    []Decision
}

// ReapWith sweeps the fixture's containers through api, applying Classify.
//
// The daemon-side list is filtered on the fixture label as well, so an
// unlabelled container is not even fetched — but Classify re-checks it, and
// removal is driven only by Classify's verdict. Nothing here can remove a
// container by image, by name, or in bulk.
func ReapWith(api DockerAPI, self int, ownRunID string, owner OwnerChecker) (ReapResult, error) {
	listed, err := api.ListContainers(docker.ListContainersOptions{
		// All, not just running: dockertest's Expire backstop STOPS containers
		// rather than removing them, so a killed run leaves Exited(137)
		// leftovers that accumulate in `docker ps -a` forever.
		All:     true,
		Filters: map[string][]string{"label": {LabelFixture + "=" + LabelFixtureValue}},
	})
	if err != nil {
		return ReapResult{}, fmt.Errorf("list fixture containers: %w", err)
	}

	containers := make([]Container, 0, len(listed))
	for _, c := range listed {
		containers = append(containers, Container{ID: c.ID, Name: containerName(c), Labels: c.Labels})
	}

	var res ReapResult
	for _, d := range Classify(containers, self, ownRunID, owner) {
		if !d.Reap {
			res.Kept = append(res.Kept, d)
			continue
		}
		err := api.RemoveContainer(docker.RemoveContainerOptions{
			ID: d.ID, Force: true, RemoveVolumes: true,
		})
		var gone *docker.NoSuchContainer
		if err != nil && !errors.As(err, &gone) {
			// Report it as kept, because it is: the caller's leak count will
			// still include it and now carries the reason it survived.
			d.Err = err
			res.Kept = append(res.Kept, d)
			continue
		}
		// A NoSuchContainer means another sweep won the race; the container is
		// gone either way, which is all this result claims.
		res.Removed = append(res.Removed, d)
	}
	return res, nil
}

// Reap connects to Docker and sweeps this fixture's abandoned containers,
// plus — when ownRunID is non-empty — every container the caller's own
// finished `go test` invocation started.
//
// Returns ErrDockerUnavailable (wrapped) when there is no daemon, so a caller
// can stay quiet on a machine where the suite skipped its SpiceDB tests
// anyway.
func Reap(ownRunID string) (ReapResult, error) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		return ReapResult{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	return ReapWith(pool.Client, os.Getpid(), ownRunID, LiveOwnerChecker())
}

// containerName returns a container's primary name without the docker API's
// leading slash, for reporting only.
func containerName(c docker.APIContainers) string {
	if len(c.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// SelfStartToken returns this process's start token, memoized: a process's
// start time cannot change, and the fixture stamps it onto every container it
// creates. Empty when the process table could not be read, which degrades the
// reaper to PID-only liveness rather than breaking container startup.
var SelfStartToken = sync.OnceValue(func() string { return StartedAt(os.Getpid()) })

// StartedAt returns a token identifying the process INSTANCE that currently
// holds pid, or "" if none does. Two processes that held the same PID at
// different times get different tokens, which is the whole point: a PID alone
// cannot distinguish a running suite from a dead one whose number the kernel
// has since handed out again.
func StartedAt(pid int) string {
	starts, err := procStarts()
	if err != nil {
		return ""
	}
	return starts[pid]
}

// LiveOwnerChecker returns an OwnerChecker backed by the real process table.
//
// The process snapshot is taken lazily and at most once per checker: a sweep
// that finds no candidates — the common case, since most starts follow a clean
// one — pays nothing, and a sweep that finds twenty pays for one `ps` rather
// than twenty.
func LiveOwnerChecker() OwnerChecker {
	var (
		once   sync.Once
		starts map[int]string
		err    error
	)
	return func(pid int, startedAt string) OwnerStatus {
		once.Do(func() {
			starts, err = procStarts()
			if err != nil {
				// Never silent: without this the reaper quietly degrades to the
				// weaker check that let PID reuse pin containers forever, and
				// nothing would say so.
				fmt.Fprintf(os.Stderr,
					"testspicedb: %v; falling back to pid-only owner liveness, which cannot detect a reused pid\n", err)
			}
		})
		if err != nil {
			// Weaker, but strictly more conservative: a live PID reports
			// OwnerUnknown, never OwnerRecycled, so a failure here can only
			// leave MORE containers in place.
			if processAlive(pid) {
				return OwnerUnknown
			}
			return OwnerGone
		}
		return ownerStatus(pid, startedAt, starts)
	}
}

// ownerStatus is the hardened liveness rule, split out from its data source so
// every branch is reachable in a test.
func ownerStatus(pid int, startedAt string, starts map[int]string) OwnerStatus {
	if pid <= 0 {
		return OwnerGone
	}
	now, held := starts[pid]
	if !held {
		// Absent from the snapshot is not proof of death. The process may have
		// started after the snapshot was taken, or be hidden from us (Linux
		// hidepid). Confirm with signal 0 before declaring it gone, because
		// that is the one wrong answer that removes a container in use.
		if processAlive(pid) {
			return OwnerUnknown
		}
		return OwnerGone
	}
	if startedAt == "" {
		// Stamped by a fixture that predates start tokens: PID liveness is all
		// the evidence there is, and it says the PID is held.
		return OwnerUnknown
	}
	if now != startedAt {
		return OwnerRecycled
	}
	return OwnerLive
}

// procStarts snapshots the start time of every visible process, keyed by PID.
//
// `lstart` is the one start-time column both BSD (macOS) and procps (Linux)
// `ps` render identically, and unlike `etime` it does not change between two
// readings of the same process.
//
// Its resolution is one second, which is enough, and the reason is worth
// stating because "one second" sounds too coarse for this job. A PID can only
// be reassigned after its previous holder exits, so a recycling process starts
// no earlier than the old owner's DEATH — while the token records the old
// owner's BIRTH. For the two to render identically, an owner would have to
// start a Docker container and die inside the same wall-clock second, AND the
// kernel would have to wrap its PID counter around within that second. Short of
// that the tokens differ and the reuse is caught. In the impossible case the
// answer is OwnerLive, which leaves the container in place — the safe
// direction, and the same answer the unhardened check gave.
func procStarts() (map[int]string, error) {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,lstart=").Output()
	if err != nil {
		return nil, fmt.Errorf("read process start times: %w", err)
	}
	return parseProcStarts(string(out)), nil
}

// parseProcStarts parses `ps -ax -o pid=,lstart=` output. Rows that do not
// parse are skipped rather than fatal: this runs before and after every suite,
// so it must not be able to break a build.
func parseProcStarts(psOutput string) map[int]string {
	starts := make(map[int]string)
	for _, line := range strings.Split(psOutput, "\n") {
		pidField, rest, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}
		pid, err := strconv.Atoi(pidField)
		if err != nil {
			continue
		}
		token := normalizeStart(rest)
		if token == "" {
			continue
		}
		starts[pid] = token
	}
	return starts
}

// normalizeStart collapses ps's column padding so a token read back later
// compares equal to the one stamped onto a container.
func normalizeStart(s string) string { return strings.Join(strings.Fields(s), " ") }

// processAlive reports whether pid names a live process. Signal 0 performs the
// existence + permission check without delivering anything: nil means alive and
// ours, EPERM means alive and someone else's. Anything other than a
// definitely-gone answer is treated as ALIVE, so an unexpected errno errs toward
// leaving a container alone.
//
// Both "gone" spellings must be checked. os.Process.Signal translates ESRCH into
// os.ErrProcessDone, so a test that only compared against syscall.ESRCH reported
// every dead owner as alive and the reaper silently never reaped anything —
// which is exactly what a run of the suite showed before this was fixed.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
}
