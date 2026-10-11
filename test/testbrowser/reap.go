// This file carries NO build tag, like labels.go, because two callers that
// never link each other have to reap by exactly the same rules: the fixture
// itself (tagged `integration || e2e`, sweeping on every container start) and
// the magefile (untagged, sweeping after a suite finishes). A second copy of
// these rules is how the one safety property that matters — never touch a
// container this fixture did not start — silently stops holding on one path.
//
// Mirrors test/testspicedb/reap.go; the ONLY differences are the package name
// (so the browser fixture reaps on the browser label, LabelFixtureValue) and
// the log-prefix strings. The owner-liveness logic is identical and is kept
// identical deliberately — see that file's commentary for why each rule exists.

package testbrowser

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
var ErrDockerUnavailable = errors.New("testbrowser: no reachable docker daemon")

// Container is the slice of a docker container listing the reaper reasons
// about. Taking a struct rather than docker.APIContainers keeps the decision
// rules testable without a daemon.
type Container struct {
	ID     string
	Name   string
	Labels map[string]string
}

// OwnerStatus is what the reaper could establish about the test process that
// started a container. Only OwnerGone and OwnerRecycled permit removal.
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
	// confirmed. Treated as live: every unresolvable case must err toward
	// leaving a container in place.
	OwnerUnknown OwnerStatus = "unknown"
)

// OwnerChecker reports the state of the process that started a container, given
// the PID and start token stamped onto it. Injectable so the decision rules can
// be tested against every owner state without arranging real processes.
type OwnerChecker func(pid int, startedAt string) OwnerStatus

// Decision records what the reaper resolved to do about one container and why.
type Decision struct {
	ID     string
	Name   string
	Reap   bool
	Reason string
	// Err is set when the reaper decided to remove this container and the
	// removal itself failed. Such a decision is reported as kept.
	Err error
}

// Classify decides, for each listed container, whether it may be removed. The
// rules mirror test/testspicedb.Classify exactly; see there for the ordered
// rationale of each.
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
// satisfies it; a test can supply a fake.
type DockerAPI interface {
	ListContainers(docker.ListContainersOptions) ([]docker.APIContainers, error)
	RemoveContainer(docker.RemoveContainerOptions) error
}

// ReapResult separates what the sweep actually removed from what it left in
// place.
type ReapResult struct {
	Removed []Decision
	Kept    []Decision
}

// ReapWith sweeps the fixture's containers through api, applying Classify.
// Nothing here removes a container by image, by name, or in bulk.
func ReapWith(api DockerAPI, self int, ownRunID string, owner OwnerChecker) (ReapResult, error) {
	listed, err := api.ListContainers(docker.ListContainersOptions{
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
			d.Err = err
			res.Kept = append(res.Kept, d)
			continue
		}
		res.Removed = append(res.Removed, d)
	}
	return res, nil
}

// Reap connects to Docker and sweeps this fixture's abandoned containers, plus
// — when ownRunID is non-empty — every container the caller's own finished `go
// test` invocation started.
func Reap(ownRunID string) (ReapResult, error) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		return ReapResult{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	return ReapWith(pool.Client, os.Getpid(), ownRunID, LiveOwnerChecker())
}

// containerName returns a container's primary name without the leading slash.
func containerName(c docker.APIContainers) string {
	if len(c.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// SelfStartToken returns this process's start token, memoized. Empty when the
// process table could not be read, which degrades the reaper to PID-only
// liveness rather than breaking container startup.
var SelfStartToken = sync.OnceValue(func() string { return StartedAt(os.Getpid()) })

// StartedAt returns a token identifying the process INSTANCE that currently
// holds pid, or "" if none does.
func StartedAt(pid int) string {
	starts, err := procStarts()
	if err != nil {
		return ""
	}
	return starts[pid]
}

// LiveOwnerChecker returns an OwnerChecker backed by the real process table.
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
				fmt.Fprintf(os.Stderr,
					"testbrowser: %v; falling back to pid-only owner liveness, which cannot detect a reused pid\n", err)
			}
		})
		if err != nil {
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
		if processAlive(pid) {
			return OwnerUnknown
		}
		return OwnerGone
	}
	if startedAt == "" {
		return OwnerUnknown
	}
	if now != startedAt {
		return OwnerRecycled
	}
	return OwnerLive
}

// procStarts snapshots the start time of every visible process, keyed by PID.
// `lstart` is the one start-time column both BSD (macOS) and procps (Linux) `ps`
// render identically and does not change between readings.
func procStarts() (map[int]string, error) {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,lstart=").Output()
	if err != nil {
		return nil, fmt.Errorf("read process start times: %w", err)
	}
	return parseProcStarts(string(out)), nil
}

// parseProcStarts parses `ps -ax -o pid=,lstart=` output. Rows that do not
// parse are skipped rather than fatal.
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
// existence check without delivering anything; anything other than a
// definitely-gone answer is treated as ALIVE.
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
