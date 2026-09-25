package installcmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

// job is a tiny fixture helper: a build target with just a name (the fake
// runner ignores the docker-relevant fields).
func job(name string) buildJob { return buildJob{t: buildTarget{name: name}, tag: name + ":dev"} }

func TestResolveBuildJobs(t *testing.T) {
	all, err := resolveBuildJobs("all", "", "")
	require.NoError(t, err)
	assert.Len(t, all, len(apimage.All), "a LOCAL all expands to exactly apimage.All, not the full catalog")

	_, err = resolveBuildJobs("all", "sometag", "")
	require.Error(t, err, "--tag with target=all is rejected")
	assert.Contains(t, err.Error(), "incompatible")

	_, err = resolveBuildJobs("nope", "", "")
	require.Error(t, err, "unknown target is rejected")
	assert.Contains(t, err.Error(), "unknown target")

	// --tag overrides ONLY the tag portion; the image name is preserved.
	one, err := resolveBuildJobs(buildTargets[0].name, "dev-fix1", "")
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, imageName(buildTargets[0].defaultTag)+":dev-fix1", one[0].tag,
		"--tag overrides the tag while keeping the image name")

	// A full name:tag form is rejected (the footgun that pushed to <registry>/<tag>).
	_, err = resolveBuildJobs(buildTargets[0].name, "spicebox-operator:dev-fix1", "")
	require.Error(t, err, "--tag must be the tag only, not a full name:tag")
	assert.Contains(t, err.Error(), "image tag only")
}

// TestResolveBuildJobs_AllExcludesToolchains is a regression test for the bug
// where `oap build all` (and bare `oap build`, which defaults to "all") expanded
// over apimage.Catalog() instead of apimage.All, silently pulling in the Go
// language-toolchain image and compiling gopls from source on every normal
// dev-loop build. "all" must never yield a Toolchains entry.
//
// This is the LOCAL expansion only — a registry build deliberately does include
// them (see TestResolveBuildJobs_AllWithRegistryIncludesToolchains), because
// there the install plants a registry ref for every catalog image whether or
// not the build produced it.
func TestResolveBuildJobs_AllExcludesToolchains(t *testing.T) {
	all, err := resolveBuildJobs("all", "", "")
	require.NoError(t, err, "all must resolve")

	for _, j := range all {
		assert.NotEqualf(t, "toolchain-go", j.t.name,
			"all must not expand to the toolchain-go target")
	}

	toolchainTargets := make(map[string]bool, len(apimage.Toolchains))
	for _, im := range apimage.Toolchains {
		toolchainTargets[im.Target] = true
	}
	for _, j := range all {
		assert.Falsef(t, toolchainTargets[j.t.name],
			"all must not expand to any apimage.Toolchains entry, got %q", j.t.name)
	}
}

// TestResolveBuildJobs_AllWithRegistryIncludesToolchains is the other half of
// TestResolveBuildJobs_AllExcludesToolchains. Locally, expanding "all" over the
// toolchains would compile gopls from source on every dev loop — so it must
// not. On the registry path the calculus inverts: the manifest rewrite plants a
// <registry>/ap-toolchain-*:dev reference into the SpiceboxToolchain CRs
// regardless, so an overlay this build skips is a dangling reference and the
// first session composing that toolchain dies on ErrImagePull.
func TestResolveBuildJobs_AllWithRegistryIncludesToolchains(t *testing.T) {
	jobs, err := resolveBuildJobs("all", "", "myreg.io/ap")
	require.NoError(t, err, "all must resolve on the registry path")

	got := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		got[j.t.name] = true
	}
	for _, im := range apimage.Toolchains {
		assert.Truef(t, got[im.Target],
			"a registry build must provision toolchain target %q", im.Target)
	}
	assert.Len(t, jobs, len(apimage.Catalog()),
		"registry expansion is exactly the catalog")
}

