package testspicedb

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadOwnerStart stands in for the start token of a test process that has
// exited. The epoch is used deliberately: an earlier draft picked a
// plausible-looking recent timestamp and it collided with this machine's real
// PID 1, which is precisely the confusion the token exists to prevent.
const deadOwnerStart = "Thu Jan 1 00:00:00 1970"

// deadOwner is an OwnerChecker that reports every owner as gone, so a test can
// isolate the question "would this container be a candidate at all?" from the
// liveness rules. Any container this checker spares is spared by something
// stronger than owner state.
func deadOwner(int, string) OwnerStatus { return OwnerGone }

// liveOwner is the opposite: every owner is running.
func liveOwner(int, string) OwnerStatus { return OwnerLive }

// fixtureLabels builds the label set the fixture stamps onto a container it
// starts.
func fixtureLabels(pid, start, runID string) map[string]string {
	return map[string]string{
		LabelFixture:    LabelFixtureValue,
		LabelOwnerPID:   pid,
		LabelOwnerStart: start,
		LabelRunID:      runID,
	}
}

// only returns the single decision for the container with the given ID,
// failing the test if the classifier did not produce exactly one.
func only(t *testing.T, decisions []Decision, id string) Decision {
	t.Helper()
	var found []Decision
	for _, d := range decisions {
		if d.ID == id {
			found = append(found, d)
		}
	}
	require.Len(t, found, 1, "expected exactly one decision for container %q", id)
	return found[0]
}

// -----------------------------------------------------------------------
// Safety: containers this fixture did not start are never candidates.
// -----------------------------------------------------------------------

