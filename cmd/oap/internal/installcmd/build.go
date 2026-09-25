package installcmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/buildx"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
)

type buildTarget struct {
	name       string // operator | sandbox | runner | ...
	dockerfile string // if empty, docker infers <context>/Dockerfile
	defaultTag string
	context    string // build context dir; defaults to "." when empty
	stage      string // if non-empty, `docker build --target <stage>`
}

// buildTargetsFrom converts an apimage.Image list into the []buildTarget shape
// build.go operates on. Shared by buildTargets (Catalog(), for single-target
// lookup/validation) and allBuildTargets (All only, for the "all" expansion)
// so the two lists derive from the same conversion instead of duplicating it.
func buildTargetsFrom(images []apimage.Image) []buildTarget {
	out := make([]buildTarget, 0, len(images))
	for _, im := range images {
		out = append(out, buildTarget{
			name:       im.Target,
			dockerfile: im.Dockerfile,
			defaultTag: im.LocalRef(),
			context:    im.Context,
			stage:      im.Stage,
		})
	}
	return out
}

// buildTargets is derived from the apimage catalog (platform images plus
// language-toolchain overlays) so a new image is added in one place
// (pkg/platform/apimage). This is the set `oap build <name>` accepts for single-target
// lookup and validation — it intentionally includes toolchain images (e.g.
// "toolchain-go") because they are valid explicit targets. Order + values
// match the catalog.
var buildTargets = buildTargetsFrom(apimage.Catalog())

// allBuildTargets is what a LOCAL `oap build all` (no --image-registry) expands
// to: apimage.All ONLY, never apimage.Toolchains. `oap build` with no argument
// defaults to target "all" (see runBuild), and the kind-smoke bootstrap runs
// `oap build all --no-load` — so expanding "all" over the toolchain images would
// compile gopls from source (images/toolchain-go's `go install
// golang.org/x/tools/gopls@…`) and add a Go-module-proxy network dependency to
// every normal dev-loop build and every smoke run. Locally, toolchain images
// are built explicitly by name (`oap build toolchain-go`), never implicitly via
// "all".
//
// A REGISTRY build is the opposite: `oap build all --image-registry` expands
// over the full apimage.Catalog(), toolchains included, because the install it
// feeds plants a registry ref for every catalog image. resolveBuildJobs below
// owns that split and documents why — read it before assuming "all" means the
// same set on both paths. Two lists disagreeing about this is exactly the drift
// that shipped SpiceboxToolchain CRs pointing at images nobody pushed.
var allBuildTargets = buildTargetsFrom(apimage.All)

// buildTargetNames is every accepted `oap build <target>` argument: each catalog
// target plus the "all" pseudo-target. Derived from buildTargets so a new
// apimage entry is advertised in --help and shell completion the moment it
// joins the catalog. It replaced two hand-maintained string literals that had
// already drifted — apimage.ToolchainClaude was in the catalog and resolved
// fine, but neither the Use string nor ValidArgs mentioned it, so the only
// documented way to provision the claude overlay did not exist.
func buildTargetNames() []string {
	out := make([]string, 0, len(buildTargets)+1)
	for _, t := range buildTargets {
		out = append(out, t.name)
	}
	return append(out, "all")
}

// dockerBuildxArgs builds the `docker buildx build` argv for the registry path:
// a single-platform cross-build pushed straight to <registry>/<localTag>. We
// push (not --load) because a cross-arch image can't be loaded into the local
// daemon. localTag is the image's default :dev tag; we prefix the registry.
//
// An EMPTY localTag is a cache-only job: it emits no -t and no --push, so the
// build only materializes the selected stage into BuildKit's cache. Tagging or
// pushing it would put an image nobody can install under a name nobody expects.
// It also emits --output=type=cacheonly: on the "docker" driver (the default
// builder), an untagged --target build otherwise still lands the stage as a
// dangling image in the daemon's image store — several GB per invocation —
// rather than staying cache-only the way the rest of this comment assumes.
func dockerBuildxArgs(registry, platform, dockerfile, stage, localTag, buildCtx, metadataFile string) []string {
	args := []string{"buildx", "build", "--platform", platform}
	if dockerfile != "" {
		args = append(args, "-f", dockerfile)
	}
	if stage != "" {
		args = append(args, "--target", stage)
	}
	if metadataFile != "" {
		args = append(args, "--metadata-file", metadataFile)
	}
	if localTag != "" {
		args = append(args, "-t", strings.TrimSuffix(registry, "/")+"/"+localTag, "--push")
	} else {
		args = append(args, "--output=type=cacheonly")
	}
	return append(args, buildCtx)
}

