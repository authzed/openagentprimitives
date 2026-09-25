package spiceboxclass

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	sandboxregistry "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

// fakeWarmKind is a minimal sandboxkinds.Kind registered once (below) under a
// made-up name, so validateSpec's WarmPool-vs-Prewarmer check has a
// resolvable kind to pair with a Runtime that actually implements
// sandboxkinds.Prewarmer — without pulling in a real backend's dependencies
// (a Kubernetes client, a peer CRD scheme, ...).
type fakeWarmKind struct{ name string }

func (k fakeWarmKind) Name() string                                           { return k.name }
func (k fakeWarmKind) Supports(sandboxkinds.Feature) bool                     { return false }
func (k fakeWarmKind) WorkspaceDomain() string                                { return "" }
func (k fakeWarmKind) ValidateClass(spiceboxv1alpha1.SpiceboxClassSpec) error { return nil }
func (k fakeWarmKind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	panic("fakeWarmKind.NewRuntime: not exercised by validateSpec")
}

var _ sandboxkinds.Kind = fakeWarmKind{}

// fakePrewarmRuntime satisfies sandboxkinds.Runtime and sandboxkinds.Prewarmer
// minimally: validateSpec only ever type-asserts a Runtimes map entry against
// Prewarmer, so every other method panics if a future test path exercises it
// by mistake rather than silently returning a meaningless zero value.
type fakePrewarmRuntime struct{}

func (fakePrewarmRuntime) Ensure(context.Context, sandboxkinds.EnsureRequest) (sandboxkinds.Handle, error) {
	panic("fakePrewarmRuntime.Ensure: not exercised by validateSpec")
}
func (fakePrewarmRuntime) Status(context.Context, sandboxkinds.Handle) (sandboxkinds.Status, error) {
	panic("fakePrewarmRuntime.Status: not exercised by validateSpec")
}
func (fakePrewarmRuntime) Teardown(context.Context, sandboxkinds.Handle) error {
	panic("fakePrewarmRuntime.Teardown: not exercised by validateSpec")
}
func (fakePrewarmRuntime) Executor(sandboxkinds.Handle) (exec.Executor, error) {
	panic("fakePrewarmRuntime.Executor: not exercised by validateSpec")
}
func (fakePrewarmRuntime) Watches() []sandboxkinds.Watch { return nil }
func (fakePrewarmRuntime) ReconcilePool(context.Context, sandboxkinds.PoolRequest) error {
	return nil
}
func (fakePrewarmRuntime) SweepOrphanedPools(context.Context, string, types.UID, []string) error {
	return nil
}

var (
	_ sandboxkinds.Runtime   = fakePrewarmRuntime{}
	_ sandboxkinds.Prewarmer = fakePrewarmRuntime{}
)

// fakeNonPrewarmRuntime satisfies ONLY sandboxkinds.Runtime, deliberately not
// sandboxkinds.Prewarmer. Its purpose is negative: a validateSpec that
// mistakenly checked "does runtimes[name] exist" instead of "does it
// type-assert to Prewarmer" would pass every existing test row (none of them
// puts a REAL, present-but-non-Prewarmer Runtime in the map) while still
// being wrong. This fixture is what makes that mistake detectable.
type fakeNonPrewarmRuntime struct{}

func (fakeNonPrewarmRuntime) Ensure(context.Context, sandboxkinds.EnsureRequest) (sandboxkinds.Handle, error) {
	panic("fakeNonPrewarmRuntime.Ensure: not exercised by validateSpec")
}
func (fakeNonPrewarmRuntime) Status(context.Context, sandboxkinds.Handle) (sandboxkinds.Status, error) {
	panic("fakeNonPrewarmRuntime.Status: not exercised by validateSpec")
}
func (fakeNonPrewarmRuntime) Teardown(context.Context, sandboxkinds.Handle) error {
	panic("fakeNonPrewarmRuntime.Teardown: not exercised by validateSpec")
}
func (fakeNonPrewarmRuntime) Executor(sandboxkinds.Handle) (exec.Executor, error) {
	panic("fakeNonPrewarmRuntime.Executor: not exercised by validateSpec")
}
func (fakeNonPrewarmRuntime) Watches() []sandboxkinds.Watch { return nil }

var _ sandboxkinds.Runtime = fakeNonPrewarmRuntime{}

// fakeWarmKindName is registered exactly once for the whole test binary of
// this package (init runs once regardless of how many tests use it).
const fakeWarmKindName = "fake-prewarm-kind"

func init() {
	sandboxregistry.Register(fakeWarmKind{name: fakeWarmKindName})
}

