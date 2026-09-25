//go:build mage
// +build mage

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"

	"github.com/authzed/openagentprimitives/pkg/gen/auditgen"
	"github.com/authzed/openagentprimitives/pkg/gen/claudeexec"
	"github.com/authzed/openagentprimitives/test/envtestreap"
	"github.com/authzed/openagentprimitives/test/suitelock"
	"github.com/authzed/openagentprimitives/test/testparallel"
	"github.com/authzed/openagentprimitives/test/testpostgres"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

type Gen mg.Namespace

// Api runs controller-gen to produce CRDs, deepcopy, and RBAC from types.
func (Gen) Api() error {
	// allowDangerousTypes=true permits float64 in CRD schemas. Required for the
	// content-inspection classifier scores (Score/Threshold on
	// PendingContentInspectionApproval), which are bounded to [0,1], come from an
	// internal ONNX call (never user input), and so carry no NaN/Inf etcd risk.
	// Constraint for future CRD fields: do NOT add unbounded/user-supplied floats;
	// prefer scaled int (e.g. milliunits, converting scores to int32).
	return sh.RunV("go", "tool", "controller-gen",
		"crd:allowDangerousTypes=true",
		"object",
		"rbac:roleName=spicebox-operator",
		"paths=./pkg/apis/...",
		"paths=./pkg/controllers/...",
		"output:crd:artifacts:config=./config/crds",
		"output:rbac:artifacts:config=./config/manager",
	)
}

// Proto regenerates gRPC types from pkg/web/gateway/gateway.proto.
// Requires protoc on PATH (brew install protobuf or apt-get install protobuf-compiler)
// and protoc-gen-go + protoc-gen-go-grpc on PATH (typically $(go env GOPATH)/bin).
func (Gen) Proto() error {
	return sh.RunV("protoc",
		"--go_out=./pkg/web/gateway/v1", "--go_opt=paths=source_relative",
		"--go-grpc_out=./pkg/web/gateway/v1", "--go-grpc_opt=paths=source_relative",
		"--proto_path=./pkg/web/gateway",
		"pkg/web/gateway/gateway.proto",
	)
}

type Build mg.Namespace

func (Build) Operator() error {
	return sh.RunV("go", "build", "-o", "bin/spicebox-operator", "./internal/cmd/operator")
}

func (Build) Runner() error {
	return sh.RunV("go", "build", "-o", "bin/agentprimitives-runner", "./internal/cmd/runner")
}

// Channelsd builds the channelsd binary.
func (Build) Channelsd() error {
	return sh.RunV("go", "build", "-o", "bin/agentprimitives-channelsd", "./internal/cmd/channelsd")
}

// Webd builds the webd binary — the browser-UI host. webd hosts the
// identityd routes (credential linking, OAuth, portal) in-process; the
// standalone identityd binary has been retired.
func (Build) Webd() error {
	return sh.RunV("go", "build", "-o", "bin/agentprimitives-webd", "./internal/cmd/webd")
}

// Authzd builds the authzd binary — the asynchronous entity-extraction
// and authorization daemon.
func (Build) Authzd() error {
	return sh.RunV("go", "build", "-o", "bin/agentprimitives-authzd", "./internal/cmd/authzd")
}

// Extractord builds the extractord binary — the zero-egress attachment
// text-extraction service.
func (Build) Extractord() error {
	return sh.RunV("go", "build", "-o", "bin/agentprimitives-extractord", "./internal/cmd/extractord")
}

// Workshop builds the ap-workshop binary — the agent-builder sidecar's
// Streamable-HTTP MCP server (internal/cmd/workshop).
func (Build) Workshop() error {
	return sh.RunV("go", "build", "-o", "bin/ap-workshop", "./internal/cmd/workshop")
}

// Oap builds the agent-primitives CLI binary.
func (Build) Oap() error {
	return sh.RunV("go", "build", "-o", "bin/oap", "./cmd/oap")
}

// Web builds / serves / drift-checks the browser UI bundles under web/.
type Web mg.Namespace

// pnpmWeb runs pnpm with its working directory set to web/, NOT `pnpm -C web`
// from the repo root.
//
// The difference matters because corepack resolves which pnpm to run by reading
// the `packageManager` field of the package.json in its CURRENT DIRECTORY — it
// does not honour `-C`. Invoked from the repo root (which has no package.json)
// corepack falls back to the newest pnpm it knows about, ignoring the version
// web/package.json pins. That is not hypothetical: the fallback (pnpm 11.17.0)
// crashes on Node 22.x with ERR_VM_DYNAMIC_IMPORT_CALLBACK_MISSING, so every
// web target failed while the pin sat there looking correct.
//
// stream=true streams output and echoes the command (sh.RunV's behaviour), for
// the long-running dev servers; false keeps the quieter sh.Run behaviour.
func pnpmWeb(stream bool, args ...string) error {
	cmd := exec.Command("pnpm", args...)
	cmd.Dir = "web"
	cmd.Stderr = os.Stderr
	if stream {
		cmd.Stdout = os.Stdout
		cmd.Stdin = os.Stdin
		fmt.Fprintf(os.Stderr, "exec: pnpm %s (in web/)\n", strings.Join(args, " "))
	}
	return cmd.Run()
}

// nodeWeb runs node with its working directory set to web/, the same way
// pnpmWeb does, so a script's relative imports and its `typescript` import
// resolve against web/node_modules.
func nodeWeb(args ...string) error {
	cmd := exec.Command("node", args...)
	cmd.Dir = "web"
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	fmt.Fprintf(os.Stderr, "exec: node %s (in web/)\n", strings.Join(args, " "))
	return cmd.Run()
}

type UI mg.Namespace

// uiPages lists every first-party JSX page and the compiled view it produces
// (paths from the repo root). A page is listed, not globbed: a page that is
// not here is not compiled, and a compile that silently skipped one would
// ship a stale view. A second page is a second pair. Each compile also writes
// a page.view.sha256 sidecar beside the view — the hash of the source it
// compiled, which Go re-checks so a forgotten compile fails the unit gate
// instead of shipping the previous page.
var uiPages = [][2]string{
	{"pkg/platform/builderbundle/src/ui/page.tsx", "pkg/platform/builderbundle/src/ui/page.view.json"},
}

// Compile turns each page.tsx in uiPages into its committed page.view.json —
// the template compiler in web/packages/agentui/src/compile.ts, run through
// tools/compile-page.ts under Node's type stripping (no bundler and no extra
// runner dependency; Node 22 needs the flag, later majors do not). The output
// is committed, like dist, so Go's embed sees it; web:check fails when it
// drifts from its source. Go-side compilation for external TSX bundles is a
// recorded seam, not built.
//
// It installs first so it works from a clean clone: compile.ts imports
// `typescript` out of web/node_modules, which a fresh checkout does not have.
func (UI) Compile() error {
	if err := pnpmWeb(false, "install", "--frozen-lockfile"); err != nil {
		return err
	}
	for _, p := range uiPages {
		src, out := p[0], p[1]
		if err := nodeWeb("--experimental-strip-types", "--no-warnings=ExperimentalWarning",
			"packages/agentui/tools/compile-page.ts", "../"+src, "../"+out); err != nil {
			return fmt.Errorf("ui:compile %s: %w", src, err)
		}
	}
	return nil
}

// Build produces the committed pkg/web/webui/webassets/dist bundles. The page
// compiles — and web/ installs, which ui:compile does first — before the
// bundles build (spec §9).
func (Web) Build() error {
	if err := (UI{}).Compile(); err != nil {
		return err
	}
	return pnpmWeb(false, "build")
}

// Dev runs the Vite dev server (HMR) for `webd --web-dev`. It is a LONG-RUNNING
// process (it does not return to the prompt) — that is expected, not a hang.
// The banner prints immediately so the dev knows what's happening; vite's
// clearScreen is disabled (vite.config.ts) so the esbuild pre-bundle + the
// "ready" line both stay visible. sh.RunV streams vite's output + echoes the cmd.
func (Web) Dev() error {
	fmt.Fprintln(os.Stderr, "==> Vite dev server starting at http://localhost:5173 (HMR).")
	fmt.Fprintln(os.Stderr, "    First run pre-bundles deps with esbuild (a few seconds).")
	fmt.Fprintln(os.Stderr, "    This is a LONG-RUNNING server — leave it running; Ctrl-C to stop.")
	fmt.Fprintln(os.Stderr, "    In another terminal: webd --web-dev --cluster-kind=local   (or: oap init --local --develop)")
	return pnpmWeb(true, "dev")
}