// TestClassify_NeverReapsAnUnlabelledContainer is the property the whole
// reaper is built around, and the one whose failure is unrecoverable: a
// developer's own SpiceDB runs off the same image, and killing it takes their
// working stack down with no way to tell them why.
//
// Every other signal is stacked AGAINST the unlabelled container here — dead
// owner, and a run ID matching the caller's own finished run — so the only
// thing that can spare it is the fixture label check itself.
func TestClassify_NeverReapsAnUnlabelledContainer(t *testing.T) {
	const ourRun = "test-steel-1234"

	cases := []struct {
		name      string
		container Container
	}{
		{
			name: "a developer's own dev-stack spicedb, same image, no labels",
			container: Container{
				ID:     "dev-stack",
				Name:   "spicedb-dev",
				Labels: map[string]string{},
			},
		},
		{
			name: "nil label map does not panic and is not a candidate",
			container: Container{
				ID:     "dev-stack-nil-labels",
				Name:   "spicedb-dev",
				Labels: nil,
			},
		},
		{
			name: "carries our owner and run labels but NOT the fixture label",
			container: Container{
				ID:   "impostor",
				Name: "spicedb-dev",
				Labels: map[string]string{
					LabelOwnerPID: "999999",
					LabelRunID:    ourRun,
				},
			},
		},
		{
			name: "fixture label present but with some other value",
			container: Container{
				ID:   "wrong-fixture-value",
				Name: "spicedb-dev",
				Labels: map[string]string{
					LabelFixture:  "someone-elses-fixture",
					LabelOwnerPID: "999999",
					LabelRunID:    ourRun,
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := only(t, Classify([]Container{tc.container}, os.Getpid(), ourRun, deadOwner), tc.container.ID)
			assert.False(t, got.Reap, "an unlabelled container must never be a reap candidate")
			assert.Contains(t, got.Reason, "not a fixture container",
				"the reason must name the label check, so a puzzled reader can see why it was spared")
		})
	}
}

// TestReapWith_NeverRemovesAnUnlabelledContainer proves the property end to
// end, through the code path that actually deletes.
//
// The fake deliberately IGNORES the label filter and hands back the
// developer's containers anyway. That is not a realistic daemon, and that is
// the point: it takes the daemon-side filter out of the argument, so the only
// thing standing between a dev stack and `docker rm -f` is the reaper's own
// rules. A regression that dropped the in-process label check would pass a
// test that trusted the filter, and fail this one.
//
// Two unlabelled shapes, because the realistic one alone does not pin the
// label check. A dev container carries no owner PID either, so the
// "unparseable owner" rule spares it and the test passes even with the label
// check deleted — which is exactly what happened when this was mutation
// tested. The impostor carries every label EXCEPT the fixture one, so nothing
// but the label check can save it.
func TestReapWith_NeverRemovesAnUnlabelledContainer(t *testing.T) {
	const ourRun = "test-steel-1234"
	devStack := docker.APIContainers{
		ID:     "dev-stack-id",
		Names:  []string{"/spicedb-dev"},
		Image:  "quay.io/authzed/spicedb:latest",
		Labels: map[string]string{},
	}
	impostor := docker.APIContainers{
		ID:    "impostor-id",
		Names: []string{"/spicedb-dev-2"},
		Image: "quay.io/authzed/spicedb:latest",
		Labels: map[string]string{
			LabelOwnerPID:   "999999",
			LabelOwnerStart: deadOwnerStart,
			LabelRunID:      ourRun,
		},
	}
	leaked := docker.APIContainers{
		ID:     "leaked-id",
		Names:  []string{"/serene_hopper"},
		Image:  "authzed/spicedb:latest",
		Labels: fixtureLabels("999999", deadOwnerStart, ourRun),
	}
	api := &fakeDocker{list: []docker.APIContainers{devStack, impostor, leaked}}

	res, err := ReapWith(api, os.Getpid(), ourRun, deadOwner)
	require.NoError(t, err)

	assert.Equal(t, []string{"leaked-id"}, api.removed,
		"only the fixture-labelled container may be removed")
	assert.NotContains(t, api.removed, "dev-stack-id",
		"the developer's own spicedb must survive a sweep, even when handed to the reaper")
	assert.NotContains(t, api.removed, "impostor-id",
		"a container carrying every label but the fixture one is still not ours")

	require.Len(t, res.Removed, 1)
	assert.Equal(t, "leaked-id", res.Removed[0].ID)
	assert.Len(t, res.Kept, 2)
}

// TestReapWith_ListsOnlyFixtureLabelledContainers pins the daemon-side half of
// the same guarantee: the sweep must never ask for a broad listing it then
// narrows in memory. Filtering by image or by name pattern is what a blanket
// prune looks like, and it must not be reachable from here.
func TestReapWith_ListsOnlyFixtureLabelledContainers(t *testing.T) {
	api := &fakeDocker{}
	_, err := ReapWith(api, os.Getpid(), "", deadOwner)
	require.NoError(t, err)

	require.Len(t, api.listOpts, 1, "the sweep must list exactly once")
	opts := api.listOpts[0]
	assert.True(t, opts.All, "stopped fixture containers accumulate too and must be swept")
	assert.Equal(t, map[string][]string{
		"label": {LabelFixture + "=" + LabelFixtureValue},
	}, opts.Filters, "the listing must be scoped to the fixture's own label and nothing else")
}

// TestClassify_SparesAConcurrentSuitesContainer: another worktree's suite has a
// live owner process, and removing its SpiceDB mid-run is the failure the
// liveness check exists to prevent.
func TestClassify_SparesAConcurrentSuitesContainer(t *testing.T) {
	c := Container{
		ID:     "other-worktree",
		Labels: fixtureLabels("4242", deadOwnerStart, "test-e2e-other"),
	}
	got := only(t, Classify([]Container{c}, os.Getpid(), "test-steel-mine", liveOwner), c.ID)
	assert.False(t, got.Reap, "a container whose owner is still running belongs to a live suite")
	assert.Contains(t, got.Reason, "still running")
}

// TestClassify_SparesOurOwnContainer: the in-process sweep runs while this
// process holds a container it just started.
func TestClassify_SparesOurOwnContainer(t *testing.T) {
	self := os.Getpid()
	c := Container{
		ID:     "mine",
		Labels: fixtureLabels(strconv.Itoa(self), SelfStartToken(), "test-steel-mine"),
	}
	// Even with the run ID matching and the owner reported dead, our own
	// container must survive: the self check comes first.
	got := only(t, Classify([]Container{c}, self, "test-steel-mine", deadOwner), c.ID)
	assert.False(t, got.Reap)
	assert.Contains(t, got.Reason, "this process")
}

// TestClassify_ReapsThisRunsContainersWithoutConsultingLiveness is the fix for
// the leak that actually accumulates. After `go test` returns, every binary
// that could hold one of these has exited — so the run ID alone settles it,
// and a recycled PID that would otherwise report "live" cannot veto the
// removal.
func TestClassify_ReapsThisRunsContainersWithoutConsultingLiveness(t *testing.T) {
	const ourRun = "test-steel-1234"
	c := Container{
		ID:     "ours",
		Labels: fixtureLabels("4242", deadOwnerStart, ourRun),
	}

	got := only(t, Classify([]Container{c}, os.Getpid(), ourRun, liveOwner), c.ID)
	assert.True(t, got.Reap, "a container from this caller's own finished run is removable")
	assert.Contains(t, got.Reason, "has finished")

	// And another run's container, on the same evidence, is not.
	other := Container{
		ID:     "theirs",
		Labels: fixtureLabels("4242", deadOwnerStart, "test-e2e-other-worktree"),
	}
	spared := only(t, Classify([]Container{other}, os.Getpid(), ourRun, liveOwner), other.ID)
	assert.False(t, spared.Reap, "another worktree's run ID must not be claimed by ours")
}

// TestClassify_EmptyOwnRunIDNeverMatches: the in-process fixture passes "",
// meaning "I have no finished run to speak for". A container whose run label is
// also empty (AP_TEST_RUN_ID unset) must not be swept up by that.
func TestClassify_EmptyOwnRunIDNeverMatches(t *testing.T) {
	c := Container{
		ID:     "unscoped",
		Labels: fixtureLabels("4242", deadOwnerStart, ""),
	}
	got := only(t, Classify([]Container{c}, os.Getpid(), "", liveOwner), c.ID)
	assert.False(t, got.Reap, "empty run IDs must not match each other")
	assert.Contains(t, got.Reason, "still running", "the owner check decides instead")
}

// TestClassify_UnparseableOwnerLabelsAreLeftAlone: a container we cannot reason
// about is one we do not touch.
func TestClassify_UnparseableOwnerLabelsAreLeftAlone(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		reason string
	}{
		{
			name:   "owner pid label missing entirely",
			labels: map[string]string{LabelFixture: LabelFixtureValue},
			reason: "no " + LabelOwnerPID + " label",
		},
		{
			name:   "owner pid label is not a number",
			labels: map[string]string{LabelFixture: LabelFixtureValue, LabelOwnerPID: "not-a-pid"},
			reason: "does not parse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Container{ID: "opaque", Labels: tc.labels}
			got := only(t, Classify([]Container{c}, os.Getpid(), "", deadOwner), c.ID)
			assert.False(t, got.Reap)
			assert.Contains(t, got.Reason, tc.reason)
		})
	}
}