// TestRegistryBuildProvisionsEveryPlantedRef is the structural invariant behind
// this whole fix, and the test that would have caught the original bug. The
// manifest rewrite (pkg/platform/manifests/substitute.go -> apimage.ResolveDigests)
// plants a registry-qualified ref for every apimage.Catalog() entry. Whatever
// set that is, the registry build must cover it. The two drifted once
// (build = All, rewrite = Catalog) and `oap init --image-registry` shipped three
// SpiceboxToolchain CRs pointing at images it never pushed.
func TestRegistryBuildProvisionsEveryPlantedRef(t *testing.T) {
	const reg = "myreg.io/ap"
	jobs, err := resolveBuildJobs("all", "", reg)
	require.NoError(t, err)

	built := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		built[imageName(j.t.defaultTag)] = true
	}
	for orig := range apimage.ResolveDigests(reg, nil, nil) {
		assert.Truef(t, built[imageName(orig)],
			"install plants a registry ref for %q but `oap build all --image-registry` never builds it", orig)
	}
}

// TestResolveBuildJobs_ExplicitToolchainStillResolves proves the explicit path
// keeps working: `oap build toolchain-go` must still resolve to exactly that
// one target even though "all" no longer includes it.
func TestResolveBuildJobs_ExplicitToolchainStillResolves(t *testing.T) {
	jobs, err := resolveBuildJobs("toolchain-go", "", "")
	require.NoError(t, err, "toolchain-go is a valid explicit target")
	require.Len(t, jobs, 1, "toolchain-go resolves to exactly one job")
	assert.Equal(t, "toolchain-go", jobs[0].t.name)
}

func TestBuildConcurrency_Bounds(t *testing.T) {
	n := buildConcurrency()
	assert.GreaterOrEqual(t, n, 1, "at least one builder")
	assert.LessOrEqual(t, n, buildMaxParallel, "never exceeds the cap")
}

// TestOrchestrateBuilds_NonTTYStreamsRaw proves mode selection: a non-TTY (or
// --raw) out streams docker output verbatim to out — the runner is handed `out`
// as its stdout, not a per-image capture buffer.
func TestOrchestrateBuilds_NonTTYStreamsRaw(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprintf("raw=%v streams to out", raw), func(t *testing.T) {
			var out, errOut bytes.Buffer
			var order []string
			var mu sync.Mutex
			run := func(_ context.Context, tg buildTarget, _ string, stdout, _ io.Writer) (string, error) {
				mu.Lock()
				order = append(order, tg.name)
				mu.Unlock()
				fmt.Fprintf(stdout, "RAW_OUTPUT_%s\n", tg.name)
				return "", nil
			}
			digests, err := orchestrateBuilds(context.Background(), &out, &errOut, []buildPhase{{job("alpha"), job("beta")}}, raw, run, nil)
			require.NoError(t, err)
			assert.Empty(t, digests)
			assert.Equal(t, []string{"alpha", "beta"}, order, "raw path builds sequentially in order")
			assert.Contains(t, out.String(), "RAW_OUTPUT_alpha", "raw path streams docker output to out")
			assert.Contains(t, out.String(), "RAW_OUTPUT_beta")
		})
	}
}

// TestBuildParallelRich_AllTargetsBuiltAndCaptured: every target is attempted,
// and on success the docker output is captured (NOT streamed to out).
func TestBuildParallelRich_AllTargetsBuiltAndCaptured(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	run := func(_ context.Context, tg buildTarget, _ string, stdout, _ io.Writer) (string, error) {
		mu.Lock()
		seen[tg.name] = true
		mu.Unlock()
		fmt.Fprintf(stdout, "DOCKER_OUTPUT_%s\n", tg.name)
		return "", nil
	}
	jobs := []buildJob{job("alpha"), job("beta"), job("gamma")}
	var out bytes.Buffer
	_, err := buildParallelRich(context.Background(), &out, []buildPhase{jobs}, 4, run, nil)
	require.NoError(t, err)

	for _, n := range []string{"alpha", "beta", "gamma"} {
		assert.Truef(t, seen[n], "target %q was attempted", n)
		assert.NotContainsf(t, out.String(), "DOCKER_OUTPUT_"+n,
			"successful build %q output is captured, not streamed to out", n)
	}
}

// TestBuildParallelRich_RunsConcurrently observes more than one build in flight
// at once (the old loop was strictly sequential).
func TestBuildParallelRich_RunsConcurrently(t *testing.T) {
	var inFlight, maxInFlight int32
	run := func(_ context.Context, _ buildTarget, _ string, _, _ io.Writer) (string, error) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			m := atomic.LoadInt32(&maxInFlight)
			if n <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond) // hold the slot so peers overlap
		atomic.AddInt32(&inFlight, -1)
		return "", nil
	}
	jobs := []buildJob{job("a"), job("b"), job("c"), job("d")}
	var out bytes.Buffer
	_, err := buildParallelRich(context.Background(), &out, []buildPhase{jobs}, 4, run, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&maxInFlight), int32(2), "builds ran in parallel")
}