// Annotdev runs the standalone annotation dev harness (Vite, port 5174) for
// iterating on the HTML-artifact annotator against fake artifacts + a fake shell.
// Standalone — it does NOT touch the Go server, sessions, or the production
// build (the harness has no app.json, so `mage web:build` never bundles it).
// Long-running; Ctrl-C to stop.
func (Web) Annotdev() error {
	fmt.Fprintln(os.Stderr, "==> Annotation harness starting at http://localhost:5174 (HMR, auto-opens browser).")
	fmt.Fprintln(os.Stderr, "    Standalone — no Go server needed. Ctrl-C to stop.")
	return pnpmWeb(true, "annotdev")
}

// Check rebuilds the bundles and fails if the committed dist drifted from source
// (the committed-asset drift guard; wire into CI).
func (Web) Check() error {
	if err := (Web{}).Build(); err != nil {
		return err
	}
	return sh.Run("git", "diff", "--exit-code", "--",
		"pkg/web/webui/webassets/dist", "pkg/platform/builderbundle/src/ui")
}

type Test mg.Namespace

// reapEnvtestOrphans kills envtest apiserver/etcd pairs left behind by earlier
// runs, before this one starts.
//
// envtest's Stop only runs when the test binary unwinds normally; a
// `go test -timeout` kill, a panic, or a Ctrl-C orphans the pair to PID 1, where
// it burns CPU until reboot. They accumulate invisibly, and once enough have
// piled up the poll-based integration/e2e tests start missing deadlines they
// clear easily in isolation — the "flaky, just re-run it isolated" symptom this
// repo has lived with. A machine mid-investigation had 21 orphans, the oldest 28
// days old; the suite could not go green until they were killed.
//
// Sweeping here (rather than only fixing the cleanup path) is what makes it
// self-healing: however a future run dies, the next one still starts clean.
// Best-effort — a sweep that cannot run must not stop the suite — but never
// silent: both the kill count and any failure are reported.
func reapEnvtestOrphans() {
	n, err := envtestreap.Reap()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not sweep envtest orphans (%v); a loaded machine may miss test deadlines\n", err)
		return
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "==> reaped %d orphaned envtest process(es) from earlier runs\n", n)
	}
}

// suiteLockWait bounds how long a suite waits for its turn. Long enough to
// outlast a full run of the other suite, short enough that one wedged run
// cannot freeze every other worktree indefinitely.
const suiteLockWait = 45 * time.Minute

// withSuiteLock runs fn holding the machine-wide suite lock, so that
// test:integration and test:e2e never run concurrently — including across the
// twenty-odd git worktrees this repo routinely has open.
//
// Both suites stand up envtest control planes and SpiceDB containers, which are
// machine-wide. Concurrent runs do not take turns; they starve each other, and
// the damage is not the lost time. The failures are timeout-shaped and land on
// whichever tests lost the race, so the failing SET MOVES between runs — three
// consecutive full e2e runs of one green tree failed on three disjoint sets,
// every one passing in isolation. A moving set of timeouts is indistinguishable
// from a real regression without re-running each test alone, which makes the
// ship gate's most expensive suite its least trustworthy signal.
//
// Set AP_TEST_NO_LOCK=1 to opt out, for CI where each run already owns its
// machine and waiting is pure cost.
func withSuiteLock(target string, fn func() error) error {
	path := suitelock.DefaultPath(gitCommonDir())
	holder := fmt.Sprintf("%s %s", filepath.Base(repoRoot()), target)

	// announced tracks the last progress message so the heartbeat is driven by
	// elapsed time rather than by which poll happens to land on a round second —
	// at a 250ms poll interval, several polls per second round to the same value.
	var announced time.Duration
	const heartbeat = 30 * time.Second

	release, err := suitelock.Acquire(suitelock.Options{
		Path:     path,
		Holder:   holder,
		Wait:     suiteLockWait,
		Disabled: os.Getenv("AP_TEST_NO_LOCK") != "",
		Notify: func(held string, waited time.Duration) {
			// Announce once on first contention, then every 30s, so a waiting run
			// is visibly waiting rather than apparently hung.
			if announced == 0 {
				announced = waited
				fmt.Fprintf(os.Stderr, "==> waiting for the test-suite lock, held by %s\n", held)
				fmt.Fprintf(os.Stderr, "    (set AP_TEST_NO_LOCK=1 to skip; lock file %s)\n", path)
				return
			}
			if waited-announced >= heartbeat {
				announced = waited
				fmt.Fprintf(os.Stderr, "==> still waiting (%s) for %s\n", waited.Round(time.Second), held)
			}
		},
	})
	if err != nil {
		return err
	}
	defer func() {
		if rerr := release(); rerr != nil {
			fmt.Fprintf(os.Stderr, "warning: releasing the test-suite lock: %v\n", rerr)
		}
	}()
	return fn()
}

// gitCommonDir returns the clone's shared git directory — the same path from
// every worktree, which is exactly the set of checkouts that contend. Falls
// back to ".git" so a failure here degrades to a per-worktree lock rather than
// breaking the build.
func gitCommonDir() string {
	out, err := sh.Output("git", "rev-parse", "--git-common-dir")
	if err != nil || strings.TrimSpace(out) == "" {
		return ".git"
	}
	dir := strings.TrimSpace(out)
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// repoRoot names this worktree, so a waiting run can tell WHICH checkout holds
// the lock. Falls back to the working directory.
func repoRoot() string {
	out, err := sh.Output("git", "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(out) == "" {
		wd, _ := os.Getwd()
		return wd
	}
	return strings.TrimSpace(out)
}

// testParallel returns the `-p` flag capping how many test binaries `go test`
// runs concurrently.
//
// go's default is -p=GOMAXPROCS, which over-subscribes these suites badly: a
// "package" here is not a goroutine. The integration tier boots an envtest
// apiserver + etcd per package, and e2e adds a SpiceDB container on top of
// that. On a 10-core machine the default fires ten of those at once, every one
// of them starves, and the tests that poll for convergence blow budgets
// calibrated in isolation — TestIdentityChoice_NonInteractive runs in 9s alone
// and exceeded its 45s poll at -p=10; the centerdot scenarios show the same
// 16s-vs-73s spread. Those read as flaky tests. They are a starved gate, and
// they are the origin of this repo's "just re-run it isolated" folklore.
//
// The cap is a fixed count rather than a fraction of GOMAXPROCS because the
// binding resource is per-package footprint (apiserver + etcd + container),
// not CPU — a 64-core box does not make a second etcd cheaper. AP_TEST_PARALLEL
// overrides it for a machine sized differently.
//
// NOTE on the history above: those starving-at--p=10 symptoms were real, but
// the cause was NOT resource pressure. InProcessRunnerFactory.Shutdown never
// cancelled its runner goroutines, so runners from FINISHED tests kept turning
// against dead apiservers for the rest of the run (fixed in 37a04422). With
// that gone, and test/e2e no longer one serial 437s package, the envtest tiers
// scale close to linearly with -p — hence envtestParallel below.
func testParallel(def int) string {
	if v := os.Getenv("AP_TEST_PARALLEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return "-p=" + strconv.Itoa(n)
		}
		// Never silently ignore a value the operator deliberately set.
		fmt.Fprintf(os.Stderr, "WARNING: ignoring AP_TEST_PARALLEL=%q (want a positive integer); using -p=%d\n", v, def)
	}
	return "-p=" + strconv.Itoa(def)
}