// TestReapWith_ReportsARemovalFailureAsKept: a container the reaper decided to
// remove and could not is still there, so the caller's leak count must still
// include it AND carry the reason. Reporting it as removed is how a leak goes
// silent.
func TestReapWith_ReportsARemovalFailureAsKept(t *testing.T) {
	boom := errors.New("daemon said no")
	api := &fakeDocker{
		list: []docker.APIContainers{{
			ID:     "stuck",
			Names:  []string{"/stuck"},
			Labels: fixtureLabels("999999", deadOwnerStart, ""),
		}},
		removeErr: boom,
	}

	res, err := ReapWith(api, os.Getpid(), "", deadOwner)
	require.NoError(t, err, "one container that will not die must not fail the whole sweep")
	assert.Empty(t, res.Removed)
	require.Len(t, res.Kept, 1)
	assert.ErrorIs(t, res.Kept[0].Err, boom)
	assert.Contains(t, res.Kept[0].Reason, "owner pid 999999 is gone",
		"the decision that led to the attempt is retained alongside the failure")
}

// TestReapWith_TreatsAlreadyGoneAsRemoved: two sweeps can race, and losing that
// race is not a failure — the container is gone, which is all the result
// claims.
func TestReapWith_TreatsAlreadyGoneAsRemoved(t *testing.T) {
	api := &fakeDocker{
		list: []docker.APIContainers{{
			ID:     "raced",
			Labels: fixtureLabels("999999", deadOwnerStart, ""),
		}},
		removeErr: &docker.NoSuchContainer{ID: "raced"},
	}
	res, err := ReapWith(api, os.Getpid(), "", deadOwner)
	require.NoError(t, err)
	require.Len(t, res.Removed, 1)
	assert.Empty(t, res.Kept)
}

