package sandboxkinds_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

func TestNamespacedRef_RoundTrips(t *testing.T) {
	ns, name, err := sandboxkinds.ParseNamespacedRef(sandboxkinds.NamespacedRef("default", "demo"))
	require.NoError(t, err)
	assert.Equal(t, "default", ns)
	assert.Equal(t, "demo", name)
}

func TestParseNamespacedRef_RejectsMalformed(t *testing.T) {
	for _, ref := range []string{"", "no-slash", "/name", "ns/", "/"} {
		t.Run("rejects "+ref, func(t *testing.T) {
			_, _, err := sandboxkinds.ParseNamespacedRef(ref)
			require.Error(t, err, "a malformed ref must not yield a usable target")
		})
	}
}

// A class that flips kinds mid-life must not have its old handle read by the
// new backend: the ref format may coincide while meaning something different.
func TestResolveHandle_RejectsAForeignKind(t *testing.T) {
	_, _, err := sandboxkinds.ResolveHandle(
		sandboxkinds.Handle{Kind: "other", Ref: "default/demo"}, "pod")
	require.Error(t, err)
}

func TestResolveHandle_AcceptsItsOwnKind(t *testing.T) {
	ns, name, err := sandboxkinds.ResolveHandle(
		sandboxkinds.Handle{Kind: "pod", Ref: "default/demo"}, "pod")
	require.NoError(t, err)
	assert.Equal(t, "default", ns)
	assert.Equal(t, "demo", name)
}

func TestRequireWorkspaceClaim_NoSharedWorkspaceIsNoOp(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kubeshapeScheme(t)).Build()
	require.NoError(t, sandboxkinds.RequireWorkspaceClaim(t.Context(), c, &v1alpha1.SpiceboxSession{}))
}

// Absent claim: pending, not failure. The parent AgentSession is still
// creating it, so the session controller must report Progressing and requeue
// rather than fail the session outright.
func TestRequireWorkspaceClaim_AbsentClaimIsPending(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kubeshapeScheme(t)).Build()
	err := sandboxkinds.RequireWorkspaceClaim(t.Context(), c, sessionSharing("demo-workspace"))
	require.Error(t, err)
	assert.ErrorIs(t, err, sandboxkinds.ErrPreconditionPending)
}

// Existence is the bar, NOT Bound. The claim's StorageClass is
// WaitForFirstConsumer, so the sandbox pod is what triggers binding: gating on
// Bound would wait for something that can only happen after this returns.
func TestRequireWorkspaceClaim_UnboundClaimIsGoodEnough(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-workspace", Namespace: "default"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	c := fake.NewClientBuilder().WithScheme(kubeshapeScheme(t)).WithObjects(pvc).Build()

	require.NoError(t, sandboxkinds.RequireWorkspaceClaim(t.Context(), c, sessionSharing("demo-workspace")),
		"an unbound claim must satisfy the gate: WaitForFirstConsumer binds only once a pod consumes it")
}

func sessionSharing(claim string) *v1alpha1.SpiceboxSession {
	s := &v1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "default"},
	}
	s.Spec.Workspace = v1alpha1.WorkspaceConfig{
		Mode: v1alpha1.WorkspaceShared, SharedClaimName: claim,
	}
	return s
}

func kubeshapeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}