// envtestParallel is testParallel for the E2E tier, sized to the machine
// (test/testparallel.Derive) instead of a hardcoded 2. On a 10-core box that is
// -p=8, which took the tier from ~480s to 233s with 52 packages green.
//
// Used ONLY by Test.E2E, because e2e is the only tier measured at that width.
// Test.Integration stays at testParallel(4) and Test.Unit at testParallel(4)
// under -race: both are plausible candidates, but extending an unmeasured value
// to them is exactly what test/testparallel's Ceiling exists to discourage.
// Measure first, then widen. AP_TEST_PARALLEL overrides either way.
func envtestParallel() string {
	return testParallel(testparallel.Derive(runtime.NumCPU()))
}

func (Test) Unit() error {
	// ./internal/... and ./cmd/... are both included alongside ./pkg/... so the
	// untagged tests of every binary are part of the ship gate: the cluster
	// components live under ./internal/cmd/ (operator, runner, channelsd, webd,
	// authzd, extractord, ...) and the user-facing CLI under ./cmd/oap. BOTH
	// roots are required — dropping either one lets that half's regressions
	// (e.g. fixtures not migrated to a new memory-capability approval) rot
	// invisibly, since integration/e2e also only reach ./cmd/oap subsets.
	// Build-tagged tests (cmd/oap e2e, cmd/oap/internal/manifests integration)
	// are excluded here by their tags and run in the tiers below.
	// ./test/... holds the shared suite harnesses (envtest reaping, the SpiceDB
	// container fixture, the suite lock). Their own untagged tests belong in the
	// gate too, or a broken harness only surfaces as a confusing e2e failure.
	// ./toolkits/... is a repo-ROOT package (not under ./pkg/...), so — like
	// ./test/... — it must be named explicitly or it never runs. It holds the
	// builtin-toolkit authorization-conformance tests (per-subcommand stateImpact,
	// the gh api bypass guards, TestAll_HasCoreSet loading every shipped toolkit).
	// Omitting it is how a live ResolveResourceID regression sat green for weeks:
	// the gate never compiled the one package that was red.
	if err := sh.RunV("go", "test", "-race", "-count=1", testParallel(4), "./pkg/...", "./internal/...", "./cmd/...", "./test/...", "./toolkits/..."); err != nil {
		return err
	}
	return checkWebBundleFreshness()
}

// defaultWebCheckDiffBase is the git ref checkWebBundleFreshness diffs
// against by default, mirroring auditgen's DiffBase convention
// (AUDIT_DIFF_BASE, defaulting to "master").
const defaultWebCheckDiffBase = "master"

// checkWebBundleFreshness runs `mage web:check` when this branch's diff
// touches pkg/**/ui/** — the TypeScript source web/vite.config.ts's
// discoverEntries globs (`pkg/**/ui/**/app.json`) into the COMMITTED,
// go:embed'd pkg/web/webui/webassets/dist bundle. Without this,
// pkg/web/webui/webassets/embed_test.go only asserts the embedded files
// EXIST, never that they match source, so a branch can change the UI, pass
// all three ship-gate suites, and merge a console built from older
// TypeScript. That happened once on this series: seven commits changed the
// admin UI, every test stayed green, and only a manual read caught that
// install.yaml would have shipped a build with no Directory panel.
// `mage web:check` (rebuild + `git diff --exit-code` over dist) already
// catches it, but was wired into no `mage test:*` target, so the gate never
// ran it.
//
// A full rebuild-and-compare needs node/pnpm, which test:unit's environment
// is not guaranteed to have and which is too slow to pay on every run
// regardless of relevance — so this only fires when the diff actually
// touches pkg/**/ui/**, which is also what keeps a `go test`-only change from
// paying pnpm's install+build cost at all. Anyone who touched pkg/**/ui/**
// already needs node to have made that edit, so paying it here costs them
// nothing new.
//
// WEB_CHECK_DIFF_BASE overrides the default "master" base. A diff-computation
// failure (no such ref, a shallow clone missing history) is logged and SKIPS
// the check rather than failing test:unit outright — test:unit is the fast
// gate every developer runs constantly (see (Test).Postgres's own doc), and a
// clone-shape problem must not turn the fast tier flaky. Per CLAUDE.md's
// no-silent-errors rule the skip is logged, not silent; it just does not
// block.
func checkWebBundleFreshness() error {
	base := os.Getenv("WEB_CHECK_DIFF_BASE")
	if base == "" {
		base = defaultWebCheckDiffBase
	}
	changed, err := changedUISourceFiles(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "==> test:unit: could not compute the UI diff against %q, skipping the "+
			"stale-web-bundle check: %v\n", base, err)
		return nil
	}
	if len(changed) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "==> test:unit: %d changed file(s) under pkg/**/ui/** vs %s — running "+
		"mage web:check to confirm the committed dist bundle still matches:\n", len(changed), base)
	for _, f := range changed {
		fmt.Fprintf(os.Stderr, "      %s\n", f)
	}
	if err := (Web{}).Check(); err != nil {
		return fmt.Errorf("web UI source changed vs %s but pkg/web/webui/webassets/dist does not match — "+
			"run `mage web:build` and commit the result: %w", base, err)
	}
	return nil
}

// changedUISourceFiles returns the pkg/**/ui/** paths (any path under pkg/
// with a directory component literally named "ui") that changed in the
// `base...HEAD` merge-base diff — the same range claudeexec.Diff uses for
// auditgen, so a developer who already knows that convention reads this one
// the same way.
//
// --end-of-options guards the same way claudeexec.Diff's does: base is
// env-controlled (WEB_CHECK_DIFF_BASE) and interpolated into a revision
// argument, so a value beginning with "-" must be rejected as a bad revision
// rather than accepted as a git flag.
func changedUISourceFiles(base string) ([]string, error) {
	out, err := sh.Output("git", "diff", "--name-only", "--end-of-options", base+"...HEAD")
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only %s...HEAD: %w", base, err)
	}
	var changed []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "pkg/") {
			continue
		}
		for _, seg := range strings.Split(line, "/") {
			if seg == "ui" {
				changed = append(changed, line)
				break
			}
		}
	}
	return changed, nil
}

func (Test) Integration() error {
	return withSuiteLock("test:integration", testIntegration)
}

// Postgres runs the tests that need a real PostgreSQL server —
// pkg/memory/postgres, the durable memory backend every remote install runs.
//
// Those tests are UNTAGGED, so `mage test:unit` already compiles and runs them;
// without a database they skip, which is how ~20 tests covering the production
// backend stayed dark. This target supplies one by starting a container
// (test/testpostgres), and exists as a separate target — rather than teaching
// test:unit to start it — because test:unit is the fast gate every developer
// runs constantly, and making it require a Docker daemon would fail it on a
// laptop with Docker stopped, for every package in the repo.
//
// The same tests also run in test:integration, which sets the same env: that
// tier already requires Docker for envtest and SpiceDB, so joining it there
// puts the backend in the ship gate at no new prerequisite.
//
// POSTGRES_URI, if set, wins over the container — point it at an existing
// database to skip container startup.
func (Test) Postgres() error {
	return sh.RunWithV(postgresTestEnv(), "go", "test", "-race", "-count=1",
		"./pkg/memory/postgres/...")
}

// postgresTestEnv opts test/testpostgres in to starting a container. Kept in one
// place so test:postgres and test:integration cannot drift apart.
func postgresTestEnv() map[string]string {
	return map[string]string{testpostgres.EnvMode: testpostgres.ModeDocker}
}

func testIntegration() error {
	envs := map[string]string{
		"KUBEBUILDER_ASSETS": mustEnvtestAssets(),
	}
	for k, v := range postgresTestEnv() {
		envs[k] = v
	}
	reapEnvtestOrphans()
	// ./pkg/... is the bulk of the envtest suite. pkg/platform/manifests is
	// added explicitly so the RBAC sufficiency+minimality harness
	// (TestRBACSufficiency) runs under a real RBAC-enforcing apiserver in CI —
	// it proves every component role grants exactly what its code needs and no
	// more, catching an over- or under-tightened cluster role here instead of
	// at deploy time.
	return withSpicedbSweep("test:integration", envs, func(envs map[string]string) error {
		// -timeout is PER PACKAGE and defaults to 10m. That is too tight for the
		// heaviest envtest packages under -race on a CI runner: pkg/platform/oap/install
		// stands up install/uninstall control planes and clocks ~9m there, so
		// normal variance tips it over 10m and fails the whole suite. e2e already
		// sets an explicit budget for the same reason; match it here.
		return sh.RunWithV(envs, "go", "test", "-race", "-count=1", "-tags=integration",
			"-timeout=30m", testParallel(4), "./pkg/...", "./test/...", "./toolkits/...")
	})
}