func NewBuildCmd(g *apcmd.Globals) *cobra.Command {
	noLoad := false
	tag := ""
	registry := ""
	platform := "linux/amd64"
	mirrorDeps := false
	createRegistry := false
	raw := false

	cmd := &cobra.Command{
		Use:       "build [" + strings.Join(buildTargetNames(), "|") + "]",
		Short:     "Build (and load) Docker images",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: buildTargetNames(),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "all"
			if len(args) == 1 {
				target = args[0]
			}
			_, err := runBuild(cmd.Context(), cmd.OutOrStdout(), target, tag, noLoad, registry, platform, mirrorDeps, createRegistry, raw, g, nil)
			return err
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "Override the image TAG only, e.g. 'dev-fix1' (the image name is kept); not a full name:tag. Only valid when target != all")
	cmd.Flags().BoolVar(&noLoad, "no-load", false, "Skip cluster image-load (kind/k3d/minikube)")
	cmd.Flags().StringVar(&registry, "image-registry", "", "Build for the cluster arch and push to <registry>/<name>:<tag> (instead of local load)")
	cmd.Flags().StringVar(&platform, "platform", "linux/amd64", "Target platform for --image-registry builds")
	cmd.Flags().BoolVar(&mirrorDeps, "mirror-dependencies", false, "Also mirror public dependency images under --image-registry (air-gapped)")
	cmd.Flags().BoolVar(&createRegistry, "create-registry", false, "Create the registry repository if missing without prompting (GKE Artifact Registry)")
	cmd.Flags().BoolVar(&raw, "raw", false, "Stream raw docker output instead of the live per-image progress UI")
	return cmd
}

// rail is nil for every non-wizard caller (NewBuildCmd's standalone `oap
// build`, and the linear `oap init`'s runBuildFn call) — orchestrateBuilds
// forwards a nil rail to progress.New verbatim via NewWithRail, so their
// checklist is byte-for-byte unchanged. Only the unified init wizard passes a
// non-nil rail, to draw its step gutter beside the build checklist.
func runBuild(ctx context.Context, out io.Writer, target, tagOverride string, noLoad bool, registry, platform string, mirrorDeps, createRegistry, raw bool, g *apcmd.Globals, rail progress.RailProvider) (map[string]string, error) {
	if mirrorDeps && registry == "" {
		return nil, fmt.Errorf("--mirror-dependencies requires --image-registry")
	}
	// Make the registry pushable before building: wire docker auth and ensure
	// the repository exists (GKE Artifact Registry today). No-op for local builds
	// and registries we don't manage.
	if registry != "" {
		confirmFn := func(prompt string) bool {
			cliout.Prompt(out, "%s [Y/n] ", prompt)
			sc := bufio.NewScanner(os.Stdin)
			if !sc.Scan() {
				return true
			}
			ans := strings.ToLower(strings.TrimSpace(sc.Text()))
			return ans == "" || ans == "y" || ans == "yes"
		}
		if err := gke.EnsureRegistryReady(ctx, newCloudReporter(out), registry, apcmd.StdinIsInteractive(os.Stdin), createRegistry, confirmFn); err != nil {
			return nil, err
		}
	}
	jobs, err := resolveBuildJobs(target, tagOverride, registry)
	if err != nil {
		return nil, err
	}
	// The runner is the seam between orchestration (parallel rows / raw stream)
	// and the docker invocation: it builds one target, writing its human-facing
	// output to stdout and its diagnostic output to stderr. Production binds it to
	// buildOne; tests stub it to drive the orchestration without docker.
	runner := func(ctx context.Context, t buildTarget, tag string, stdout, stderr io.Writer) (string, error) {
		return buildOne(ctx, stdout, stderr, t, tag, noLoad, registry, platform, g)
	}
	// The shared Go compile, when the job set warrants it, runs as phase 0 so
	// the images that COPY from it build against a warm cache. Phase 1 is
	// every job in this resolved set, unconditionally — including images like
	// sandbox and promptinjection-detector that do not derive from the shared
	// builder (apimage.UsesGoBuilder is false for them) and so gain nothing
	// from waiting behind phase 0. That is not a deliberate choice: the phase
	// split is binary (prebuild jobs vs. everything else) and nothing narrows
	// phase 1 to just the builder-derived jobs. Doing so is an available
	// optimization — it has not been measured, and changing the split is a
	// separate, measured follow-up, not something to do here.
	phases := make([]buildPhase, 0, 2)
	if pre := prebuildJobs(jobs); len(pre) > 0 {
		phases = append(phases, buildPhase(pre))
	}
	phases = append(phases, buildPhase(jobs))
	digests, err := orchestrateBuilds(ctx, out, os.Stderr, phases, raw, runner, rail)
	if err != nil {
		return nil, err
	}
	if mirrorDeps {
		fmt.Fprintln(out, "==> mirror dependency images")
		if err := mirrorDependencies(ctx, out, registry); err != nil {
			return nil, err
		}
	}
	return digests, nil
}

