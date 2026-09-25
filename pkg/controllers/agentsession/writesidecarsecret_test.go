package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestWriteSidecarSecret_ReplacesDataWholesale(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: c}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "sess-ns"},
	}
	ctx := context.Background()

	require.NoError(t, r.writeSidecarSecret(ctx, sess, "demo-creds",
		map[string]string{"github": "tok-1", "linear": "tok-2"}))

	// Simulate what the apiserver does: populate Data from StringData.
	var secret corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "sess-ns", Name: "demo-creds"}, &secret))
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	for k, v := range secret.StringData {
		secret.Data[k] = []byte(v)
	}
	secret.StringData = nil
	require.NoError(t, c.Update(ctx, &secret))

	// Re-materialize with "linear" removed.
	require.NoError(t, r.writeSidecarSecret(ctx, sess, "demo-creds",
		map[string]string{"github": "tok-1"}))

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "sess-ns", Name: "demo-creds"}, &got))
	// After the fix, Data should be nil (cleared) and StringData should contain only the new values.
	// In a real apiserver, StringData would be converted to Data and cleared, but the fake client
	// stores both. The important thing is that Data was cleared before StringData was set.
	assert.NotContains(t, got.Data, "linear", "Data must not contain old keys after update")
	assert.Contains(t, got.StringData, "github", "surviving key must remain in StringData")
	assert.NotContains(t, got.StringData, "linear", "removed key must be dropped from StringData")
	assert.Equal(t, "tok-1", got.StringData["github"])
}