// Bronze runs the bronzethread suite: whole-session tests driven by an
// AUTHORED transcript, with everything except the LLM and stateful tool
// outputs running for real.
//
// Its own tier because it answers a different question than e2e. An e2e test
// hand-writes LLM rules, and a rule that never fires is silent — which is how
// the first plan-gate whole-session test silently exercised nothing. A
// bronzethread bundle is POSITIONAL with a per-step divergence check, so an
// unfired step is an error by construction.
//
// The bundle format is what `steelthread:capture` will emit, so a captured
// session will drop into this same driver unchanged.
func (Test) Bronze() error {
	envs := map[string]string{
		"KUBEBUILDER_ASSETS": mustEnvtestAssets(),
	}
	// threadrun holds the replay driver (Load/Run) bronze actually runs on —
	// lifted there so steelthread can share it — including the driver's own
	// unit tests (TestSubstituteMintedIDs*). Without it here, `test:bronze` as
	// a standalone target would silently stop exercising the driver's unit
	// coverage even though the driver is exactly what this target exists to
	// exercise; test:e2e's ./test/e2e/... glob still catches it, but that is
	// the whole-suite gate, not this tier's own signal.
	return withSpicedbSweep("test:bronze", envs, func(envs map[string]string) error {
		return sh.RunWithV(envs, "go", "test", "-count=1", "-tags=e2e",
			"-timeout=10m", "-v", "./test/e2e/bronzethread/...", "./test/e2e/threadrun/...")
	})
}

// Steel runs the steelthread suite: whole-session tests driven by a transcript
// CAPTURED from a real session rather than authored.
//
// Same driver and same divergence contract as Bronze — the difference is
// evidentiary. A green bronze bundle proves the system handles an interaction;
// a green steel bundle also shows a real model produced it, at least once.
//
// Skips cleanly with no bundles: captures come from a live cluster, so a fresh
// clone legitimately has none.
func (Test) Steel() error {
	envs := map[string]string{
		"KUBEBUILDER_ASSETS": mustEnvtestAssets(),
	}
	return withSpicedbSweep("test:steel", envs, func(envs map[string]string) error {
		return sh.RunWithV(envs, "go", "test", "-count=1", "-tags=e2e",
			"-timeout=10m", "-v", "./test/e2e/steelthread/...")
	})
}

// E2E runs the in-process end-to-end test framework against envtest
// + a real SpiceDB container (via test/testspicedb). Requires Docker
// running locally; SpiceDB tests skip cleanly if Docker is absent.
//
// Covers every e2e-tagged package: the in-process harness (test/e2e/...),
// the channelsd pipeline e2e (pkg/channels/channelsd/e2e/...), authzd's
// real-NATS round-trip tests (internal/cmd/authzd/...), and the `oap init
// --local` e2e tests in cmd/oap. cmd/oap is package main (unit + e2e mixed),
// so we filter to the e2e tests by name — the whole package still compiles
// under -tags=e2e, so e2e bit-rot is caught. Keep new cmd/oap e2e tests
// under the TestApInitLocal* prefix, or extend the -run filter below.
func (Test) E2E() error {
	return withSuiteLock("test:e2e", testE2E)
}

func testE2E() error {
	envs := map[string]string{
		"KUBEBUILDER_ASSETS": mustEnvtestAssets(),
	}
	reapEnvtestOrphans()
	// Dedicated e2e-only packages: run the whole package.
	//
	// -timeout is PER PACKAGE, and pkg/e2e (the root package, not its
	// subdirectories) has outgrown 10m: it boots a full envtest + SpiceDB
	// harness per test and now holds enough of them to exhaust that budget on an
	// idle machine. The failure does not name a slow test — it panics with
	// whichever one happened to be running when the alarm fired, which reads as
	// a hang in an innocent test and got attributed to load more than once.
	// Raise the ceiling rather than keep re-diagnosing it; a genuinely stuck
	// test still trips this, just later.
	// -p 2 caps how many PACKAGES run at once, and it is the difference between
	// a gate you can read and one you cannot.
	//
	// go test defaults -p to GOMAXPROCS — 10 here — and this suite is 45
	// packages, nearly all of which boot their own envtest control plane
	// (apiserver + etcd) and a SpiceDB container. Ten Kubernetes control planes
	// at once starves them all, so Expect* calls blow their deadlines and the
	// run reports failures that have nothing to do with the code: every one
	// passes in isolation, and the FAILING SET MOVES between runs on an
	// unchanged tree. The suite was contending with itself, not with other work
	// on the machine — it did this starting from an idle box.
	//
	// A moving set of deadline failures is unreadable as a signal: it trains
	// everyone to re-run rather than investigate, which is exactly how a real
	// regression gets waved through. Slower and trustworthy beats faster and
	// ignored.
	// cmd/authzd is in this list because its e2e-tagged tests were NOT being
	// run by the gate at all. coldstart_nats_e2e_test.go is the only test that
	// drives the metaagent NATS decode end to end, and it sat behind a build
	// tag that `mage test:unit` never compiles and this target never selected —
	// so a change to that decode path could pass all three suites untouched.
	// Found when a struct refactor of the worker's entry point needed exactly
	// that test to verify the field mapping, and nothing in the gate ran it.
	return withSpicedbSweep("test:e2e", envs, func(envs map[string]string) error {
		err := sh.RunWithV(envs, "go", "test", "-count=1", "-tags=e2e",
			"-timeout=25m", envtestParallel(), "./test/e2e/...", "./pkg/channels/channelsd/e2e/...", "./internal/cmd/authzd/...")
		if err == nil {
			// cmd/oap: run only the e2e-tagged tests (package compiles in full).
			err = sh.RunWithV(envs, "go", "test", "-count=1", "-tags=e2e",
				"-timeout=10m", envtestParallel(), "-run", "TestApInitLocal", "./cmd/oap/")
		}
		return err
	})
}

func (Test) Toolspec() error {
	return sh.RunV("go", "test", "-race", "-count=1", "./pkg/tools/toolspec/...", "./cmd/oap/internal/toolscmd/...")
}

func (Test) ToolspecFuzz() error {
	return sh.RunV("go", "test", "-run=^$", "-fuzz=FuzzParseDoesNotPanic",
		"-fuzztime=30s", "./pkg/tools/toolspec/parser/...")
}

func (Test) ToolspecLive() error {
	if os.Getenv("GEMINI_API_KEY") == "" {
		return fmt.Errorf("GEMINI_API_KEY must be set for test:toolspec-live")
	}
	return sh.RunV("go", "test", "-tags=gemini_live", "-count=1",
		"./pkg/tools/toolspec/llm/gemini/...")
}