// buildJob pairs a build target with the concrete tag to build it under.
type buildJob struct {
	t   buildTarget
	tag string
}

// resolveBuildJobs expands a target argument ("all" or a single image name) into
// the list of jobs to build, preserving the original --tag validation and
// unknown-target errors.
//
// registry selects the "all" expansion, and the two cases genuinely differ:
//
//   - LOCAL (registry == "") expands to apimage.All only. Compiling a language
//     toolchain from source — images/toolchain-go's `go install
//     golang.org/x/tools/gopls@…` — on every dev-loop build and every kind-smoke
//     run is exactly the cost the overlays were split out of the base image to
//     avoid. A local cluster gets its overlays from an explicit `oap build
//     toolchain-<x>` + image load (magefiles/magefile.go's smoke bootstrap) or
//     from the desktop dev bake (apimage.DesktopBakeImages(dev=true)).
//   - REGISTRY (registry != "") expands to the full apimage.Catalog(). The
//     manifest rewrite (pkg/platform/manifests/substitute.go -> apimage.ResolveDigests)
//     plants a <registry>/<name>:<tag> reference for EVERY catalog image into
//     the installed CRs, whether or not this build produced it. An overlay
//     skipped here is therefore a reference to an image that exists nowhere:
//     the install still exits 0, and the failure surfaces days later as
//     Init:ErrImagePull on the first session whose SpiceboxClass composes that
//     toolchain. Provisioning what we plant is the invariant
//     (TestRegistryBuildProvisionsEveryPlantedRef).
func resolveBuildJobs(target, tagOverride, registry string) ([]buildJob, error) {
	if target == "all" {
		if tagOverride != "" {
			return nil, fmt.Errorf("--tag is incompatible with target=all")
		}
		expand := allBuildTargets
		if registry != "" {
			expand = buildTargets // the full catalog: All + Toolchains
		}
		jobs := make([]buildJob, 0, len(expand))
		for _, t := range expand {
			jobs = append(jobs, buildJob{t: t, tag: t.defaultTag})
		}
		return jobs, nil
	}
	for _, t := range buildTargets {
		if t.name == target {
			tag := t.defaultTag
			if tagOverride != "" {
				// --tag overrides only the TAG portion of the image's
				// "<name>:<tag>" localTag; the image NAME is preserved. A bare
				// "--tag dev-fix1" previously replaced the whole localTag, so
				// the build pushed to <registry>/dev-fix1 (name dropped) — a
				// silent footgun. Reject a "name:tag" form to make the contract
				// obvious.
				if strings.ContainsAny(tagOverride, ":/") {
					return nil, fmt.Errorf(
						"--tag is the image tag only (e.g. 'dev-fix1'), not %q — the image name %q is kept automatically",
						tagOverride, imageName(t.defaultTag))
				}
				tag = imageName(t.defaultTag) + ":" + tagOverride
			}
			return []buildJob{{t: t, tag: tag}}, nil
		}
	}
	return nil, fmt.Errorf("unknown target %q", target)
}

