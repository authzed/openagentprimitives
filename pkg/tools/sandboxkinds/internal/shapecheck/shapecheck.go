// Package shapecheck is a test-support sandbox backend modelling a provider
// OUTSIDE the cluster: an opaque ID handle, no Kubernetes client, no pod, a
// native file API and native snapshots.
//
// It ships no production behavior. Its job is to fail compilation if a
// Kubernetes assumption leaks into the sandboxkinds interfaces, while those
// interfaces are still cheap to change — and to give the conformance suite a
// second, deliberately unlike implementation to run against.
//
// It is NOT registered in the process-wide registry: it is not a backend
// anyone may select from a SpiceboxClass.
package shapecheck

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// KindName is this backend's name. Never registered; see the package doc.
const KindName = "shapecheck"

// Kind models an off-cluster sandbox provider.
type Kind struct{}

func (Kind) Name() string { return KindName }

func (Kind) Supports(f sandboxkinds.Feature) bool {
	switch f {
	case sandboxkinds.FeatureSharedWorkspace:
		// Shareable inside this provider's own volume world — see
		// WorkspaceDomain. Not shareable with a Kubernetes PVC.
		return true
	case sandboxkinds.FeatureHostEgressAllowlist:
		// A provider-enforced hostname allowlist, which the Kubernetes
		// backends cannot offer.
		return true
	case sandboxkinds.FeatureConfigMapMounts, sandboxkinds.FeatureToolchainOverlay, sandboxkinds.FeatureUnpackMounts:
		// All three are Kubernetes volume/pod mechanics — ConfigMap mounts,
		// toolchain overlays, and initContainer-based unpacking — with no
		// off-cluster analogue.
		return false
	default:
		return false
	}
}

func (Kind) WorkspaceDomain() string { return "shapecheck-volume" }

func (Kind) ValidateClass(v1alpha1.SpiceboxClassSpec) error { return nil }

func (Kind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	// Deliberately ignores Deps: an off-cluster backend needs no cluster
	// client. If this ever stops compiling, the seam has grown a Kubernetes
	// requirement.
	var rt sandboxkinds.Runtime = &Runtime{
		sandboxes: map[string]*sandbox{},
	}
	return rt, nil
}

type sandbox struct {
	phase sandboxkinds.Phase
	files map[string][]byte
}

// Runtime is an in-memory stand-in for a remote provider's API.
type Runtime struct {
	mu        sync.Mutex
	seq       int
	byRef     map[string]string // sessionKey → sandbox ID
	sandboxes map[string]*sandbox
}

func sessionKey(s *v1alpha1.SpiceboxSession) string { return s.Namespace + ":" + s.Name }