func goodResources() spiceboxv1alpha1.SpiceboxResources {
	return spiceboxv1alpha1.SpiceboxResources{
		CPU:              resource.MustParse("500m"),
		Memory:           resource.MustParse("256Mi"),
		EphemeralStorage: resource.MustParse("100Mi"),
	}
}

func goodTools() []spiceboxv1alpha1.SpiceboxTool {
	return []spiceboxv1alpha1.SpiceboxTool{
		{Name: "echo", Command: []string{"/bin/echo"}},
	}
}

func TestValidateSpec(t *testing.T) {
	// The reserved set the reconciler derives from the builtin toolkits
	// (gh → GITHUB_TOKEN, claude → ANTHROPIC_API_KEY).
	reservedEnv := []string{"GITHUB_TOKEN", "ANTHROPIC_API_KEY"}

	tests := []struct {
		name       string
		spec       spiceboxv1alpha1.SpiceboxClassSpec
		runtimes   sandboxkinds.Runtimes
		wantOK     bool
		wantReason string
		msgSubstr  string
	}{
		{
			name: "ok",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x", Resources: goodResources(), Tools: goodTools(),
			},
			wantOK: true,
		},
		{
			name: "missing image",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "", Resources: goodResources(), Tools: goodTools(),
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidImage,
			msgSubstr:  "spec.image",
		},
		{
			name: "whitespace-only image",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "   ", Resources: goodResources(), Tools: goodTools(),
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidImage,
			msgSubstr:  "spec.image",
		},
		{
			name: "no tools",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x", Resources: goodResources(), Tools: nil,
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidTool,
			msgSubstr:  "at least one",
		},
		{
			name: "tool with empty name",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:     "x",
				Resources: goodResources(),
				Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "", Command: []string{"/bin/x"}}},
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidTool,
			msgSubstr:  "name is required",
		},
		{
			name: "duplicate tool name",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:     "x",
				Resources: goodResources(),
				Tools: []spiceboxv1alpha1.SpiceboxTool{
					{Name: "a", Command: []string{"/bin/a"}},
					{Name: "a", Command: []string{"/bin/b"}},
				},
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidTool,
			msgSubstr:  "duplicate",
		},
		{
			name: "tool with empty command",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:     "x",
				Resources: goodResources(),
				Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "a", Command: nil}},
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidTool,
			msgSubstr:  "command is required",
		},
		{
			name: "tool with blank command[0]",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:     "x",
				Resources: goodResources(),
				Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "a", Command: []string{""}}},
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidTool,
			msgSubstr:  "command is required",
		},
		{
			name: "missing cpu",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x",
				Resources: spiceboxv1alpha1.SpiceboxResources{
					Memory:           resource.MustParse("256Mi"),
					EphemeralStorage: resource.MustParse("100Mi"),
				},
				Tools: goodTools(),
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidResource,
			msgSubstr:  "cpu/memory/ephemeralStorage",
		},
		{
			name: "missing memory",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x",
				Resources: spiceboxv1alpha1.SpiceboxResources{
					CPU:              resource.MustParse("500m"),
					EphemeralStorage: resource.MustParse("100Mi"),
				},
				Tools: goodTools(),
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidResource,
		},
		{
			name: "missing ephemeral-storage",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x",
				Resources: spiceboxv1alpha1.SpiceboxResources{
					CPU:    resource.MustParse("500m"),
					Memory: resource.MustParse("256Mi"),
				},
				Tools: goodTools(),
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidResource,
		},
		{
			name: "envDefaults shadows auth-injected GITHUB_TOKEN",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:       "x",
				Resources:   goodResources(),
				Tools:       goodTools(),
				EnvDefaults: map[string]string{"GITHUB_TOKEN": "operator-supplied"},
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidEnvDefaults,
			msgSubstr:  "GITHUB_TOKEN",
		},
		{
			name: "envDefaults with only GIT_DIR is allowed",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:       "x",
				Resources:   goodResources(),
				Tools:       goodTools(),
				EnvDefaults: map[string]string{"GIT_DIR": "/var/ap-git/.git"},
			},
			wantOK: true,
		},
		{
			name: "warmPool.replicas>0 on a kind that is not a Prewarmer is invalid",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x", Resources: goodResources(), Tools: goodTools(),
				// Sandbox.Kind unset resolves to "pod", which implements no
				// Prewarmer — no runtimes entry needed to prove that. The
				// CLASS ITSELF set warmPool, so this is its own mistake.
				Sandbox: spiceboxv1alpha1.SandboxBackend{
					WarmPool: &spiceboxv1alpha1.WarmPoolConfig{Replicas: 3, Namespaces: []string{"ns-a"}},
				},
			},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool,
			msgSubstr:  "warmPool.replicas=3",
		},
		{
			name: "warmPool.replicas>0 on a registered kind absent from runtimes is invalid",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x", Resources: goodResources(), Tools: goodTools(),
				Sandbox: spiceboxv1alpha1.SandboxBackend{
					Kind:     fakeWarmKindName,
					WarmPool: &spiceboxv1alpha1.WarmPoolConfig{Replicas: 1, Namespaces: []string{"ns-a"}},
				},
			},
			// No runtimes map entry for fakeWarmKindName: same as a Runtime
			// that just doesn't implement Prewarmer, from validateSpec's view.
			wantReason: spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool,
			msgSubstr:  fakeWarmKindName,
		},
		{
			name: "warmPool.replicas>0 on a kind whose Runtime EXISTS but does not implement Prewarmer is invalid",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x", Resources: goodResources(), Tools: goodTools(),
				Sandbox: spiceboxv1alpha1.SandboxBackend{
					Kind:     fakeWarmKindName,
					WarmPool: &spiceboxv1alpha1.WarmPoolConfig{Replicas: 1, Namespaces: []string{"ns-a"}},
				},
			},
			// A REAL, present Runtime that just isn't a Prewarmer — distinct
			// from the row above (no entry at all). Guards against an
			// implementation that checks "runtimes[name] != nil" instead of
			// the actual type assertion.
			runtimes:   sandboxkinds.Runtimes{fakeWarmKindName: fakeNonPrewarmRuntime{}},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool,
			msgSubstr:  fakeWarmKindName,
		},
		{
			name: "warmPool.replicas>0 with no namespaces is invalid, even on a Prewarmer-capable kind",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x", Resources: goodResources(), Tools: goodTools(),
				// Replicas > 0, Namespaces empty: the capability is there but
				// nowhere to place a pool was named, and the CLASS ITSELF set
				// this — its own mistake.
				Sandbox: spiceboxv1alpha1.SandboxBackend{
					Kind:     fakeWarmKindName,
					WarmPool: &spiceboxv1alpha1.WarmPoolConfig{Replicas: 3},
				},
			},
			runtimes:   sandboxkinds.Runtimes{fakeWarmKindName: fakePrewarmRuntime{}},
			wantReason: spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool,
			msgSubstr:  "namespaces is empty",
		},
		{
			name: "warmPool.replicas: 0 (explicit off) on a non-Prewarmer kind is still valid",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x", Resources: goodResources(), Tools: goodTools(),
				Sandbox: spiceboxv1alpha1.SandboxBackend{
					WarmPool: &spiceboxv1alpha1.WarmPoolConfig{Replicas: 0},
				},
			},
			wantOK: true,
		},
		{
			name: "warmPool.replicas>0 on a kind whose runtime implements Prewarmer, with namespaces named, is valid",
			spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "x", Resources: goodResources(), Tools: goodTools(),
				Sandbox: spiceboxv1alpha1.SandboxBackend{
					Kind:     fakeWarmKindName,
					WarmPool: &spiceboxv1alpha1.WarmPoolConfig{Replicas: 3, Namespaces: []string{"ns-a", "ns-b"}},
				},
			},
			runtimes: sandboxkinds.Runtimes{fakeWarmKindName: fakePrewarmRuntime{}},
			wantOK:   true,
		},
	}
	// NOTE: there is deliberately no row here for "a cluster-tier warmPool
	// does not invalidate the class." validateSpec's signature makes that
	// unprovable AT THIS LEVEL: it takes reservedEnv, runtimes, and the
	// class's OWN spec, with no tier input at all, so such a row would be
	// byte-identical to the "ok" row above and would pass whether or not the
	// property held. The Reconcile-level proof — where the tier resolution
	// actually happens — is
	// controller_unit_test.go's
	// TestReconcile_ClusterTierWarmPoolOnNonPrewarmerKindStaysValid.

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpec(tc.spec, reservedEnv, tc.runtimes)
			if tc.wantOK {
				assert.NoError(t, err, "validateSpec should succeed")
				return
			}
			require.Error(t, err, "validateSpec should fail")
			assert.Equal(t, tc.wantReason, reasonFor(err), "validation reason")
			if tc.msgSubstr != "" {
				assert.Contains(t, err.Error(), tc.msgSubstr,
					"error message should contain %q", tc.msgSubstr)
			}
		})
	}
}