// imageName returns the "<name>" portion of a "<name>:<tag>" localTag.
func imageName(localTag string) string {
	if i := strings.LastIndex(localTag, ":"); i >= 0 {
		return localTag[:i]
	}
	return localTag
}

// buildRunner builds (and loads/pushes) one target, writing its human-facing
// output to stdout and diagnostics to stderr. It is the test seam for the build
// orchestration: production binds it to buildOne. It returns the pushed digest
// ("sha256:…") on the registry path, or "" on the local path.
type buildRunner func(ctx context.Context, t buildTarget, tag string, stdout, stderr io.Writer) (string, error)

const (
	// buildMaxParallel bounds how many images build concurrently; the effective
	// limit is min(buildMaxParallel, NumCPU).
	buildMaxParallel = 4
	// buildETA is the per-image expected build time. It drives the "~3m" hint on
	// the row (a docker build has no honest percentage, so we show spinner +
	// elapsed + ETA, not a byte bar).
	buildETA = 3 * time.Minute
	// buildDeadline is effectively "wait for docker": far longer than any real
	// build, so the progress reporter never prompts mid-build. A genuinely hung
	// docker exec eventually trips it and fails the row, matching the old
	// behavior of waiting on docker until ctx cancellation.
	buildDeadline = 24 * time.Hour
	// statusUpdateInterval is how often the per-image status-updater goroutine
	// reads the build's last output line and feeds it to the phase row. 450ms
	// is imperceptibly slower than the 100ms tick rate, so users always see
	// a recent step without a distracting flicker on each frame.
	statusUpdateInterval = 450 * time.Millisecond
)

// buildConcurrency is the effective parallelism: at most buildMaxParallel, never
// more than the CPU count, never below 1.
func buildConcurrency() int {
	n := min(buildMaxParallel, runtime.NumCPU())
	if n < 1 {
		n = 1
	}
	return n
}

// buildPhase is one ordered stage of a build run: every job in it runs
// concurrently, and the phase completes before the next begins.
//
// The ordering exists so that when a build set contains images sharing a parent
// stage (see prebuildJobs), that stage is materialized before the images that
// COPY from it build.
type buildPhase []buildJob

// prebuildJobs returns the jobs that must complete before jobs can be fanned
// out. An image that is a thin final stage over a shared builder (see
// apimage.UsesGoBuilder) contributes nothing on its own: several such images
// built concurrently from a cold cache each race to produce the same builder
// vertex. Materializing that vertex once first makes the fan-out an ordinary
// layer-cache hit instead of a bet on BuildKit deduplicating identical vertices
// across concurrent solves.
//
// The returned job carries NO tag, which buildOne reads as cache-only: no -t,
// no --push, no --load, no digest. It is not an image and deliberately has no
// apimage catalog entry.
func prebuildJobs(jobs []buildJob) []buildJob {
	n := 0
	for _, j := range jobs {
		if apimage.UsesGoBuilder(j.t.dockerfile, j.t.stage) {
			n++
		}
	}
	if n < apimage.GoBuilderPrebuildMin {
		return nil
	}
	return []buildJob{{t: buildTarget{
		name:       "go-services (shared compile)",
		dockerfile: apimage.GoBuilder.Dockerfile,
		context:    apimage.GoBuilder.Context,
		stage:      apimage.GoBuilder.Stage,
	}}}
}