// TestBuildParallelRich_FirstErrorAbortsAndSurfacesOutput: a failing image fails
// the whole build, cancels (aborts) its in-flight peers, and prints the failing
// image's captured docker output so the error stays debuggable.
func TestBuildParallelRich_FirstErrorAbortsAndSurfacesOutput(t *testing.T) {
	var mu sync.Mutex
	started := map[string]bool{}
	var alphaAborted, gammaAborted atomic.Bool
	run := func(ctx context.Context, tg buildTarget, _ string, stdout, stderr io.Writer) (string, error) {
		mu.Lock()
		started[tg.name] = true
		mu.Unlock()
		if tg.name == "beta" {
			fmt.Fprintln(stderr, "FAILMARKER: beta build blew up")
			return "", fmt.Errorf("docker build beta: exit status 1")
		}
		// Peers block until the failure cancels the shared context.
		<-ctx.Done()
		if tg.name == "alpha" {
			alphaAborted.Store(true)
		} else {
			gammaAborted.Store(true)
		}
		return "", ctx.Err()
	}
	jobs := []buildJob{job("alpha"), job("beta"), job("gamma")}
	var out bytes.Buffer
	_, err := buildParallelRich(context.Background(), &out, []buildPhase{jobs}, 4, run, nil)

	require.Error(t, err, "a failing image fails the build")
	assert.Contains(t, err.Error(), "beta", "the surfaced error is the root failure")

	mu.Lock()
	assert.True(t, started["alpha"] && started["beta"] && started["gamma"], "all three started in parallel")
	mu.Unlock()
	assert.True(t, alphaAborted.Load(), "alpha was aborted via ctx cancel")
	assert.True(t, gammaAborted.Load(), "gamma was aborted via ctx cancel")

	assert.Contains(t, out.String(), "FAILMARKER: beta build blew up",
		"the failing image's captured output is surfaced for debugging")
}

// TestBuildParallelRich_CollectsDigests: digests returned by the runner are
// collected into the map keyed by imageName(t.defaultTag).
func TestBuildParallelRich_CollectsDigests(t *testing.T) {
	tgt0 := buildTargets[0]
	tgt1 := buildTargets[1]
	jobs := []buildJob{
		{t: tgt0, tag: tgt0.defaultTag},
		{t: tgt1, tag: tgt1.defaultTag},
	}
	run := func(_ context.Context, tgt buildTarget, _ string, stdout, _ io.Writer) (string, error) {
		fmt.Fprintf(stdout, "built %s\n", tgt.name)
		return "sha256:" + tgt.name, nil
	}
	var out bytes.Buffer
	digests, err := buildParallelRich(context.Background(), &out, []buildPhase{jobs}, 2, run, nil)
	require.NoError(t, err)
	assert.Len(t, digests, 2)
	assert.Equal(t, "sha256:"+tgt0.name, digests[imageName(tgt0.defaultTag)])
	assert.Equal(t, "sha256:"+tgt1.name, digests[imageName(tgt1.defaultTag)])
}

// fakeRail is a minimal progress.RailProvider standing in for the unified init
// wizard's own railState, without coupling these tests to that wizard-specific
// type.
type fakeRail struct {
	steps  []tui.Step
	active int
}

func (f *fakeRail) Steps() []tui.Step { return f.steps }
func (f *fakeRail) Active() int       { return f.active }

