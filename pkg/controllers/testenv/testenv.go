// Package testenv provides a controller-runtime envtest harness
// shared across controller test packages.
package testenv

import (
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	runtimepkg "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// envtestK8sVersion pins the kube version setup-envtest fetches when
// KUBEBUILDER_ASSETS isn't pre-populated. Kept in sync with the value
// in magefiles/magefile.go's mustEnvtestAssets.
const envtestK8sVersion = "1.30.x"

type Env struct {
	Cfg     *rest.Config
	Client  client.Client
	Scheme  *apiruntime.Scheme
	testEnv *envtest.Environment
}

// errAssetsUnavailable means the envtest binaries could not be located or
// downloaded. That is the ONLY condition a caller may legitimately skip on: the
// machine cannot run envtest at all.
//
// Every other failure is a real defect and must fail the test. Skipping on them
// is how an entire suite reports green while proving nothing — a malformed CEL
// rule in a CRD once turned every integration and e2e run into a silent
// all-skipped pass, because "could not install CRDs" was handled identically to
// "binaries missing".
var errAssetsUnavailable = errors.New("testenv: envtest binaries unavailable")

// startShared boots an envtest Environment with the spicebox CRDs and returns
// a ready *Env. It does NOT register t.Cleanup — callers own the lifecycle.
func startShared() (env *Env, err error) {
	_, thisFile, _, _ := runtimepkg.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	te := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	// The agent-sandbox CRDs come from the module itself, so they can never
	// drift from the Go types we compile against — both are pinned by go.mod.
	if crds, err := agentSandboxCRDs(); err != nil {
		// Loud, not fatal: only the agent-sandbox conformance test needs these,
		// and it fails with NewRuntime's own clear "CRDs are not installed"
		// message. Skipping the whole integration suite over this would hide
		// far more than it protects.
		fmt.Fprintf(os.Stderr, "testenv: agent-sandbox CRDs unavailable: %v\n", err)
	} else {
		te.CRDs = append(te.CRDs, crds...)
	}
	// When KUBEBUILDER_ASSETS isn't set (direct `go test` invocation,
	// not `mage test:integration`), shell out to setup-envtest to
	// locate the cached binaries — downloading them on first use.
	// Only this path yields a skippable outcome (no network, no disk).
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		assets, err := resolveEnvtestAssets()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errAssetsUnavailable, err)
		}
		te.BinaryAssetsDirectory = assets
	}
	// Past this point the binaries exist, so every failure is a real defect —
	// a CRD that will not install, a scheme that will not build — and is
	// returned as a hard error rather than folded into a skip.
	cfg, err := te.Start()
	if err != nil {
		return nil, fmt.Errorf("envtest boot failed (binaries present): %w", err)
	}
	sch := apiruntime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		_ = te.Stop()
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	if err := spiceboxv1alpha1.AddToScheme(sch); err != nil {
		_ = te.Stop()
		return nil, fmt.Errorf("add spicebox scheme: %w", err)
	}
	if err := sandboxv1beta1.AddToScheme(sch); err != nil {
		_ = te.Stop()
		return nil, fmt.Errorf("add sandbox scheme: %w", err)
	}
	// A DIFFERENT API group (extensions.agents.x-k8s.io): the pre-warming
	// types. agentSandboxCRDs above installs all four CRDs, so a client whose
	// scheme knew only the base group would fail on the very Get the
	// agent-sandbox backend makes for a SandboxClaim.
	if err := sandboxextv1beta1.AddToScheme(sch); err != nil {
		_ = te.Stop()
		return nil, fmt.Errorf("add sandbox-extensions scheme: %w", err)
	}
	cli, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		_ = te.Stop()
		return nil, fmt.Errorf("build client: %w", err)
	}
	return &Env{Cfg: cfg, Client: cli, Scheme: sch, testEnv: te}, nil
}