// TestReapWith_ListFailureIsReturnedNotSwallowed: a sweep that cannot see the
// containers must say so rather than report "nothing to do".
func TestReapWith_ListFailureIsReturnedNotSwallowed(t *testing.T) {
	boom := errors.New("no daemon")
	_, err := ReapWith(&fakeDocker{listErr: boom}, os.Getpid(), "", deadOwner)
	require.ErrorIs(t, err, boom)
}

// -----------------------------------------------------------------------
// Owner liveness: telling a live owner from a recycled PID.
// -----------------------------------------------------------------------

// TestOwnerStatus_DistinguishesAReusedPID is the hardening, stated as the exact
// scenario it exists for.
//
// The old rule was `processAlive(pid)`. A dead owner whose PID the kernel has
// since handed to some other live process answers "alive" under that rule, so
// the container is skipped on that sweep — and on every later one, forever,
// because the answer never changes. macOS recycles PIDs quickly and a suite
// spawns dozens of short-lived test binaries, so this is the shape of a leak
// that GROWS rather than plateaus.
//
// Both halves are asserted side by side: the old predicate says alive, the new
// one says recycled. That difference is the fix.
func TestOwnerStatus_DistinguishesAReusedPID(t *testing.T) {
	self := os.Getpid()
	starts, err := procStarts()
	require.NoError(t, err, "reading the process table is a precondition for this test")
	ourStart := starts[self]
	require.NotEmpty(t, ourStart, "this process must appear in its own process table")

	require.NotEqual(t, ourStart, deadOwnerStart,
		"fixture sanity: the dead owner's token must differ from ours")

	// The old rule cannot tell these apart.
	require.True(t, processAlive(self),
		"baseline: the recycled PID is held by a live process, which is why the old check said 'alive'")

	assert.Equal(t, OwnerRecycled, ownerStatus(self, deadOwnerStart, starts),
		"a live PID that is NOT the process which created the container is a reused PID")
	assert.Equal(t, OwnerLive, ownerStatus(self, ourStart, starts),
		"the same PID with its own start token is a genuinely live owner")
}

