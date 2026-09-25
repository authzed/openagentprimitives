package kube_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

func newConfigMap(name, namespace string, data map[string]string) *unstructured.Unstructured {
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	cm.SetName(name)
	cm.SetNamespace(namespace)
	_ = unstructured.SetNestedStringMap(cm.Object, data, "data")
	return cm
}

func TestApply(t *testing.T) {
	cases := []struct {
		name     string
		existing *unstructured.Unstructured
		apply    *unstructured.Unstructured
		wantData map[string]string
	}{
		{
			name:     "create new object: data persists",
			existing: nil,
			apply:    newConfigMap("hello", "default", map[string]string{"k": "v"}),
			wantData: map[string]string{"k": "v"},
		},
		{
			name:     "update existing object: data overwritten",
			existing: newConfigMap("hello", "default", map[string]string{"k": "old"}),
			apply:    newConfigMap("hello", "default", map[string]string{"k": "new"}),
			wantData: map[string]string{"k": "new"},
		},
	}
	gvr := corev1.SchemeGroupVersion.WithResource("configmaps")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := runtime.NewScheme()
			require.NoError(t, scheme.AddToScheme(s))
			var dyn = dynfake.NewSimpleDynamicClient(s)
			if tc.existing != nil {
				dyn = dynfake.NewSimpleDynamicClient(s, tc.existing)
				// The dynamic fake serves an apply patch as a strategic-merge
				// patch, which fails for *unstructured.Unstructured ("unable to
				// find api field in struct Unstructured"). Only the fake needs the
				// Update fallback; production must surface that rejection instead —
				// see TestApply_ProductionDoesNotFallBackToUpdate.
				kube.SetApplyUpdateFallbackForTest(t)
			}

			require.NoError(t, kube.Apply(context.Background(), dyn, tc.apply, "ap-test"))

			got, err := dyn.Resource(gvr).Namespace("default").Get(context.Background(), "hello", metav1.GetOptions{})
			require.NoError(t, err)
			data, _, _ := unstructured.NestedStringMap(got.Object, "data")
			assert.Equal(t, tc.wantData, data)
		})
	}
}

// TestApply_ProductionDoesNotFallBackToUpdate asserts the PRODUCTION shape —
// note the deliberate absence of SetApplyUpdateFallbackForTest. A patch
// rejection that is not NotFound must surface to the caller, not degrade into a
// full-object Update that replaces the live object with the manifest (dropping
// controller-added finalizers, ownerReferences, and every field another manager
// owns) and then report success.
func TestApply_ProductionDoesNotFallBackToUpdate(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(s))
	dyn := dynfake.NewSimpleDynamicClient(s, newConfigMap("hello", "default", map[string]string{"k": "old"}))

	updates := 0
	dyn.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		// The shape the apiserver returns when a stored object no longer matches
		// an upgraded CRD schema — reachable on the `oap install` upgrade path.
		return true, nil, apierrors.NewBadRequest("failed to create typed patch object")
	})
	dyn.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})

	err := kube.Apply(context.Background(), dyn,
		newConfigMap("hello", "default", map[string]string{"k": "new"}), "ap-test")

	require.Error(t, err, "a non-NotFound patch failure must be returned")
	assert.Contains(t, err.Error(), "failed to create typed patch object")
	assert.Zero(t, updates, "production Apply must never issue a full-object Update")

	got, err := dyn.Resource(corev1.SchemeGroupVersion.WithResource("configmaps")).
		Namespace("default").Get(context.Background(), "hello", metav1.GetOptions{})
	require.NoError(t, err)
	data, _, _ := unstructured.NestedStringMap(got.Object, "data")
	assert.Equal(t, map[string]string{"k": "old"}, data,
		"the live object must be left untouched when the apply is rejected")
}