// orchestrateBuilds runs the phases in order — every job within a phase
// concurrently, each phase drained before the next — either as a live parallel
// checklist (rich) or as a sequential raw stream. Raw is selected when --raw is
// set or out is not a TTY (CI/piped): a live checklist would be corrupted by
// interleaved raw docker output, so non-rich runs stream verbatim and stay
// sequential. rail is forwarded to the rich path only — the raw path renders
// nothing, so it has no gutter to draw.
func orchestrateBuilds(ctx context.Context, out, errOut io.Writer, phases []buildPhase, raw bool, run buildRunner, rail progress.RailProvider) (map[string]string, error) {
	if raw || !cliout.IsTTY(out) {
		digests := map[string]string{}
		for _, ph := range phases {
			for _, j := range ph {
				d, err := run(ctx, j.t, j.tag, out, errOut)
				if err != nil {
					return nil, err
				}
				if d != "" {
					digests[imageName(j.t.defaultTag)] = d
				}
			}
		}
		return digests, nil
	}
	return buildParallelRich(ctx, out, phases, buildConcurrency(), run, rail)
}

// syncBuffer is a mutex-guarded byte buffer. A single image's captured docker
// stdout and stderr are written by exec's two pump goroutines (the registry path
// also tees stderr through a MultiWriter), so the sink must be concurrency-safe.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lastLine returns the last non-empty line of the captured output, trimmed of
// trailing whitespace. It is used by the status-updater goroutine in
// runPhase to feed the build's current step to the phase row.
// Returns an empty string if no output has been written yet.
func (b *syncBuffer) lastLine() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	// Walk backward over '\n'-delimited lines to find the last non-empty one.
	for len(s) > 0 {
		end := len(s)
		// Trim any trailing newline to avoid a spurious empty "line" at the end.
		if s[end-1] == '\n' {
			s = s[:end-1]
			continue
		}
		// Find the start of the last line.
		start := strings.LastIndex(s, "\n") + 1
		line := strings.TrimRight(s[start:], "\r\t ")
		if line != "" {
			return line
		}
		// The last segment was blank; drop it and keep looking.
		if start == 0 {
			return ""
		}
		s = s[:start-1]
	}
	return ""
}

// buildResult tracks one image's outcome: its captured docker output, pushed
// digest (non-empty only on the registry path), and the error its goroutine
// returned (read only after the errgroup has joined).
type buildResult struct {
	job    buildJob
	buf    *syncBuffer
	digest string
	err    error
}

// buildParallelRich builds the phases in order, rendering one progress row per
// image through a single reporter that spans every phase so the checklist stays
// continuous. Each image's docker output is captured to a per-image buffer (so
// the live rows aren't corrupted); on failure the failing image's captured
// output is printed below the checklist so the error stays debuggable. A phase
// that fails aborts the run before the next phase starts.
//
// rail == nil behaves EXACTLY like the old progress.New call — NewWithRail
// forwards a nil rail there directly — so a standalone `oap build` (and the
// linear `oap init`'s build phase) is provably unaffected by this parameter
// existing at all. Only the unified init wizard passes a non-nil rail, to
// composite its step gutter beside the build checklist.
func buildParallelRich(ctx context.Context, out io.Writer, phases []buildPhase, concurrency int, run buildRunner, rail progress.RailProvider) (map[string]string, error) {
	rep := progress.NewWithRail(out, os.Stdin, true /*assumeYes: builds never prompt to keep waiting*/, rail)

	digests := map[string]string{}
	for _, ph := range phases {
		results, err := runPhase(ctx, rep, ph, concurrency, run)
		if err != nil {
			_ = rep.Close()
			// Surface the captured output of the image whose error the group
			// returned (the root failure); the other rows were aborted by the
			// cancellation and have no useful output.
			for _, r := range results {
				if r.err != nil && r.err == err {
					if captured := r.buf.String(); captured != "" {
						fmt.Fprintf(out, "\n--- docker output for failed build %q ---\n%s\n", r.job.t.name, captured)
					}
					break
				}
			}
			return nil, err
		}
		for _, r := range results {
			if r.digest != "" {
				digests[imageName(r.job.t.defaultTag)] = r.digest
			}
		}
	}
	_ = rep.Close()
	return digests, nil
}

