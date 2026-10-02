package install_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// agentClassConflict builds the optimistic-concurrency error the apiserver
// returns when an SSA apply carries a resourceVersion that a concurrent write
// has already advanced past.
func agentClassConflict(name string) error {
	return apierrors.NewConflict(
		schema.GroupResource{Group: "agentprimitives.authzed.com", Resource: "agentclasses"},
		name,
		// The apiserver's registry.OptimisticLockErrorMsg, verbatim.
		errors.New("the object has been modified; please apply your changes to the latest version and try again"))
}

// TestInstall_ControllerWriteBetweenCreateAndApply_RetriesOnCurrentVersion pins
// the fix for a recurring e2e flake: applyPreparedObject Creates an object and
// then SSA-applies it conditional on the Create-time resourceVersion, but the
// operator's own controllers watch these kinds and routinely write to the fresh
// object (status, finalizers) in that window. The resulting conflict is benign
// — the object is still the one this run created — so Install must re-read the
// current resourceVersion and retry, not abort.
//
// Fake-client unit test for the same reason as the adopt-hint tests above: a
// real apiserver cannot be made to lose this race deterministically.
func TestInstall_ControllerWriteBetweenCreateAndApply_RetriesOnCurrentVersion(t *testing.T) {
	const ns = "test-ns"

	patches := 0
	c := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				if patches == 1 {
					// The controller's write landed first; the conditional
					// apply loses the race exactly once.
					return agentClassConflict(obj.GetName())
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	result, err := install.Install(context.Background(), c, bundleWithSoleAgentClass("fresh-agent"), oap.Answers{}, nil, install.InstallOpts{
		Namespace: ns,
	})

	require.NoError(t, err, "a lost race against the object's own controller must be retried, not surfaced")
	require.NotNil(t, result)
	assert.GreaterOrEqual(t, patches, 2, "the apply must have been attempted again after the conflict")
}

// TestInstall_ObjectReplacedDuringApply_FailsClosed is the safety property the
// retry must not relax: a conflict whose re-read comes back with a DIFFERENT
// uid means the object this run created was deleted and replaced mid-install.
// That is the foreign-seizure case the conditional apply exists to refuse —
// retrying on the replacement's resourceVersion would silently overwrite an
// object this run does not own.
func TestInstall_ObjectReplacedDuringApply_FailsClosed(t *testing.T) {
	const ns = "test-ns"

	patches := 0
	c := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				return agentClassConflict(obj.GetName())
			},
			Get: func(ctx context.Context, cl client.WithWatch, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
				if err := cl.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "AgentClass" {
					// The object the conflict re-read finds is a same-name
					// replacement, not the one this run created.
					u.SetUID("replacement-uid")
				}
				return nil
			},
		}).
		Build()

	_, err := install.Install(context.Background(), c, bundleWithSoleAgentClass("fresh-agent"), oap.Answers{}, nil, install.InstallOpts{
		Namespace: ns,
	})

	require.Error(t, err, "a replaced object must abort Install")
	assert.Contains(t, err.Error(), "AgentClass/fresh-agent", "the error must name the object")
	assert.Contains(t, strings.ToLower(err.Error()), "replaced", "the error must say the object was replaced, not report a bare conflict")
	assert.Equal(t, 1, patches, "a replacement is not a retriable condition; no second apply may be attempted")
}

// TestInstall_PersistentConflict_GivesUpWithConflictError bounds the retry: a
// conflict that never resolves (every attempt loses) must eventually surface
// as the apply failure it is, not loop forever.
func TestInstall_PersistentConflict_GivesUpWithConflictError(t *testing.T) {
	const ns = "test-ns"

	patches := 0
	c := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				return agentClassConflict(obj.GetName())
			},
		}).
		Build()

	_, err := install.Install(context.Background(), c, bundleWithSoleAgentClass("fresh-agent"), oap.Answers{}, nil, install.InstallOpts{
		Namespace: ns,
	})

	require.Error(t, err, "a conflict that never resolves must fail the Install")
	assert.Contains(t, err.Error(), "AgentClass/fresh-agent", "the error must name the object")
	assert.Greater(t, patches, 1, "the conflict must have been retried before giving up")
	assert.Less(t, patches, 10, "the retry must be bounded")
}