// TestResolveGVRScope guards the namespaced/cluster-scoped classification of the
// built-in kinds the install bundle applies. A cluster-scoped kind misclassified
// as namespaced makes `oap install`/`oap init` demand metadata.namespace and fail
// (the apiserver would 405) — the regression that hit ValidatingWebhookConfiguration
// and StorageClass.
// TestApplyClusterScopedCR exercises the actual bring-up path that failed:
// applying a namespace-less, cluster-scoped SpiceboxToolchain. Before the fix,
// resolveGVR misclassified it as namespaced and Apply refused it with
// "missing metadata.namespace" (the apiserver would 405).
func TestApplyClusterScopedCR(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "spiceboxtoolchains"}
	s := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(s, map[schema.GroupVersionResource]string{
		gvr: "SpiceboxToolchainList",
	})

	tc := &unstructured.Unstructured{}
	tc.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	tc.SetKind("SpiceboxToolchain")
	tc.SetName("go") // deliberately no namespace — it is cluster-scoped

	require.NoError(t, kube.Apply(context.Background(), dyn, tc, "ap-test"),
		"cluster-scoped SpiceboxToolchain must apply without metadata.namespace")

	got, err := dyn.Resource(gvr).Get(context.Background(), "go", metav1.GetOptions{})
	require.NoError(t, err, "applied SpiceboxToolchain must be retrievable cluster-scoped")
	assert.Empty(t, got.GetNamespace(), "cluster-scoped CR must not have a namespace")
}

func TestResolveGVRScope(t *testing.T) {
	clusterScoped := []string{
		"Namespace", "ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition",
		"ValidatingWebhookConfiguration", "MutatingWebhookConfiguration", "StorageClass",
	}
	for _, k := range clusterScoped {
		ns, err := kube.ResolveNamespacedForTest("", k)
		require.NoError(t, err, k)
		assert.False(t, ns, "%s must be cluster-scoped (no metadata.namespace)", k)
	}

	namespaced := []string{
		"ConfigMap", "Secret", "Service", "ServiceAccount", "Deployment",
		"Role", "RoleBinding", "NetworkPolicy",
	}
	for _, k := range namespaced {
		ns, err := kube.ResolveNamespacedForTest("", k)
		require.NoError(t, err, k)
		assert.True(t, ns, "%s must be namespaced", k)
	}
}

// TestResolveGVRMatchesBundleCRDs is the drift guard: for every CRD shipped in
// the embedded install bundle, resolveGVR must classify its kind with the same
// scope the CRD's own spec.scope declares. This is what should have caught the
// cluster-scoped SpiceboxToolchain being demanded a namespace by `oap init` —
// the previous hand-maintained cluster-scoped list silently drifted the moment
// the CRD shipped.
func TestResolveGVRMatchesBundleCRDs(t *testing.T) {
	scopes, err := manifests.CRDScopes()
	require.NoError(t, err)
	require.NotEmpty(t, scopes, "embedded bundle must contain CRDs")
	for kind, s := range scopes {
		ns, err := kube.ResolveNamespacedForTest(s.Group, kind)
		require.NoError(t, err, kind)
		assert.Equal(t, s.Namespaced, ns,
			"%s (%s) must match its CRD spec.scope", kind, s.Group)
	}
}

func TestPluralizeCRKinds(t *testing.T) {
	// Verify the heuristic produces correct lowercased plurals for the
	// agentprimitives CR family. This protects mapperFallback for kinds
	// that aren't in the static switch.
	cases := []struct {
		kind string
		want string
	}{
		{kind: "AgentClass", want: "agentclasses"},
		{kind: "AgentIdentity", want: "agentidentities"},
		{kind: "AgentSession", want: "agentsessions"},
		{kind: "SpiceboxClass", want: "spiceboxclasses"},
		{kind: "SpiceboxSession", want: "spiceboxsessions"},
		{kind: "SpiceboxToolspec", want: "spiceboxtoolspecs"},
		{kind: "SpiceboxToolkit", want: "spiceboxtoolkits"},
		{kind: "ToolCall", want: "toolcalls"},
	}
	for _, tc := range cases {
		t.Run(tc.kind+" -> "+tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, kube.PluralizeForTest(tc.kind))
		})
	}
}