// TestOwnerStatus_RecycledPIDBecomesAReapCandidate carries the discrimination
// through to the decision, because a hardened predicate nothing consults would
// change nothing.
func TestOwnerStatus_RecycledPIDBecomesAReapCandidate(t *testing.T) {
	self := os.Getpid()
	starts, err := procStarts()
	require.NoError(t, err)

	// Owner PID 1 is init: always alive, never a test binary, so a container
	// claiming it is the clearest possible case of a reused PID.
	c := Container{
		ID:     "pinned-by-a-reused-pid",
		Labels: fixtureLabels("1", deadOwnerStart, ""),
	}
	require.NotEqual(t, deadOwnerStart, starts[1],
		"fixture sanity: init did not start at the fabricated time")

	check := func(pid int, startedAt string) OwnerStatus { return ownerStatus(pid, startedAt, starts) }
	got := only(t, Classify([]Container{c}, self, "", check), c.ID)
	assert.True(t, got.Reap, "a container pinned by a reused PID must become reapable")
	assert.Contains(t, got.Reason, "was reused")
}

// TestOwnerStatus_RealExitedProcess uses a process that genuinely ran and
// exited, rather than a fabricated PID: the reaper's whole job is to recognise
// exactly this state.
func TestOwnerStatus_RealExitedProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	alive, err := procStarts()
	require.NoError(t, err)
	childStart := alive[pid]
	require.NotEmpty(t, childStart, "a running child must appear in the process table")
	assert.Equal(t, OwnerLive, ownerStatus(pid, childStart, alive),
		"while the child runs, its container must be spared")

	require.NoError(t, cmd.Process.Kill())
	_, _ = cmd.Process.Wait()

	after, err := procStarts()
	require.NoError(t, err)
	assert.Equal(t, OwnerGone, ownerStatus(pid, childStart, after),
		"once the owner has exited its container is abandoned")

	// And the token really is instance-specific rather than a constant: PID 1
	// booted long before this test binary did, so their tokens must differ.
	//
	// Deliberately NOT compared against the child: `lstart` has one-second
	// resolution and the child was spawned in the same second as the process
	// that spawned it, so those two legitimately match. See procStarts for why
	// that bound cannot produce a false "live" for a real recycled PID.
	require.NotEmpty(t, alive[1], "init must appear in the process table")
	assert.NotEqual(t, alive[1], alive[os.Getpid()],
		"processes started at different times must carry different tokens")
}

// TestOwnerStatus_UnresolvableCasesLeaveTheContainerAlone. Every branch here is
// a case where the reaper does not KNOW, and not knowing must never remove.
func TestOwnerStatus_UnresolvableCasesLeaveTheContainerAlone(t *testing.T) {
	self := os.Getpid()
	starts, err := procStarts()
	require.NoError(t, err)

	t.Run("container predates start tokens: pid liveness is all there is", func(t *testing.T) {
		assert.Equal(t, OwnerUnknown, ownerStatus(self, "", starts),
			"an empty start label must not be read as a mismatch")
	})

	t.Run("process hidden from the snapshot but answering signal 0", func(t *testing.T) {
		// Linux hidepid, or a process that started after the snapshot was
		// taken. Absent from the table is not proof of death.
		assert.Equal(t, OwnerUnknown, ownerStatus(self, "whatever", map[int]string{}),
			"a PID absent from the snapshot must be confirmed dead by signal 0 first")
	})

	t.Run("non-positive pids are gone", func(t *testing.T) {
		assert.Equal(t, OwnerGone, ownerStatus(0, "x", starts), "an unset owner label parses to 0")
		assert.Equal(t, OwnerGone, ownerStatus(-1, "x", starts), "never signalled as a process group")
	})
}

// TestLiveOwnerChecker_MatchesTheRawRule wires the exported constructor to the
// same expectations, so the lazily-snapshotted path cannot drift from the one
// the tests above pin.
func TestLiveOwnerChecker_MatchesTheRawRule(t *testing.T) {
	check := LiveOwnerChecker()
	self := os.Getpid()
	assert.Equal(t, OwnerLive, check(self, SelfStartToken()))
	assert.Equal(t, OwnerRecycled, check(self, deadOwnerStart))
	assert.Equal(t, OwnerGone, check(0, ""))
}