// TestBuildParallelRich_NonNilRailIsInertOffATerminal is F5's plumbing proof
// for the Build phase: buildParallelRich accepts a non-nil rail (the unified
// init wizard's own railState, threaded through runBuild/orchestrateBuilds) and
// forwards it to progress.NewWithRail unconditionally — but NewWithRail itself
// only composites the rail on a genuine terminal (cliout.IsTTY(out)). Off one —
// every test writer, since a bytes.Buffer is never a *os.File — it selects the
// exact same streaming renderer a nil rail would, so passing a rail here cannot
// change a single byte of this test's output. That is what makes it safe for
// this package's tests to exercise the plumbing without a real terminal: actual
// rail RENDERING is proven generically in pkg progress
// (TestChecklistRendersRailGutter), against the same progress.NewWithRail this
// call reaches.
func TestBuildParallelRich_NonNilRailIsInertOffATerminal(t *testing.T) {
	// A single job, not two concurrent ones: two rows race the shared reporter
	// mutex independently on each of the two calls below, so their relative
	// output order isn't reproducible run-to-run — that's a property of
	// concurrent scheduling, not of the rail, and would flake this comparison
	// for a reason unrelated to what it's proving.
	//
	// The runner sleeps briefly before returning so the phase's very first poll
	// deterministically observes "not yet done" (and so prints its one "waiting"
	// line) on BOTH calls below — without it, whether the runner's goroutine
	// finishes before or after that first poll is a genuine scheduling race
	// (observed flaking this comparison under load), independent of the rail.
	jobs := []buildJob{job("alpha")}
	run := func(_ context.Context, tg buildTarget, _ string, stdout, _ io.Writer) (string, error) {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(stdout, "built %s\n", tg.name)
		return "sha256:" + tg.name, nil
	}

	var withNilRail bytes.Buffer
	digestsNil, errNil := buildParallelRich(context.Background(), &withNilRail, []buildPhase{jobs}, 1, run, nil)
	require.NoError(t, errNil)

	var withRail bytes.Buffer
	rail := &fakeRail{steps: []tui.Step{{ID: "build", Label: "Build"}}, active: 0}
	digestsRail, errRail := buildParallelRich(context.Background(), &withRail, []buildPhase{jobs}, 1, run, rail)
	require.NoError(t, errRail)

	assert.Equal(t, digestsNil, digestsRail, "a non-nil rail must not change what buildParallelRich returns")
	assert.Equal(t, withNilRail.String(), withRail.String(),
		"off a real terminal, a non-nil rail renders byte-identically to a nil one — NewWithRail only composites the rail on a TTY")
}

// TestBuildTargetNames_CoverEveryCatalogTarget pins the accepted-target list to
// the catalog. The list used to be a hand-maintained string literal that had
// drifted: apimage.ToolchainClaude was in the catalog (and resolveBuildJobs
// accepted it) while --help and shell completion listed only toolchain-go and
// toolchain-node, so the command that provisions the claude overlay was
// undiscoverable from the CLI itself.
func TestBuildTargetNames_CoverEveryCatalogTarget(t *testing.T) {
	names := buildTargetNames()
	for _, im := range apimage.Catalog() {
		assert.Containsf(t, names, im.Target,
			"oap build must advertise catalog target %q", im.Target)
	}
	assert.Contains(t, names, "all", `the "all" pseudo-target stays advertised`)
	assert.Len(t, names, len(apimage.Catalog())+1,
		"exactly the catalog plus all — no hand-added extras")
}

func TestBuildHelpListsTargets(t *testing.T) {
	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"build", "--help"})
	require.NoError(t, root.Execute(), "build --help")
	for _, want := range []string{"operator", "sandbox", "runner", "toolchain-claude", "all"} {
		assert.Containsf(t, stdout.String(), want, "build help missing %q", want)
	}
}

// TestSyncBuffer_LastLine verifies that lastLine returns the last non-empty
// line of the captured output, trimmed of trailing whitespace, and handles
// edge cases (empty buffer, trailing newlines, blank lines at end).
func TestSyncBuffer_LastLine(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty buffer returns empty string", input: "", want: ""},
		{name: "single line no newline", input: "#3 RUN go build ./...", want: "#3 RUN go build ./..."},
		{name: "single line with newline", input: "#3 RUN go build ./...\n", want: "#3 RUN go build ./..."},
		{name: "multi-line: last non-empty returned", input: "#3 COPY . .\n#8 12.3 go: downloading example.com/mod v0.1.0\n", want: "#8 12.3 go: downloading example.com/mod v0.1.0"},
		{name: "trailing blank lines: last non-empty line returned", input: "#5 done\n\n\n", want: "#5 done"},
		{name: "trailing whitespace trimmed", input: "#9 exporting layers   \n", want: "#9 exporting layers"},
		{name: "only blank lines returns empty", input: "\n\n\n", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b syncBuffer
			_, err := b.Write([]byte(tc.input))
			require.NoError(t, err)
			assert.Equal(t, tc.want, b.lastLine())
		})
	}
}