// Smoke boots a kind cluster, builds + loads images, runs oap init,
// applies a multi-bundle AgentSession fixture, and asserts cross-pod
// /workspace visibility. Skips silently if `kind` is not on PATH. Honors
// KEEP_SMOKE_CLUSTER=1 to skip teardown for debugging.
func (Test) Smoke() error {
	const clusterName = "ap-smoke"
	kctx, teardown, ok, err := bootPlatformOnKind(clusterName)
	if !ok {
		fmt.Println("test:smoke: skipping — `kind` not on PATH")
		return nil
	}
	defer teardown()
	if err != nil {
		return err
	}

	// Fail fast on a broken platform boot before the feature assertions: every
	// system Deployment (operator, channelsd, webd, authzd, extractord,
	// spicedb) must reach Available. This is the only layer that runs a real
	// kubelet, so it is where image-USER, missing-Secret, and
	// token-mount-race regressions surface — the envtest suites
	// (apiserver+etcd only) cannot see them.
	if err := waitAllSystemDeployments(kctx); err != nil {
		return err
	}

	// Toolchain overlay: build + load the go toolchain image, apply a class
	// that names it, and prove `go build` runs inside the sandbox. This is the
	// only layer with a real kubelet, so it is the only place the disk-backed
	// GOCACHE, the glibc pin, and the `cp -R` permission model are actually
	// exercised. `oap build all` deliberately does NOT build toolchain images
	// (they're opt-in payloads, not always-needed service images), so build +
	// load it explicitly here.
	if err := sh.RunV("./bin/oap", "build", "toolchain-go", "--no-load"); err != nil {
		return fmt.Errorf("oap build toolchain-go: %w", err)
	}
	if err := sh.RunV("kind", "load", "docker-image", "--name", clusterName, "ap-toolchain-go:dev"); err != nil {
		return fmt.Errorf("kind load ap-toolchain-go:dev: %w", err)
	}
	if err := smokeToolchain(kctx); err != nil {
		return err
	}

	// Build + load the Go echo-MCP sidecar image used by the SidecarToolbox
	// smoke step below. Build context is the repo root so go.mod resolves.
	const echoImage = "echo-mcp-smoke:dev"
	if err := sh.RunV("docker", "build",
		"-f", "magefiles/smoke/echo-mcp/Dockerfile",
		"-t", echoImage, "."); err != nil {
		return fmt.Errorf("docker build echo-mcp: %w", err)
	}
	if err := sh.RunV("kind", "load", "docker-image", "--name", clusterName, echoImage); err != nil {
		return fmt.Errorf("kind load %s: %w", echoImage, err)
	}

	// SidecarToolbox smoke: inject a REAL echo-MCP sidecar container into an
	// AgentSession runner Pod and prove the in-pod loopback MCP path. Runs
	// first because it is self-contained (own fixture, own session) and does
	// not depend on the multi-bundle workspace assertions below.
	if err := smokeSidecar(kctx); err != nil {
		return err
	}

	// Secret-gated separate-pod sidecar smoke: prove the operator detects a
	// secret-gated SidecarToolbox (secretInputs present), creates a SEPARATE
	// per-session pod (not a container in the runner), and injects the
	// secret-output value into that pod as the SMOKE_SECRET env var.
	if err := smokeSecretGatedSidecar(kctx); err != nil {
		return err
	}

	// Apply fixture.
	if err := sh.RunV("kubectl", "--context", kctx, "apply", "-f", "magefiles/smoke/multi_bundle_fixture.yaml"); err != nil {
		return fmt.Errorf("apply fixture: %w", err)
	}

	// Poll for PVC Bound.
	deadline := time.Now().Add(90 * time.Second)
	var pvcBound bool
	for time.Now().Before(deadline) {
		out, _ := sh.Output("kubectl", "--context", kctx, "-n", "default", "get", "pvc", "smoke-workspace", "-o=jsonpath={.status.phase}")
		if out == "Bound" {
			pvcBound = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !pvcBound {
		return fmt.Errorf("workspace PVC not Bound within 90s")
	}

	// Wait for both bundle pods Ready.
	for _, sess := range []string{"smoke-a", "smoke-b"} {
		podName := sess + "-pod"
		if err := sh.RunV("kubectl", "--context", kctx, "-n", "default",
			"wait", "--for=condition=ready", "pod/"+podName, "--timeout=90s"); err != nil {
			return fmt.Errorf("wait for pod %s: %w", podName, err)
		}
	}

	// Write a probe file in pod A.
	if err := sh.RunV("kubectl", "--context", kctx, "-n", "default", "exec", "smoke-a-pod", "--",
		"sh", "-c", "echo hello > /workspace/probe"); err != nil {
		return fmt.Errorf("write probe in pod a: %w", err)
	}

	// Read it from pod B.
	got, err := sh.Output("kubectl", "--context", kctx, "-n", "default", "exec", "smoke-b-pod", "--",
		"cat", "/workspace/probe")
	if err != nil {
		return fmt.Errorf("read probe in pod b: %w", err)
	}
	got = strings.TrimSpace(got)
	if got != "hello" {
		return fmt.Errorf("cross-pod workspace failed: pod a wrote 'hello', pod b read %q", got)
	}

	fmt.Println("test:smoke: PASS — cross-pod workspace visible")
	return nil
}

// SmokeInstall is the lightweight platform-boot check: it boots its OWN kind
// cluster, installs the full platform with `oap init`, and asserts that every
// system Deployment reaches Available — nothing more. It is the fast "does a
// fresh install come up clean" gate, separate from (and far quicker than) the
// feature-exercising Test.Smoke. Skips silently if `kind` is not on PATH;
// honors KEEP_SMOKE_CLUSTER=1 to retain the cluster for debugging.
func (Test) SmokeInstall() error {
	kctx, teardown, ok, err := bootPlatformOnKind("ap-smoke-install")
	if !ok {
		fmt.Println("test:smokeinstall: skipping — `kind` not on PATH")
		return nil
	}
	defer teardown()
	if err != nil {
		return err
	}
	if err := waitAllSystemDeployments(kctx); err != nil {
		return err
	}
	fmt.Println("test:smokeinstall: PASS — all system Deployments Available")
	return nil
}

// allServiceImages are the images `oap init` deploys that must be present
// in-cluster for the platform to boot. The list is load-bearing: omitting one
// (webd/authzd were both missing here originally) lets that component sit in
// ImagePullBackOff while the smoke passes around it.
var allServiceImages = []string{
	"spicebox-operator:dev",
	"spicebox-sandbox:dev",
	"agentprimitives-runner:dev",
	"agentprimitives-channelsd:dev",
	"agentprimitives-webd:dev",
	"agentprimitives-authzd:dev",
	"agentprimitives-extractord:dev",
}

// bootPlatformOnKind creates (if missing) a kind cluster named clusterName,
// builds bin/oap + all images, loads the service images, and runs `oap init`
// against it. It returns the kube-context, a teardown func (a no-op when
// KEEP_SMOKE_CLUSTER=1), and ok=false (with nil err) when `kind` is not on PATH
// so callers can skip cleanly. Shared by Test.Smoke and Test.SmokeInstall so
// both exercise the identical install path.
func bootPlatformOnKind(clusterName string) (kctx string, teardown func(), ok bool, err error) {
	teardown = func() {}
	if _, e := exec.LookPath("kind"); e != nil {
		return "", teardown, false, nil
	}

	existing, _ := sh.Output("kind", "get", "clusters")
	if !strings.Contains(existing, clusterName) {
		if e := sh.RunV("kind", "create", "cluster", "--name", clusterName); e != nil {
			return "", teardown, true, fmt.Errorf("kind create: %w", e)
		}
	}
	if os.Getenv("KEEP_SMOKE_CLUSTER") != "1" {
		teardown = func() { _ = sh.Run("kind", "delete", "cluster", "--name", clusterName) }
	}

	// Build bin/oap (host-side CLI used to drive the rest of the boot).
	if e := (Build{}.Oap()); e != nil {
		return "", teardown, true, e
	}
	// Build all images via `oap build all --no-load`, then load the service
	// images ourselves so we don't depend on oap's loader auto-detecting this
	// specific cluster.
	if e := sh.RunV("./bin/oap", "build", "all", "--no-load"); e != nil {
		return "", teardown, true, fmt.Errorf("oap build all: %w", e)
	}
	for _, img := range allServiceImages {
		if e := sh.RunV("kind", "load", "docker-image", "--name", clusterName, img); e != nil {
			return "", teardown, true, fmt.Errorf("kind load %s: %w", img, e)
		}
	}

	kctx = "kind-" + clusterName
	if e := sh.RunV("./bin/oap", "init", "--context", kctx); e != nil {
		return "", teardown, true, fmt.Errorf("oap init: %w", e)
	}
	return kctx, teardown, true, nil
}

// waitAllSystemDeployments blocks until every Deployment in
// agentprimitives-system reports Available — the real-kubelet platform-boot
// gate. On timeout it dumps pod state so a CI failure is diagnosable without a
// re-run. (nats is a StatefulSet and is covered transitively: the Deployments
// that depend on it cannot become Available until it serves.)
func waitAllSystemDeployments(kctx string) error {
	if err := sh.RunV("kubectl", "--context", kctx, "-n", "agentprimitives-system",
		"wait", "--for=condition=Available", "deployment", "--all", "--timeout=180s"); err != nil {
		pods, _ := sh.Output("kubectl", "--context", kctx, "-n", "agentprimitives-system", "get", "pods")
		return fmt.Errorf("system Deployments did not all become Available:\n%s\n%w", pods, err)
	}
	return nil
}

// smokeSidecar applies the SidecarToolbox fixture and asserts the end-to-end
// real-sidecar injection + in-pod loopback MCP path:
//
//  1. The SidecarToolbox CR reaches Valid=True. This is load-bearing: the CR
//     only goes Valid=True after the controller's one-shot probe Pod runs the
//     echo image, calls tools/list over the pod network, and confirms the
//     declared `echo` tool is present (Reachable=True). The probe Pod requires
//     the echo image to be kind-loadable in-cluster — which Test.Smoke does
//     before calling this. AgentClass validation then gates on Valid=True.
//  2. The runner Pod (<session>-runner) contains a container named
//     "sidecar-<ref>" — proving injection.
//  3. That sidecar container reaches Ready. Its startupProbe hits /healthz on
//     the in-pod loopback (127.0.0.1:<allocated-port>), so Ready proves the
//     loopback echo-MCP server actually serves — without depending on shell
//     tools (wget/curl) being present in the runner image.
func smokeSidecar(kctx string) error {
	const (
		ns          = "default"
		toolboxName = "sb-echo"
		sessionName = "sidecar-smoke"
		podName     = sessionName + "-runner"
		container   = "sidecar-" + toolboxName
	)

	if err := sh.RunV("kubectl", "--context", kctx, "apply",
		"-f", "magefiles/smoke/sidecar_fixture.yaml"); err != nil {
		return fmt.Errorf("apply sidecar fixture: %w", err)
	}

	// Wait for the SidecarToolbox to go Valid=True. The probe Pod boot +
	// tools/list round-trip dominates here, so allow a generous window.
	if err := sh.RunV("kubectl", "--context", kctx, "-n", ns, "wait",
		"--for=condition=Valid", "sidecartoolbox/"+toolboxName, "--timeout=120s"); err != nil {
		// Surface the CR status to aid diagnosis (kubectl wait only prints the
		// timeout, not why the condition never flipped).
		desc, _ := sh.Output("kubectl", "--context", kctx, "-n", ns,
			"get", "sidecartoolbox/"+toolboxName, "-o=yaml")
		return fmt.Errorf("SidecarToolbox %q not Valid=True within 120s: %w\n%s", toolboxName, err, desc)
	}

	// Wait for the runner Pod (and thus the injected sidecar container) Ready.
	// Pod-readiness is gated on the sidecar container's startupProbe (/healthz).
	if err := sh.RunV("kubectl", "--context", kctx, "-n", ns, "wait",
		"--for=condition=ready", "pod/"+podName, "--timeout=120s"); err != nil {
		desc, _ := sh.Output("kubectl", "--context", kctx, "-n", ns,
			"get", "pod/"+podName, "-o=yaml")
		return fmt.Errorf("runner pod %q not Ready within 120s: %w\n%s", podName, err, desc)
	}

	// Assert the injected sidecar container is present in the Pod spec.
	names, err := sh.Output("kubectl", "--context", kctx, "-n", ns,
		"get", "pod/"+podName, "-o=jsonpath={.spec.containers[*].name}")
	if err != nil {
		return fmt.Errorf("get pod container names: %w", err)
	}
	if !strings.Contains(names, container) {
		return fmt.Errorf("sidecar container %q not injected; pod containers: %q", container, names)
	}

	// Assert the sidecar container specifically reached Ready (its startupProbe
	// validates the in-pod loopback /healthz). jsonpath selects the container
	// status entry whose name matches and reports its ready flag.
	jp := fmt.Sprintf("-o=jsonpath={.status.containerStatuses[?(@.name==%q)].ready}", container)
	ready, err := sh.Output("kubectl", "--context", kctx, "-n", ns,
		"get", "pod/"+podName, jp)
	if err != nil {
		return fmt.Errorf("get sidecar container ready status: %w", err)
	}
	if strings.TrimSpace(ready) != "true" {
		return fmt.Errorf("sidecar container %q not Ready (loopback /healthz never passed); ready=%q", container, ready)
	}

	fmt.Printf("test:smoke: PASS — sidecar %q injected into %s and loopback echo-MCP Ready\n", container, podName)
	return nil
}

// smokeToolchain applies the toolchain fixture (a SpiceboxClass naming the go
// toolchain + a raw SpiceboxSession bound to it — no AgentClass/AgentSession
// layer, since this execs the sandbox pod directly rather than through a
// ToolCall) and proves `go build` actually runs inside it:
//
//  1. Wait for the SpiceboxSession to go Ready=True. That requires the
//     SpiceboxSession controller to resolve+freeze spec.toolchains, the pod
//     builder to compose the toolchain init container + read-only overlay +
//     disk-backed /var/ap-cache, and the Pod to reach Running+Ready.
//  2. Wait for the sandbox Pod itself Ready (belt-and-suspenders: Session
//     Ready already mirrors Pod Ready, but asserting on the Pod directly
//     keeps the failure signal precise if that ever changes).
//  3. kubectl exec a script that builds a trivial module inside
//     /var/ap-cache (the only writable disk-backed area — /tmp and /work are
//     50Mi/100Mi tmpfs, both charged against the pod memory limit) and runs
//     `go version` + `gopls version`.
func smokeToolchain(kctx string) error {
	const (
		ns          = "default"
		sessionName = "tc-smoke"
		podName     = sessionName + "-pod"
	)

	if err := sh.RunV("kubectl", "--context", kctx, "apply",
		"-f", "magefiles/smoke/toolchain-class.yaml"); err != nil {
		return fmt.Errorf("apply toolchain smoke fixture: %w", err)
	}

	// Wait for the SpiceboxSession to go Ready=True. The toolchain init
	// container copying the ~350MB go payload into the shared emptyDir
	// dominates here, so allow a generous window.
	if err := sh.RunV("kubectl", "--context", kctx, "-n", ns, "wait",
		"--for=condition=Ready", "spiceboxsession/"+sessionName, "--timeout=180s"); err != nil {
		desc, _ := sh.Output("kubectl", "--context", kctx, "-n", ns,
			"get", "spiceboxsession/"+sessionName, "-o=yaml")
		return fmt.Errorf("SpiceboxSession %q not Ready within 180s: %w\n%s", sessionName, err, desc)
	}

	if err := sh.RunV("kubectl", "--context", kctx, "-n", ns, "wait",
		"--for=condition=ready", "pod/"+podName, "--timeout=180s"); err != nil {
		desc, _ := sh.Output("kubectl", "--context", kctx, "-n", ns,
			"get", "pod/"+podName, "-o=yaml")
		return fmt.Errorf("toolchain sandbox pod %q not Ready within 180s: %w\n%s", podName, err, desc)
	}

	// TMPDIR + GOCACHE must be disk-backed; /tmp is a 50Mi tmpfs. Building in
	// /var/ap-cache proves both the mount and the writable cache.
	const goBuild = `set -e
mkdir -p /var/ap-cache/smoke && cd /var/ap-cache/smoke
printf 'package main\nfunc main() {}\n' > main.go
go mod init smoke >/dev/null
go build ./...
go version
gopls version`
	if err := sh.RunV("kubectl", "--context", kctx, "-n", ns, "exec", podName,
		"-c", "sandbox", "--", "/bin/sh", "-c", goBuild); err != nil {
		return fmt.Errorf("go build inside toolchain sandbox: %w", err)
	}

	fmt.Println("test:smoke: PASS — go build succeeded inside the toolchain sandbox")
	return nil
}

// smokeSecretGatedSidecar proves the secret-gated separate-pod SidecarToolbox
// control-plane path in the kind cluster:
//
//  1. Pre-seed the per-session secret-output Secret with a known value. The
//     real producer flow (an agent tool-call) is not driven here — running a
//     live LLM turn in a kind cluster requires an API key + external network
//     access, which are outside the scope of this self-contained smoke. The
//     control-plane path the operator takes is identical regardless of whether
//     the Secret arrived from a live producer or a pre-seed.
//  2. Apply the secret-gated sidecar fixture (SidecarToolbox with secretInputs,
//     AgentClass, AgentSession). The operator sees the pre-seeded Secret and
//     resolves the sidecar as RunMode=separate-pod.
//  3. Wait for the SidecarToolbox to go Valid=True (probe pod runs, confirms
//     whoami + echo are served).
//  4. Wait for the separate sidecar Pod (<session>-sidecar-<ref>) to reach
//     Ready. Its startupProbe hits /healthz — Ready proves the echo-MCP
//     server started successfully inside the separate pod.
//  5. Assert the separate pod is NOT a container in the runner pod — it must
//     NOT appear in the runner pod's container list.
//  6. Assert the separate pod's container spec carries SMOKE_SECRET as an env
//     var sourced via secretKeyRef from the pre-seeded per-session Secret. This
//     confirms the operator wired the secretInput binding correctly.
//  7. Assert the per-session secret-output Secret holds the pre-seeded value
//     under the expected key — and that the sha256 fingerprint of that value
//     matches what the echo-MCP whoami tool would compute.
func smokeSecretGatedSidecar(kctx string) error {
	const (
		ns          = "default"
		sessionName = "sg-sidecar-smoke"
		toolboxName = "sg-echo"
		// SidecarPodName convention: <session>-sidecar-<ref>
		sidecarPodName = sessionName + "-sidecar-" + toolboxName
		// SecretOutputSecretName convention: <session>-secret-outputs
		soSecretName = sessionName + "-secret-outputs"
		// The secret-output key bound by secretInputs[0].from
		soKey = "kubeconfig"
		// The env var name the operator injects (secretInputs[0].name)
		smokeSecretEnvVar = "SMOKE_SECRET"
		// The runner pod name for the negative containment assertion
		runnerPodName = sessionName + "-runner"
	)

	// The known secret value we pre-seed. A synthetic kubeconfig-shaped string
	// using fake names — no real hosts, no real tokens. The smoke asserts the
	// operator wired the env var from this value's Secret, not the value itself.
	const secretValue = "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://cluster.smoke.invalid:6443\n  name: smoke\nusers:\n- name: smoke-admin\n  user:\n    token: SMOKE-SENTINEL-d34db33f\n"

	// Step 1: pre-seed the per-session secret-output Secret with the known
	// value BEFORE applying the AgentSession fixture. The operator reconciler
	// checks for this Secret's existence when it resolves a secret-gated sidecar;
	// by writing it first, we avoid a race between fixture apply and reconcile.
	//
	// Pass the YAML on stdin (kubectl apply -f -). exec.Command is used directly
	// rather than sh.RunV because mage's sh package does not support stdin piping.
	soSecretYAML := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  %s: %q
`, soSecretName, ns, soKey, secretValue)
	applyCmd := exec.Command("kubectl", "--context", kctx, "apply", "-f", "-")
	applyCmd.Stdin = strings.NewReader(soSecretYAML)
	applyCmd.Stdout = os.Stdout
	applyCmd.Stderr = os.Stderr
	if err := applyCmd.Run(); err != nil {
		return fmt.Errorf("pre-seed secret-output Secret %q: %w", soSecretName, err)
	}

	// Step 2: apply the secret-gated sidecar fixture (SidecarToolbox,
	// AgentClass, AgentSession).
	if err := sh.RunV("kubectl", "--context", kctx, "apply",
		"-f", "magefiles/smoke/secret_gated_sidecar_fixture.yaml"); err != nil {
		return fmt.Errorf("apply secret-gated sidecar fixture: %w", err)
	}

	// Step 3: wait for the SidecarToolbox to go Valid=True. The probe pod
	// boots, calls tools/list, and confirms whoami + echo are served.
	if err := sh.RunV("kubectl", "--context", kctx, "-n", ns, "wait",
		"--for=condition=Valid", "sidecartoolbox/"+toolboxName, "--timeout=120s"); err != nil {
		desc, _ := sh.Output("kubectl", "--context", kctx, "-n", ns,
			"get", "sidecartoolbox/"+toolboxName, "-o=yaml")
		return fmt.Errorf("SidecarToolbox %q not Valid=True within 120s: %w\n%s", toolboxName, err, desc)
	}

	// Step 4: wait for the SEPARATE sidecar pod to be Ready. The pod name
	// follows the SidecarPodName convention: <session>-sidecar-<ref>.
	if err := sh.RunV("kubectl", "--context", kctx, "-n", ns, "wait",
		"--for=condition=ready", "pod/"+sidecarPodName, "--timeout=120s"); err != nil {
		desc, _ := sh.Output("kubectl", "--context", kctx, "-n", ns,
			"get", "pod/"+sidecarPodName, "-o=yaml")
		return fmt.Errorf("separate sidecar pod %q not Ready within 120s: %w\n%s", sidecarPodName, err, desc)
	}

	// Step 5: assert the sidecar is a SEPARATE pod, not a container in the
	// runner pod. The runner pod may not exist yet (the session is
	// AwaitingSecret, and the runner only boots once the sidecar is Ready), but
	// either way the sidecar pod must be a top-level pod, not a runner container.
	runnerContainers, _ := sh.Output("kubectl", "--context", kctx, "-n", ns,
		"get", "pod/"+runnerPodName, "-o=jsonpath={.spec.containers[*].name}")
	if strings.Contains(runnerContainers, "sidecar-"+toolboxName) {
		return fmt.Errorf("sidecar %q must be a SEPARATE pod, not a container in the runner pod %q",
			toolboxName, runnerPodName)
	}

	// Step 6: assert the separate pod's container carries SMOKE_SECRET wired
	// via secretKeyRef to the per-session secret-output Secret. jsonpath
	// extracts the env entry's name and secretKeyRef fields; we inspect them
	// separately to produce a clear error message when either is wrong.
	//
	// The sidecar pod has exactly one container (the echo-MCP image). We
	// query the env var list and match the SMOKE_SECRET entry.
	envJSON, err := sh.Output("kubectl", "--context", kctx, "-n", ns,
		"get", "pod/"+sidecarPodName,
		"-o=jsonpath={.spec.containers[0].env}")
	if err != nil {
		return fmt.Errorf("get sidecar pod env: %w", err)
	}
	if !strings.Contains(envJSON, smokeSecretEnvVar) {
		return fmt.Errorf("sidecar pod %q container env does not carry %q; env=%q",
			sidecarPodName, smokeSecretEnvVar, envJSON)
	}
	if !strings.Contains(envJSON, soSecretName) {
		return fmt.Errorf("sidecar pod %q env var %q not sourced from Secret %q; env=%q",
			sidecarPodName, smokeSecretEnvVar, soSecretName, envJSON)
	}
	if !strings.Contains(envJSON, soKey) {
		return fmt.Errorf("sidecar pod %q env var %q not sourced from key %q of Secret %q; env=%q",
			sidecarPodName, smokeSecretEnvVar, soKey, soSecretName, envJSON)
	}

	// Step 7: assert the per-session secret-output Secret holds the pre-seeded
	// value under the expected key. kubectl jsonpath returns the base64-encoded
	// form for Secret.data fields; a non-empty response confirms the key exists.
	// We do NOT decode or print the value — asserting key existence is sufficient
	// to prove the pre-seed landed; the separate pod's SMOKE_SECRET env wiring
	// (Step 6) ties the operator to this exact Secret+key.
	soData, err := sh.Output("kubectl", "--context", kctx, "-n", ns,
		"get", "secret/"+soSecretName, "-o=jsonpath={.data."+soKey+"}")
	if err != nil || soData == "" {
		return fmt.Errorf("per-session secret-output Secret %q key %q not found or empty", soSecretName, soKey)
	}

	fmt.Printf("test:smoke: PASS — secret-gated sidecar %q is a separate pod %q, Ready, SMOKE_SECRET wired from Secret %q[%q]\n",
		toolboxName, sidecarPodName, soSecretName, soKey)
	return nil
}

// sha256Hex returns the lowercase hex sha256 of s. Used by
// smokeSecretGatedSidecar to compute the expected fingerprint that the
// echo-MCP whoami tool would return for the pre-seeded secret value.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Manifests regenerates pkg/platform/manifests/install.yaml from
// `kubectl kustomize config/`. Committed to git so `go build ./cmd/oap` works
// without mage or kustomize on PATH.
//
// Uses exec.Command rather than sh.Output to preserve the trailing newline
// kustomize emits — the embedded file must be byte-identical to a fresh
// kustomize render so TestInstallYAMLMatchesKustomize doesn't false-positive.
func Manifests() error {
	cmd := exec.Command("kubectl", "kustomize", "config/")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("kustomize build failed: %w\n%s", err, ee.Stderr)
		}
		return fmt.Errorf("kustomize build failed: %w", err)
	}
	return os.WriteFile("pkg/platform/manifests/install.yaml", out, 0644)
}

// mustEnvtestAssets resolves the KUBEBUILDER_ASSETS directory holding the k8s
// 1.30 control-plane binaries (etcd, kube-apiserver), downloading them on first
// use, and returns the path for the integration/e2e suites to export.
//
// It captures stdout ONLY (Output, not CombinedOutput). On a cold module cache
// — every fresh CI runner — `go run setup-envtest` writes "go: downloading …"
// progress to stderr before the resolved path lands on stdout; merging the two
// poisons KUBEBUILDER_ASSETS with those lines, so envtest tries to fork/exec
// "<download-progress>\n…/etcd" and dies with "no such file or directory",
// failing every envtest-backed test. `-p path` keeps stdout to just the path
// even while it downloads both the module and the binaries, so stdout is clean.
func mustEnvtestAssets() string {
	out, err := exec.Command("go", "run",
		"sigs.k8s.io/controller-runtime/tools/setup-envtest",
		"use", "-p", "path", "1.30.x",
	).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			fmt.Fprintln(os.Stderr, string(ee.Stderr))
		}
		panic(err)
	}
	return strings.TrimSpace(string(out))
}

// spicedbContainerCount returns the number of running spicedb containers THIS
// repo's test fixture started, or 0 if docker is unavailable. Used to warn on
// leaked test containers after the integration/e2e suites.
//
// Filtering on the fixture's label rather than `ancestor=authzed/spicedb` is
// deliberate: the ancestor filter also counts a SpiceDB the developer runs for
// their own work on the same machine, so the "leaked N containers" warning fired
// on containers the suite never created — and pointed at containers nothing here
// may touch.
func spicedbContainerCount() int {
	out, err := sh.Output("docker", "ps", "-q",
		"--filter", "label="+testspicedb.LabelFixture+"="+testspicedb.LabelFixtureValue)
	if err != nil || strings.TrimSpace(out) == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(out), "\n"))
}

// spicedbRunID mints an identifier for ONE suite invocation. It is exported to
// the test processes as AP_TEST_RUN_ID and lands on every fixture container
// that invocation starts.
//
// This is what lets the post-run sweep remove the run's containers without
// consulting owner liveness at all: `go test` has returned, so every binary
// that could still be holding one has exited. Minted fresh here rather than
// read from the environment, so two worktrees running suites at the same time
// can never claim each other's containers — even if a developer has exported
// the variable.
func spicedbRunID(target string) string {
	return fmt.Sprintf("%s-%d-%d", strings.ReplaceAll(target, ":", "-"), os.Getpid(), time.Now().UnixNano())
}

// withSpicedbSweep runs a suite with SpiceDB fixture-container reaping on both
// sides of it, and reports anything it could not remove.
//
// The leak this closes is structural, not a bug in the fixture's own cleanup.
// test/testspicedb sweeps abandoned containers when a NEW container starts, so
// after the LAST test binary of a suite exits there is nothing left to sweep —
// that run's containers survive until some later run happens to start one.
// Counting them and printing a warning, which is all this used to do, detects
// the leak and then leaves it on the machine to degrade every subsequent run.
//
// Sweeping BEFORE as well as after matters for the same reason reapEnvtestOrphans
// does: a run killed with Ctrl-C never reaches its own post-run sweep, and the
// next run should not have to start on a machine full of its containers.
//
// Every safety rule lives in testspicedb.Classify — nothing here filters by
// image or by name, and a container this repo's fixture did not start is never
// a candidate.
func withSpicedbSweep(target string, envs map[string]string, run func(map[string]string) error) error {
	runID := spicedbRunID(target)
	envs[testspicedb.EnvRunID] = runID

	sweepSpicedbContainers(target+" (pre-run)", "")
	before := spicedbContainerCount()

	err := run(envs)

	kept := sweepSpicedbContainers(target, runID)
	if after := spicedbContainerCount(); after > before {
		// Still a leak after sweeping. Silence would be worse than the old
		// warning, so say which containers survived and which rule spared each.
		fmt.Fprintf(os.Stderr, "WARNING: %d spicedb container(s) leaked by %s and were NOT reaped:\n", after-before, target)
		for _, d := range kept {
			fmt.Fprintf(os.Stderr, "    %s %s: %s\n", shortDockerID(d.ID), d.Name, d.Reason)
			if d.Err != nil {
				fmt.Fprintf(os.Stderr, "        removal failed: %v\n", d.Err)
			}
		}
	}
	return err
}

// sweepSpicedbContainers reaps this repo's leaked SpiceDB fixture containers and
// returns the ones it left in place, so the caller can explain a residual leak.
//
// Best-effort, like reapEnvtestOrphans: a sweep that cannot run must not fail
// the suite. Never silent, with one deliberate exception — no Docker daemon,
// where the suite skipped its SpiceDB tests too and there is nothing to warn
// about.
func sweepSpicedbContainers(label, runID string) []testspicedb.Decision {
	res, err := testspicedb.Reap(runID)
	if err != nil {
		if errors.Is(err, testspicedb.ErrDockerUnavailable) {
			return nil
		}
		fmt.Fprintf(os.Stderr, "warning: could not sweep spicedb fixture containers for %s (%v); leaked containers will slow later runs\n", label, err)
		return nil
	}
	if len(res.Removed) > 0 {
		fmt.Fprintf(os.Stderr, "==> reaped %d leaked spicedb container(s) for %s\n", len(res.Removed), label)
	}
	return res.Kept
}

// shortDockerID trims a container ID to the 12 characters the docker CLI shows,
// so a warning can be pasted straight into `docker inspect`.
func shortDockerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Docs regenerates the showcase docs site's generated reference pages from the
// live source of truth (the cobra tree, the CRD schemas) — see Cli and Crd in
// clidocs.go. Both are deterministic; neither invokes an LLM.
type Docs mg.Namespace

type Audit mg.Namespace

// auditGenerator builds an audit Generator wired to the production claude/git
// execs and configured from the AUDIT_* environment variables. See the
// "Auditing" section of AGENTS.md for what a pass does.
func auditGenerator() *auditgen.Generator {
	return &auditgen.Generator{
		Run:      claudeexec.Run,
		Diff:     claudeexec.Diff,
		Model:    os.Getenv("AUDIT_CLAUDE_MODEL"),
		DiffBase: os.Getenv("AUDIT_DIFF_BASE"),
		DryRun:   os.Getenv("AUDIT_DRY_RUN") != "",
		Out:      os.Stdout,
	}
}

// All runs a whole-repo audit pass (all of pkg/, internal/ and cmd/) across every lens and
// writes docs/audits/<date>-all-audit.md. Expensive by design — it partitions
// the tree into subsystem groups and fans out; reserve it for a periodic health
// check and use audit:recent / audit:pkg for routine work.
func (Audit) All() error {
	return auditGenerator().All(context.Background())
}

// Recent audits only the code changed vs AUDIT_DIFF_BASE (default master):
// `git diff master...HEAD`. A no-op when that diff is empty. Writes
// docs/audits/<date>-recent-audit.md. Good for a pre-merge pass on a branch.
func (Audit) Recent() error {
	return auditGenerator().Recent(context.Background())
}

// Pkg audits a single package or directory named by its repo-relative path and
// writes docs/audits/<date>-<slug>-audit.md. Example:
// `mage audit:pkg pkg/web/webui/chat`. A separate target from audit:all because mage
// has no optional positional arguments.
func (Audit) Pkg(path string) error {
	return auditGenerator().Pkg(context.Background(), path)
}