// Start boots an envtest cluster with Spicebox CRDs installed.
func Start(t *testing.T) *Env {
	t.Helper()
	// Point the logger at THIS test, then install it once. Order matters only
	// in that both must happen before any controller starts logging.
	currentT.Store(t)
	t.Cleanup(func() {
		// Compare-and-swap, not an unconditional clear: a sibling test may
		// already have claimed the writer, and stealing it back would silence
		// the test that is actually running.
		currentT.CompareAndSwap(t, nil)
	})
	setLoggerOnce.Do(func() {
		ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(testWriter{})))
	})
	env, err := startShared()
	if errors.Is(err, errAssetsUnavailable) {
		t.Skipf("envtest binaries unavailable (set KUBEBUILDER_ASSETS or run `mage test:integration`): %v", err)
	}
	if err != nil {
		// NOT a skip: the machine can run envtest and something is actually
		// broken. Failing here is what stops a whole suite going green on a
		// tree where no test ever ran.
		t.Fatalf("testenv: %v", err)
	}
	t.Cleanup(func() {
		// NEVER discard this error. A failed Stop leaves a kube-apiserver + etcd
		// pair orphaned to PID 1, and those keep burning CPU for as long as the
		// machine is up. They accumulate silently across runs, and once enough of
		// them pile up every poll-based test in the suite starts missing deadlines
		// that pass fine in isolation — which reads as "flaky tests" and is the
		// real origin of this repo's "just re-run it isolated" folklore. We found
		// 21 orphans, the oldest 28 days old, while chasing exactly that.
		//
		// Written to stderr (matching startShared's boot-failure report) rather
		// than only t.Logf, which `go test` hides unless -v is passed — the whole
		// point is that this must not be quiet. It does NOT fail the test: the
		// test's own result is still valid, and `mage test:*` reaps orphans before
		// each run (reapEnvtestOrphans) so a missed Stop self-heals.
		if err := env.testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "testenv: envtest Stop FAILED for %s — apiserver/etcd may be orphaned: %v\n", t.Name(), err)
			t.Logf("testenv: envtest Stop failed: %v", err)
		}
	})
	return env
}

// currentT is the test whose Log the controller-runtime logger writes to right
// now. It exists because controller-runtime's SetLogger is effectively
// write-once: DelegatingLogSink.Fulfill accepts the FIRST logger and ignores
// every later call. Binding the writer to a captured *testing.T therefore
// pinned every controller and channelsd log line in the process to whichever
// test happened to run first, and silently dropped the rest.
//
// That is not a cosmetic loss. It makes "did this code path even execute?"
// unanswerable for every test but one — which, in a suite where the interesting
// failures are races and gates, is exactly the question you need answered. A
// whole debugging session was spent inferring behaviour that a single log line
// would have settled.
var currentT atomic.Pointer[testing.T]

// setLoggerOnce guards the one SetLogger call that actually takes. Calling it
// per-test was always a no-op after the first; making that explicit stops the
// next reader from assuming a fresh logger per test.
var setLoggerOnce sync.Once

// testWriter forwards to whichever test is currently running. A nil currentT
// means no test owns the logger at this instant (a goroutine outliving its
// test); dropping the line is correct — t.Log after a test completes panics.
type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) {
	if t := currentT.Load(); t != nil {
		t.Log(string(p))
	}
	return len(p), nil
}

// resolveEnvtestAssets runs setup-envtest to locate (or download) the
// kube-apiserver / etcd / kubectl binaries envtest needs. Returns the
// directory containing them, or an error if setup-envtest can't be
// run or its output isn't a usable path.
func resolveEnvtestAssets() (string, error) {
	out, err := osexec.Command("go", "run",
		"sigs.k8s.io/controller-runtime/tools/setup-envtest",
		"use", "-p", "path", envtestK8sVersion,
	).CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// agentSandboxCRDs loads the agent-sandbox CRDs from the Go module cache and
// neutralizes their conversion webhook.
//
// The shipped CRD declares strategy: Webhook pointing at a Service that does
// not exist under envtest. v1beta1 is the storage version so conversion should
// never fire for the only version AP speaks — but "should never fire" is the
// kind of assumption that surfaces later as an unexplained integration
// failure, so it is removed rather than relied upon.
func agentSandboxCRDs() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	out, err := osexec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/agent-sandbox").Output()
	if err != nil {
		return nil, fmt.Errorf("locate the agent-sandbox module: %w", err)
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "k8s", "crds")

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(b, &crd); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
			Strategy: apiextensionsv1.NoneConverter,
		}
		crds = append(crds, &crd)
	}
	return crds, nil
}
