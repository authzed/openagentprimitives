package settingswizard

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// testScheme returns a runtime.Scheme with corev1 + v1alpha1 registered,
// matching the pattern used across cmd/oap tests.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s), "corev1.AddToScheme")
	require.NoError(t, v1alpha1.AddToScheme(s), "v1alpha1.AddToScheme")
	return s
}

func TestLoadExisting_absentReturnsEmptyNamedCluster(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	got, err := LoadExisting(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ClusterAgentSettingsName, got.Name)
	assert.Nil(t, got.Spec.Limits)
}

func TestLoadExisting_returnsExistingForPrepopulate(t *testing.T) {
	existing := &v1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
		Spec:       v1alpha1.SettingsSpec{Limits: &v1alpha1.SettingsLimits{Pinning: &v1alpha1.PinningPolicy{}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	got, err := LoadExisting(context.Background(), c)
	require.NoError(t, err)
	require.NotNil(t, got.Spec.Limits)
	assert.NotNil(t, got.Spec.Limits.Pinning)
}