// TestSelfStartToken_IsStableAndNonEmpty: the token is stamped onto every
// container the fixture starts, and a token that changed between two readings
// would make every later sweep call its own live containers recycled.
func TestSelfStartToken_IsStableAndNonEmpty(t *testing.T) {
	first := SelfStartToken()
	require.NotEmpty(t, first, "this process must be able to read its own start time")
	assert.Equal(t, first, SelfStartToken(), "a process's start time cannot change")
	assert.Equal(t, first, StartedAt(os.Getpid()),
		"the memoized token must equal a fresh reading, or stamped and compared values diverge")
}

// TestStartedAt_UnknownPIDIsEmpty: an absent process yields no token, which
// callers read as "no evidence" rather than as a mismatch.
func TestStartedAt_UnknownPIDIsEmpty(t *testing.T) {
	assert.Empty(t, StartedAt(-1))
}

// -----------------------------------------------------------------------
// Process-table parsing.
// -----------------------------------------------------------------------

// psSample is real `ps -ax -o pid=,lstart=` output shape: right-aligned PIDs,
// a five-field date containing spaces, and the trailing column padding macOS
// emits — which is exactly what a naive split-and-take-the-rest gets wrong.
//
// The last row is a SINGLE-DIGIT day, which ps pads to two columns ("Aug  2").
// That is the only thing in real output with interior padding, so without it
// this sample never exercises the collapse at all — it passed unchanged with
// normalizeStart mutated to the identity function.
const psSample = "    1 Mon Aug 24 14:20:39 2026    \n" +
	"  341 Mon Aug 24 14:21:02 2026    \n" +
	"23903 Sat Aug 29 09:14:55 2026    \n" +
	"  512 Sat Aug  2 09:14:55 2026    \n"

func TestParseProcStarts_KeepsTheWholeDateAndTrimsPadding(t *testing.T) {
	got := parseProcStarts(psSample)
	assert.Equal(t, map[int]string{
		1:     "Mon Aug 24 14:20:39 2026",
		341:   "Mon Aug 24 14:21:02 2026",
		23903: "Sat Aug 29 09:14:55 2026",
		512:   "Sat Aug 2 09:14:55 2026",
	}, got)
}

// TestParseProcStarts_SkipsMalformedRowsWithoutPanicking: this parser runs
// before and after every suite, so it must be impossible for it to break a
// build.
func TestParseProcStarts_SkipsMalformedRowsWithoutPanicking(t *testing.T) {
	junk := "garbage\n\n  PID STARTED\nx Mon Aug 24 14:20:39 2026\n77 Mon Aug 24 14:20:39 2026\n99\n"
	var got map[int]string
	require.NotPanics(t, func() { got = parseProcStarts(junk) })
	assert.Equal(t, map[int]string{77: "Mon Aug 24 14:20:39 2026"}, got)
}

func TestParseProcStarts_EmptyInputIsSafe(t *testing.T) {
	assert.Empty(t, parseProcStarts(""))
}

// TestNormalizeStart collapses padding so a token stamped from one reading
// compares equal to the same process read back later.
func TestNormalizeStart(t *testing.T) {
	assert.Equal(t, "Mon Aug 24 14:20:39 2026",
		normalizeStart("  Mon  Aug 24   14:20:39 2026   "))
	assert.Empty(t, normalizeStart("   "))
}

// -----------------------------------------------------------------------
// Helpers.
// -----------------------------------------------------------------------

// fakeDocker records what the reaper asked for and what it removed, so a test
// can assert on the requests themselves rather than on their effects.
type fakeDocker struct {
	list      []docker.APIContainers
	listErr   error
	removeErr error

	listOpts []docker.ListContainersOptions
	removed  []string
}

func (f *fakeDocker) ListContainers(opts docker.ListContainersOptions) ([]docker.APIContainers, error) {
	f.listOpts = append(f.listOpts, opts)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeDocker) RemoveContainer(opts docker.RemoveContainerOptions) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, opts.ID)
	return nil
}