// Ensure creates a sandbox, or returns the one this session already has. The
// lookup is by tag rather than by deterministic name — the same convergence a
// real provider needs, since it assigns the ID.
func (r *Runtime) Ensure(_ context.Context, req sandboxkinds.EnsureRequest) (sandboxkinds.Handle, error) {
	if req.Session == nil {
		return sandboxkinds.Handle{}, fmt.Errorf("EnsureRequest.Session is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byRef == nil {
		r.byRef = map[string]string{}
	}
	key := sessionKey(req.Session)
	if id, ok := r.byRef[key]; ok {
		return sandboxkinds.Handle{Kind: KindName, Ref: id}, nil
	}
	r.seq++
	id := fmt.Sprintf("sbx-%08d", r.seq)
	r.byRef[key] = id
	r.sandboxes[id] = &sandbox{phase: sandboxkinds.PhaseReady, files: map[string][]byte{}}
	return sandboxkinds.Handle{Kind: KindName, Ref: id}, nil
}

func (r *Runtime) lookup(h sandboxkinds.Handle) (*sandbox, error) {
	if h.Kind != KindName {
		return nil, fmt.Errorf("handle belongs to sandbox kind %q, not %q", h.Kind, KindName)
	}
	s, ok := r.sandboxes[h.Ref]
	if !ok {
		return nil, nil // gone, not an error
	}
	return s, nil
}

func (r *Runtime) Status(_ context.Context, h sandboxkinds.Handle) (sandboxkinds.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.lookup(h)
	if err != nil {
		return sandboxkinds.Status{}, err
	}
	if s == nil {
		return sandboxkinds.Status{Phase: sandboxkinds.PhaseGone, Reason: "SandboxGone"}, nil
	}
	return sandboxkinds.Status{Phase: s.phase, Reason: "SandboxReady"}, nil
}

func (r *Runtime) Teardown(_ context.Context, h sandboxkinds.Handle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.lookup(h); err != nil {
		return err
	}
	delete(r.sandboxes, h.Ref)
	for k, id := range r.byRef {
		if id == h.Ref {
			delete(r.byRef, k)
		}
	}
	return nil
}

// Executor returns a transport bound to h. A real provider would dial its own
// API here; the point is that the signature needs no Kubernetes plumbing.
func (r *Runtime) Executor(h sandboxkinds.Handle) (exec.Executor, error) {
	if h.Kind != KindName {
		return nil, fmt.Errorf("handle belongs to sandbox kind %q, not %q", h.Kind, KindName)
	}
	return boundExec{ref: h.Ref}, nil
}

// Watches is empty: there is nothing cluster-local to observe.
func (r *Runtime) Watches() []sandboxkinds.Watch { return nil }

func (r *Runtime) PutFile(_ context.Context, h sandboxkinds.Handle, path string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.lookup(h)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("sandbox %q is gone", h.Ref)
	}
	s.files[path] = append([]byte(nil), data...)
	return nil
}

func (r *Runtime) GetFile(_ context.Context, h sandboxkinds.Handle, path string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.lookup(h)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("sandbox %q is gone", h.Ref)
	}
	b, ok := s.files[path]
	if !ok {
		return nil, fmt.Errorf("no such file %q", path)
	}
	return append([]byte(nil), b...), nil
}

func (r *Runtime) Snapshot(_ context.Context, h sandboxkinds.Handle) (sandboxkinds.SnapshotRef, error) {
	return sandboxkinds.SnapshotRef{Kind: KindName, Ref: h.Ref + "-snap"}, nil
}

func (r *Runtime) Restore(_ context.Context, _ sandboxkinds.Handle, _ sandboxkinds.SnapshotRef) error {
	return nil
}

// boundExec echoes its argv so the conformance suite can prove the transport
// carries a command, env and exit code without a cluster.
type boundExec struct{ ref string }

func (b boundExec) Exec(_ context.Context, req exec.Request) (exec.Result, error) {
	if len(req.Command) == 0 {
		return exec.Result{ExitCode: -1}, fmt.Errorf("empty command")
	}
	out := req.Command[len(req.Command)-1]
	if v, ok := req.Env["ECHO"]; ok {
		out = v
	}
	return exec.Result{Stdout: []byte(out), ExitCode: 0}, nil
}

// DemoSession returns a fixture session. Exported for the conformance suite.
func DemoSession() *v1alpha1.SpiceboxSession {
	return &v1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns", UID: "demo-uid"},
		Spec:       v1alpha1.SpiceboxSessionSpec{Class: "demo-class"},
	}
}

// DemoClass returns a fixture class spec. Exported for the conformance suite.
func DemoClass() v1alpha1.SpiceboxClassSpec {
	return v1alpha1.SpiceboxClassSpec{
		Image: "demo.invalid/spicebox-sandbox:test",
		Resources: v1alpha1.SpiceboxResources{
			CPU:              resource.MustParse("1"),
			Memory:           resource.MustParse("256Mi"),
			EphemeralStorage: resource.MustParse("1Gi"),
		},
	}
}

var (
	_ sandboxkinds.Kind           = Kind{}
	_ sandboxkinds.Runtime        = (*Runtime)(nil)
	_ sandboxkinds.FileTransferer = (*Runtime)(nil)
	_ sandboxkinds.Snapshotter    = (*Runtime)(nil)
	_ exec.Executor               = boundExec{}
)