func TestDockerBuildxArgs(t *testing.T) {
	got := dockerBuildxArgs("myreg.io/ap", "linux/amd64", "Dockerfile.services", "channelsd", "agentprimitives-channelsd:dev", ".", "/tmp/meta.json")
	assert.Equal(t, []string{
		"buildx", "build", "--platform", "linux/amd64",
		"-f", "Dockerfile.services",
		"--target", "channelsd",
		"--metadata-file", "/tmp/meta.json",
		"-t", "myreg.io/ap/agentprimitives-channelsd:dev", "--push", ".",
	}, got)
}

func TestDockerBuildxArgs_inferredDockerfile(t *testing.T) {
	got := dockerBuildxArgs("myreg.io/ap", "linux/amd64", "", "", "pi-detector:dev", "images/promptinjection-detector", "")
	assert.Equal(t, []string{
		"buildx", "build", "--platform", "linux/amd64",
		"-t", "myreg.io/ap/pi-detector:dev", "--push", "images/promptinjection-detector",
	}, got)
}

// TestDockerBuildxArgs_cacheOnly: a job with no tag must produce NO image — no
// -t, no --push, no --metadata-file — so the build only warms BuildKit's cache
// and can never be mistaken for something installable or pushed to the registry
// under an accidental name.
func TestDockerBuildxArgs_cacheOnly(t *testing.T) {
	got := dockerBuildxArgs("myreg.io/ap", "linux/amd64", "Dockerfile.services", "builder", "", ".", "")
	assert.Equal(t, []string{
		"buildx", "build", "--platform", "linux/amd64",
		"-f", "Dockerfile.services",
		"--target", "builder",
		"--output=type=cacheonly",
		".",
	}, got)
	assert.NotContains(t, got, "--push", "a cache-only prebuild must never push")
	assert.NotContains(t, got, "-t", "a cache-only prebuild must never tag")
}

func TestBuildTargets_includesDetector(t *testing.T) {
	var found bool
	for _, tgt := range buildTargets {
		if tgt.defaultTag == "pi-detector:dev" {
			found = true
			assert.Equal(t, "images/promptinjection-detector", tgt.context,
				"detector target must use images/promptinjection-detector as build context")
			assert.Empty(t, tgt.dockerfile,
				"detector target must have empty dockerfile (docker infers <context>/Dockerfile)")
		}
	}
	assert.True(t, found, "oap build must include the prompt-injection detector image (pi-detector:dev)")
}

// TestOrchestrateBuilds_RawRunsPhasesInOrder: the raw/non-TTY path drains each
// phase fully before starting the next, so a shared-compile phase 0 is always
// complete before the images that COPY from it build.
func TestOrchestrateBuilds_RawRunsPhasesInOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	run := func(_ context.Context, tg buildTarget, _ string, _, _ io.Writer) (string, error) {
		mu.Lock()
		order = append(order, tg.name)
		mu.Unlock()
		return "", nil
	}
	var out, errOut bytes.Buffer
	phases := []buildPhase{{job("shared")}, {job("alpha"), job("beta")}}
	_, err := orchestrateBuilds(context.Background(), &out, &errOut, phases, true /*raw*/, run, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"shared", "alpha", "beta"}, order, "phase 0 runs to completion first")
}

// TestBuildParallelRich_PhaseBarrier: no job in phase 1 may start until every
// job in phase 0 has finished.
//
// This barrier is what the prebuild relies on: without it, several concurrent
// builds of images sharing one builder stage would each race to produce the
// same vertex.
func TestBuildParallelRich_PhaseBarrier(t *testing.T) {
	var sharedDone atomic.Bool
	var violations atomic.Int32
	run := func(_ context.Context, tg buildTarget, _ string, _, _ io.Writer) (string, error) {
		if tg.name == "shared" {
			time.Sleep(60 * time.Millisecond)
			sharedDone.Store(true)
			return "", nil
		}
		if !sharedDone.Load() {
			violations.Add(1)
		}
		return "", nil
	}
	var out bytes.Buffer
	phases := []buildPhase{{job("shared")}, {job("alpha"), job("beta"), job("gamma")}}
	_, err := buildParallelRich(context.Background(), &out, phases, 4, run, nil)
	require.NoError(t, err)
	assert.Zero(t, violations.Load(), "no phase-1 job started before phase 0 completed")
}

