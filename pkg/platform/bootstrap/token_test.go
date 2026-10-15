package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
)

func newFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithObjects(objs...).Build()
}

func TestEnsureTokenSecret_CreatesWhenAbsent(t *testing.T) {
	c := newFakeClient()
	created, err := EnsureTokenSecret(context.Background(), c, c, TokenSecretNamespace, "spicebox-webd-memory-token")
	require.NoError(t, err)
	assert.True(t, created, "a fresh mint reports created=true")

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: TokenSecretNamespace, Name: "spicebox-webd-memory-token"}, &got))
	assert.Len(t, string(got.Data[TokenSecretKey]), 64, "token is 64 hex chars")
	assert.Equal(t, corev1.SecretTypeOpaque, got.Type)
	assert.Contains(t, got.Labels, adoptguard.AdoptedLabel, "minted Secret carries the adopted label")
}

func TestEnsureTokenSecret_IdempotentNoRotate(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-webd-memory-token", Namespace: TokenSecretNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{TokenSecretKey: []byte("preexisting-value")},
	}
	c := newFakeClient(existing)
	created, err := EnsureTokenSecret(context.Background(), c, c, TokenSecretNamespace, "spicebox-webd-memory-token")
	require.NoError(t, err)
	assert.False(t, created, "an existing Secret reports created=false")

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: TokenSecretNamespace, Name: "spicebox-webd-memory-token"}, &got))
	assert.Equal(t, "preexisting-value", string(got.Data[TokenSecretKey]), "existing token is never rotated")
}
