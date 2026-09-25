package installcmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

func TestEnsureWebhookTLSIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	firstCA, err := ensureWebhookTLSWithClient(ctx, c)
	require.NoError(t, err, "first call must succeed")
	require.NotEmpty(t, firstCA, "first call must return non-empty CA bytes")

	var first corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-webhook-tls",
	}, &first), "tls Secret must exist after first call")

	for _, k := range []string{"ca.crt", "tls.crt", "tls.key"} {
		assert.NotEmpty(t, first.Data[k], "tls Secret must have %s", k)
	}
	firstKey := first.Data["tls.key"]

	secondCA, err := ensureWebhookTLSWithClient(ctx, c)
	require.NoError(t, err, "second call must succeed")
	require.NotEmpty(t, secondCA, "second call must return non-empty CA bytes")

	var second corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-webhook-tls",
	}, &second), "tls Secret must still exist after second call")

	assert.True(t, bytes.Equal(firstCA, secondCA),
		"re-install must return the same CA bytes")
	assert.True(t, bytes.Equal(firstKey, second.Data["tls.key"]),
		"re-install must not rotate the server key")
	assert.True(t, bytes.Equal(first.Data["tls.crt"], second.Data["tls.crt"]),
		"re-install must not rotate the server cert")
}