// runPhase builds every job in one phase concurrently (bounded by concurrency),
// returning each job's result. The first failure cancels the shared context,
// aborting the other in-flight builds in this phase.
func runPhase(ctx context.Context, rep progress.Reporter, ph buildPhase, concurrency int, run buildRunner) ([]*buildResult, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	grp, gctx := errgroup.WithContext(ctx)
	grp.SetLimit(concurrency)

	results := make([]*buildResult, len(ph))
	for i := range ph {
		results[i] = &buildResult{job: ph[i], buf: &syncBuffer{}}
	}

	for i := range ph {
		res := results[i]
		grp.Go(func() error {
			// A sibling already failed and cancelled the group: skip starting a
			// row for a build that would only be killed anyway.
			if err := gctx.Err(); err != nil {
				return err
			}

			phase := rep.Phase("build " + res.job.t.name)

			// Run docker in its own goroutine so the phase's Await can drive the
			// spinner while it runs; the poll observes an atomic done flag.
			var done atomic.Bool
			buildDone := make(chan struct{})
			go func() {
				res.digest, res.err = run(gctx, res.job.t, res.job.tag, res.buf, res.buf)
				done.Store(true)
				close(buildDone)
			}()

			// Status-updater: every ~450ms read the build's last output line and
			// feed it to the phase row so the user sees the current build step
			// (e.g. "#8 12.3 go: downloading …") instead of the generic "waiting".
			// The goroutine exits when buildDone is closed; statusDone signals that
			// it has fully exited so phase.Done()/Fail() can't race with Status().
			statusDone := make(chan struct{})
			go func() {
				ticker := time.NewTicker(statusUpdateInterval)
				defer ticker.Stop()
				defer close(statusDone)
				for {
					select {
					case <-buildDone:
						return
					case <-ticker.C:
						phase.Status(res.buf.lastLine())
					}
				}
			}()

			poll := func(context.Context) (bool, error) { return done.Load(), nil }
			awaitErr := phase.Await(gctx, ctx, buildDeadline, buildETA, poll, nil)
			<-buildDone  // docker goroutine has fully finished; res.err/res.buf are final
			<-statusDone // status-updater goroutine has exited; phase.Status() will not fire again

			err := res.err
			if err == nil {
				// Await returned before the build errored (ctx cancel / deadline).
				err = awaitErr
			}
			res.err = err
			if err != nil {
				phase.Fail()
				return err
			}
			phase.Done()
			return nil
		})
	}

	return results, grp.Wait()
}