// TestBuildParallelRich_PhaseZeroFailureSkipsPhaseOne: a failing shared compile
// aborts the run before any image build starts, and its captured output is
// surfaced so the failure stays debuggable.
func TestBuildParallelRich_PhaseZeroFailureSkipsPhaseOne(t *testing.T) {
	var phaseOneStarted atomic.Bool
	run := func(_ context.Context, tg buildTarget, _ string, stdout, _ io.Writer) (string, error) {
		if tg.name == "shared" {
			// A delay before failing gives a no-barrier implementation time to
			// start phase 1's jobs concurrently with "shared" and flip
			// phaseOneStarted, so the assertion below would deterministically
			// catch a missing barrier instead of depending on the gctx.Err()
			// check winning a race against goroutine scheduling.
			time.Sleep(60 * time.Millisecond)
			fmt.Fprintln(stdout, "FAILMARKER: shared compile blew up")
			return "", fmt.Errorf("docker build shared: exit status 1")
		}
		phaseOneStarted.Store(true)
		return "", nil
	}
	var out bytes.Buffer
	phases := []buildPhase{{job("shared")}, {job("alpha"), job("beta")}}
	_, err := buildParallelRich(context.Background(), &out, phases, 4, run, nil)

	require.Error(t, err, "a failing shared compile fails the build")
	assert.Contains(t, err.Error(), "shared")
	assert.False(t, phaseOneStarted.Load(), "phase 1 never started")
	assert.Contains(t, out.String(), "FAILMARKER: shared compile blew up",
		"the failing phase's captured output is surfaced")
}

// goSvcJob is a fixture job shaped like a Go service image: a stage of the
// shared services Dockerfile.
func goSvcJob(name string) buildJob {
	return buildJob{
		t: buildTarget{
			name:       name,
			dockerfile: apimage.GoBuilder.Dockerfile,
			context:    apimage.GoBuilder.Context,
			stage:      name,
			defaultTag: name + ":dev",
		},
		tag: name + ":dev",
	}
}

// TestPrebuildJobs covers the rule that decides whether the shared Go compile
// gets its own phase. Emitting it for a single image is pure overhead (that
// build produces the builder vertex itself); omitting it for several means they
// all start cold and race to produce the same vertex.
func TestPrebuildJobs(t *testing.T) {
	cases := []struct {
		name string
		jobs []buildJob
		want bool
	}{
		{name: "five Go services: shared compile is emitted", jobs: []buildJob{goSvcJob("operator"), goSvcJob("runner"), goSvcJob("channelsd"), goSvcJob("webd"), goSvcJob("authzd")}, want: true},
		{name: "two Go services: shared compile is emitted", jobs: []buildJob{goSvcJob("webd"), goSvcJob("authzd")}, want: true},
		{name: "one Go service: no shared compile, that build makes the vertex", jobs: []buildJob{goSvcJob("runner")}, want: false},
		{name: "no Go services: no shared compile", jobs: []buildJob{job("sandbox"), job("toolchain-go")}, want: false},
		{name: "one Go service plus other images: no shared compile", jobs: []buildJob{goSvcJob("runner"), job("sandbox")}, want: false},
		{name: "empty job set: no shared compile", jobs: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pre := prebuildJobs(tc.jobs)
			if !tc.want {
				assert.Empty(t, pre)
				return
			}
			require.Len(t, pre, 1, "exactly one shared compile")
			assert.Empty(t, pre[0].tag, "the shared compile is cache-only: no tag, no image, no push")
			assert.Equal(t, apimage.GoBuilder.Dockerfile, pre[0].t.dockerfile)
			assert.Equal(t, apimage.GoBuilder.Stage, pre[0].t.stage)
			assert.Equal(t, apimage.GoBuilder.Context, pre[0].t.context)
			assert.NotEmpty(t, pre[0].t.name, "the progress row needs a label")
		})
	}
}

// TestPrebuildJobs_ContributesNoDigest guards the registry-path invariant: the
// shared compile has no catalog entry and no tag, so it must never land in the
// digest map that the manifest rewrite plants refs from.
func TestPrebuildJobs_ContributesNoDigest(t *testing.T) {
	pre := prebuildJobs([]buildJob{goSvcJob("webd"), goSvcJob("authzd")})
	require.Len(t, pre, 1)
	assert.Empty(t, imageName(pre[0].t.defaultTag), "the shared compile has no image name to key a digest by")
}
