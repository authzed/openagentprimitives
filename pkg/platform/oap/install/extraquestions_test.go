package install_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// bundleFixture loads the shared .oap fixture. Tests must never read the
// examples directory — it is user-facing demo material, not a fixture.
func bundleFixture(t *testing.T) *oap.Bundle {
	t.Helper()
	b, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err, "load the shared oap fixture")
	return b
}

// bundleWithMemory builds a minimal bundle whose lone AgentClass CR already
// declares spec.resources.memory. The shared oaptest fixture's AgentClass has
// no spec.resources map at all, and setAtPath (pkg/platform/oap/overlay.go) requires
// every intermediate path segment to already resolve to a map — it does not
// auto-vivify one — so a synthetic capacity-style extra question needs a CR
// that already has the map to bind an answer into it. Built directly (no
// on-disk fixture) since Bundle.Validate only needs a well-formed Manifest and
// an allowed-Kind CR, not the shared fixture's questions/secrets.
func bundleWithMemory(t *testing.T) *oap.Bundle {
	t.Helper()
	return &oap.Bundle{
		Manifest: &oap.Manifest{
			OapFormatVersion: "1",
			Agent:            oap.Agent{Name: "demo-agent", Version: "1.0.0"},
		},
		Manifests: []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: demo-class
spec:
  description: demo fixture agent
  resources:
    memory: "1Gi"
`),
	}
}

func TestInstall_ExtraQuestions(t *testing.T) {
	t.Run("hook's question is resolved and its binding overlays the CR: spec.resources.memory changes", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
		var hookCalled bool

		result, err := install.Install(context.Background(), c, bundleWithMemory(t), oap.Answers{}, nil, install.InstallOpts{
			Namespace: "test-ns",
			Sets:      map[string]string{"capacity.demo-class.memory": "512Mi"},
			ExtraQuestions: func(_ context.Context, crs []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				hookCalled = true
				return []oap.Question{{
					Name:    "capacity.demo-class.memory",
					Type:    oap.QString,
					Prompt:  "SpiceboxClass demo-class exceeds this cluster's memory ceiling — lower it?",
					Binding: []oap.Binding{{Target: "AgentClass/demo-class#spec.resources.memory"}},
				}}, nil, nil
			},
		})

		require.NoError(t, err, "install must succeed once the extra question is answered via Sets")
		assert.True(t, hookCalled, "the hook must run")
		require.NotNil(t, result)

		got := &unstructured.Unstructured{}
		got.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
		got.SetKind("AgentClass")
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "test-ns", Name: "demo-class"}, got), "Get the applied AgentClass")

		mem, found, err := unstructured.NestedString(got.Object, "spec", "resources", "memory")
		require.NoError(t, err)
		require.True(t, found, "spec.resources.memory must be set on the applied CR")
		assert.Equal(t, "512Mi", mem, "the resolved extra answer must have overlaid spec.resources.memory")
	})

	t.Run("hook's hard error aborts before any cluster write", func(t *testing.T) {
		var writes int
		c := fake.NewClientBuilder().
			WithScheme(runtime.NewScheme()).
			WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					writes++
					return nil
				},
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++
					return nil
				},
			}).Build()

		var called bool
		_, err := install.Install(context.Background(), c, bundleFixture(t), oap.Answers{}, nil, install.InstallOpts{
			Namespace: "test-ns",
			ExtraQuestions: func(context.Context, []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				called = true
				return nil, nil, errors.New("SpiceboxClass demo-sandbox cannot fit this cluster")
			},
		})

		require.Error(t, err)
		assert.True(t, called, "the hook must run")
		assert.Contains(t, err.Error(), "cannot fit this cluster", "the hook's message must reach the user verbatim")
		assert.Zero(t, writes, "an aborting hook must leave nothing written")
	})

	t.Run("non-interactive install with an unanswered required extra question fails, naming it", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

		_, err := install.Install(context.Background(), c, bundleFixture(t), oap.Answers{}, nil, install.InstallOpts{
			Namespace:   "test-ns",
			Interactive: false,
			ExtraQuestions: func(_ context.Context, crs []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				return []oap.Question{{
					Name:    "capacity.demo-class.memory",
					Type:    oap.QString,
					Prompt:  "SpiceboxClass demo-class exceeds this cluster's memory ceiling — lower it?",
					Binding: []oap.Binding{{Target: "AgentClass/demo-class#spec.description"}},
				}}, nil, nil
			},
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "capacity.demo-class.memory", "the error must name the unanswered question")
		assert.Contains(t, err.Error(), "missing required question", "must explain why, not just fail silently")
	})

	t.Run("hook's notices land in Result.Warnings", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

		result, err := install.Install(context.Background(), c, bundleFixture(t), oap.Answers{}, nil, install.InstallOpts{
			Namespace: "test-ns",
			ExtraQuestions: func(context.Context, []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				return nil, []string{"capacity check skipped: cluster ceiling unknown"}, nil
			},
		})

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, []string{"capacity check skipped: cluster ceiling unknown"}, result.Warnings, "the hook's notice must reach Result.Warnings verbatim")
	})

	t.Run("nil ExtraQuestions is skipped; install still succeeds with no warnings", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

		result, err := install.Install(context.Background(), c, bundleFixture(t), oap.Answers{}, nil, install.InstallOpts{Namespace: "test-ns"})

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Empty(t, result.Warnings)
	})
}