// buildOne builds (and loads/pushes) a single target. All human-facing echo +
// docker stdout go to stdout; docker stderr goes to stderr. In the rich path
// both are the same per-image capture buffer; in the raw path they are out and
// os.Stderr respectively (today's behavior). An empty tag is a cache-only job:
// it builds t.stage to warm BuildKit's cache and returns no digest, skipping
// -t/--push/--load and the metadata-file dance entirely.
func buildOne(ctx context.Context, stdout, stderr io.Writer, t buildTarget, tag string, noLoad bool, registry, platform string, g *apcmd.Globals) (string, error) {
	buildCtx := t.context
	if buildCtx == "" {
		buildCtx = "."
	}

	// Registry path: cross-build for the target platform and push. No local load.
	if registry != "" {
		// A cache-only job (empty tag) produces no image, so there is no digest
		// to read back and no metadata file to ask for.
		mfPath := ""
		if tag != "" {
			mf, err := os.CreateTemp("", "ap-buildx-meta-*.json")
			if err != nil {
				return "", fmt.Errorf("create buildx metadata file: %w", err)
			}
			mfPath = mf.Name()
			_ = mf.Close()
			defer os.Remove(mfPath)
		}

		args := dockerBuildxArgs(registry, platform, t.dockerfile, t.stage, tag, buildCtx, mfPath)
		fmt.Fprintf(stdout, "==> docker %s\n", strings.Join(args, " "))
		// Each attempt gets a fresh stderr capture: classification must read the
		// failure that ended THIS attempt, not one a previous attempt survived.
		push := func(ctx context.Context) (string, error) {
			var translate bytes.Buffer
			cmd := exec.CommandContext(ctx, "docker", args...)
			cmd.Stdout = stdout
			cmd.Stderr = io.MultiWriter(stderr, &translate) // tee, so we can translate auth/repo failures
			err := cmd.Run()                                // must complete before the capture is read
			return translate.String(), err
		}
		if err := buildx.RunPush(ctx, stdout, t.name, registry, buildx.Retry{}, push); err != nil {
			return "", err
		}
		if tag == "" {
			return "", nil
		}
		meta, err := os.ReadFile(mfPath)
		if err != nil {
			return "", fmt.Errorf("read buildx metadata for %s: %w", t.name, err)
		}
		digest, err := buildx.MetadataDigest(meta)
		if err != nil {
			return "", fmt.Errorf("resolve pushed digest for %s: %w", t.name, err)
		}
		return digest, nil
	}

	args := []string{"build"}
	if t.dockerfile != "" {
		args = append(args, "-f", t.dockerfile)
	}
	if t.stage != "" {
		args = append(args, "--target", t.stage)
	}
	// An empty tag is a cache-only job: build the stage to warm BuildKit's
	// cache, produce no image, and skip the load path entirely.
	// --output=type=cacheonly makes that literal: on the "docker" driver (the
	// default builder), an untagged --target build otherwise still lands the
	// stage as a dangling image in the daemon's image store rather than
	// staying cache-only.
	if tag != "" {
		args = append(args, "-t", tag)
	} else {
		args = append(args, "--output=type=cacheonly")
	}
	args = append(args, buildCtx)
	// Echo the exact argv rather than a reconstruction, so what the user sees is
	// what runs.
	fmt.Fprintf(stdout, "==> docker %s\n", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build %s: %w", t.name, err)
	}
	if tag == "" || noLoad {
		return "", nil
	}

	// Resolve current context.
	kctx, err := g.CurrentContext()
	if err != nil {
		fmt.Fprintf(stdout, "    (skipping image-load: %v)\n", err)
		return "", nil
	}
	plan := imageload.For(kctx, tag)
	switch plan.Disposition {
	case imageload.NotNeeded:
		fmt.Fprintf(stdout, "    (no image-load needed for context %q)\n", kctx)
		return "", nil
	case imageload.NeedsRegistry:
		// Reaching here means a local build for a cluster that cannot receive
		// one. Silently succeeding leaves an image nowhere the cluster can see.
		return "", fmt.Errorf("built %s locally but context %q has no image-load path — pass --image-registry <registry> to push it instead (or --no-load if that is intentional)", tag, kctx)
	case imageload.VMLoad:
		// Unlike loadImageIntoCluster (in image.go), buildOne has no SSH/VM
		// machinery of its own — it only knows how to exec plan.Argv, which
		// VMLoad leaves nil. Falling through to that exec would index a nil
		// slice and panic. `oap image load` already implements the VM path, so
		// point there instead of crashing or (as before this fix) silently
		// claiming "no image-load needed" for a context that plainly needs one.
		return "", fmt.Errorf("built %s locally but context %q is the oap-desktop VM — run `oap image load %s` to load it (or pass --no-load if that is intentional)", tag, kctx, tag)
	case imageload.LocalLoad:
		fmt.Fprintf(stdout, "==> %s\n", strings.Join(plan.Argv, " "))
		loadCmd := exec.CommandContext(ctx, plan.Argv[0], plan.Argv[1:]...)
		loadCmd.Stdout = stdout
		loadCmd.Stderr = stderr
		if err := loadCmd.Run(); err != nil {
			return "", fmt.Errorf("image-load: %w", err)
		}
		return "", nil
	default:
		// Unreachable while imageload's classification stays total; adding a
		// disposition must surface as an error here, not as an index into the
		// nil Argv that every non-LocalLoad disposition carries.
		return "", fmt.Errorf("unhandled image-load disposition %s for context %q: cannot load %s", plan.Disposition, kctx, tag)
	}
}
