package install

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// TestManagedKindsCoverAllowedBundleKinds pins managedKinds against
// oap.AllowedBundleKindNames(). managedKinds' own doc comment says it mirrors
// oap.allowedBundleKinds "kept in sync by hand" — this test turns that
// comment into an enforced invariant: every agentprimitives-group Kind a
// .oap bundle can carry must have a managedKinds entry, or Uninstall
// silently orphans a bundled instance of it on the cluster instead of
// reaping it. That's exactly what happened when AgentUI first became
// bundleable (pkg/platform/oap's allowedBundleKinds) without a matching managedKinds
// entry here — this test would have failed it immediately, by name, instead
// of leaving the gap to be found by inspection.
func TestManagedKindsCoverAllowedBundleKinds(t *testing.T) {
	managed := make(map[string]bool, len(managedKinds))
	for _, mk := range managedKinds {
		managed[mk.gvk.Kind] = true
	}

	for _, kind := range oap.AllowedBundleKindNames() {
		assert.True(t, managed[kind],
			"oap.allowedBundleKinds carries %q but install.managedKinds has no entry for it — Uninstall would orphan a bundled %s CR on the cluster", kind, kind)
	}
}

func TestUninstallGraphPreservesOtherNamespacesClusterResources(t *testing.T) {
	obj := &v1alpha1.SpiceboxToolkit{ObjectMeta: metav1.ObjectMeta{Name: "other-private-toolkit", Labels: map[string]string{
		instance.LabelInstall: "same-name", instance.LabelInstallNamespace: "other-namespace",
	}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(obj).Build()
	count, err := UninstallGraph(context.Background(), c, "same-name", "agents")
	require.NoError(t, err)
	assert.Zero(t, count)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(obj), &v1alpha1.SpiceboxToolkit{}))
}

func TestUninstallGraphDeletesOwnedChannel(t *testing.T) {
	channel := &v1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{
		Name: "root-child-channel", Namespace: "agents",
		Labels:      map[string]string{instance.LabelInstall: "root", instance.LabelInstallNamespace: "agents"},
		Annotations: map[string]string{AnnotationDependencyPath: "child"},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(channel).Build()

	count, err := UninstallGraph(context.Background(), c, "root", "agents")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Error(t, c.Get(context.Background(), client.ObjectKeyFromObject(channel), &v1alpha1.Channel{}))
}

func TestUninstallGraphAggregatesPathQualifiedErrorsAndContinues(t *testing.T) {
	primary := errors.New("delete refused")
	root := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "agents", Labels: map[string]string{instance.LabelInstall: "root"}}}
	child := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "root-child", Namespace: "agents", Labels: map[string]string{instance.LabelInstall: "root"}, Annotations: map[string]string{AnnotationDependencyPath: "child"}}}
	var attempted []string
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(root, child).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			attempted = append(attempted, obj.GetName())
			return primary
		},
	}).Build()
	count, err := UninstallGraph(context.Background(), c, "root", "agents")
	assert.Zero(t, count)
	assert.ErrorIs(t, err, primary)
	assert.Contains(t, err.Error(), "root > child")
	assert.Equal(t, []string{"root", "root-child"}, attempted)
}
