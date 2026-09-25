package agentsandbox_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/agentsandbox"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

// newScheme builds a scheme for these tests. There is no shared pkg/testscheme
// in this repo (verified); each test package builds its own.
//
// Registers BOTH sandbox API groups: sandboxv1beta1 is
// "agents.x-k8s.io/v1beta1" (the Sandbox this package's Runtime creates and
// reads); sandboxextv1beta1 is the DIFFERENT "extensions.agents.x-k8s.io/v1beta1"
// group (SandboxTemplate, SandboxWarmPool, SandboxClaim -- the prewarming
// types). Conflating the two would be a real bug, not just a test smell: they
// are separate CRDs a cluster can install independently.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, sandboxv1beta1.AddToScheme(s))
	require.NoError(t, sandboxextv1beta1.AddToScheme(s))
	return s
}

func TestKind_RegisteredUnderItsName(t *testing.T) {
	k, ok := registry.Get(agentsandbox.KindName)
	require.True(t, ok, "the blank import in this test's package must register the kind")
	assert.Equal(t, "agent-sandbox", k.Name())
}

// The workspace domain is what lets a multi-bundle session mix pod and
// agent-sandbox bundles and still share one PVC. Supports(FeatureSharedWorkspace)
// alone cannot express this: two kinds can each support sharing and still be
// unable to share with each other.
func TestKind_WorkspaceDomainMatchesPod(t *testing.T) {
	assert.Equal(t, sandboxkinds.DomainKubernetesPVC, agentsandbox.Kind{}.WorkspaceDomain())
	assert.Equal(t, pod.Kind{}.WorkspaceDomain(), agentsandbox.Kind{}.WorkspaceDomain(),
		"a mixed pod + agent-sandbox shared-workspace session must be admitted")
}

func TestKind_Supports(t *testing.T) {
	k := agentsandbox.Kind{}
	assert.True(t, k.Supports(sandboxkinds.FeatureSharedWorkspace))
	assert.True(t, k.Supports(sandboxkinds.FeatureConfigMapMounts))
	assert.True(t, k.Supports(sandboxkinds.FeatureToolchainOverlay))
	// This backend renders the same PodSpec as the pod kind, initContainer
	// included, so it can honour an unpacking mount identically.
	assert.True(t, k.Supports(sandboxkinds.FeatureUnpackMounts))
	// AP enforces L3/L4 only; the hostname allowlist is recorded on status for
	// a DNS-aware policy controller, never enforced here. Claiming it would be
	// a security misstatement, not an optimism.
	assert.False(t, k.Supports(sandboxkinds.FeatureHostEgressAllowlist))
}

// Bring-your-own: AP does not install agent-sandbox. A cluster without the
// CRDs must fail closed at NewRuntime so the operator logs and skips the kind
// -- which is also what keeps Owns(&Sandbox{}) unregistered. Registering an
// informer for an absent CRD wedges the manager at "failed to wait for caches
// to sync".
func TestNewRuntime_FailsClosedWhenCRDsAbsent(t *testing.T) {
	// A mapper that knows only the core types: no Sandbox mapping.
	// newScheme registers the Sandbox types, but the client is built WITHOUT a
	// Sandbox REST mapping — exactly a cluster where the CRDs are not installed.
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	_, err := agentsandbox.Kind{}.NewRuntime(sandboxkinds.Deps{Client: c})
	require.Error(t, err, "an absent CRD must not yield a usable runtime")
	assert.Contains(t, err.Error(), "agent-sandbox")
}

func TestNewRuntime_SucceedsWhenCRDsPresent(t *testing.T) {
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithRESTMapper(mapperWithSandbox(t, s)).Build()

	rt, err := agentsandbox.Kind{}.NewRuntime(sandboxkinds.Deps{Client: c})
	require.NoError(t, err)
	require.NotNil(t, rt)
}

func TestNewRuntime_RequiresAClient(t *testing.T) {
	_, err := agentsandbox.Kind{}.NewRuntime(sandboxkinds.Deps{})
	require.Error(t, err, "a nil client must fail closed, not yield a nil-panicking runtime")
}

// mapperWithSandbox builds a RESTMapper that resolves the Sandbox GroupKind
// plus the extensions.agents.x-k8s.io prewarming kinds (SandboxTemplate,
// SandboxWarmPool, SandboxClaim), standing in for a cluster with all four CRDs
// installed.
func mapperWithSandbox(t *testing.T, s *runtime.Scheme) meta.RESTMapper {
	t.Helper()
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		sandboxv1beta1.GroupVersion,
		sandboxextv1beta1.GroupVersion,
	})
	m.Add(sandboxv1beta1.GroupVersion.WithKind("Sandbox"), meta.RESTScopeNamespace)
	m.Add(sandboxextv1beta1.GroupVersion.WithKind("SandboxTemplate"), meta.RESTScopeNamespace)
	m.Add(sandboxextv1beta1.GroupVersion.WithKind("SandboxWarmPool"), meta.RESTScopeNamespace)
	m.Add(sandboxextv1beta1.GroupVersion.WithKind("SandboxClaim"), meta.RESTScopeNamespace)
	return m
}
