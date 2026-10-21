package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: TokenSecretNamespace, Name: name}, &s), "get %s", name)
	return &s
}

func TestMintAllInternalSecrets_MintsTheTokenSet(t *testing.T) {
	c := newFakeClient()
	require.NoError(t, MintAllInternalSecrets(context.Background(), c, c, MintOptions{}))

	for _, n := range MemoryTokenSecretNames {
		s := getSecret(t, c, n)
		assert.Len(t, string(s.Data[TokenSecretKey]), 64, "%s", n)
	}
	assert.Len(t, string(getSecret(t, c, AdmindTokenSecretName).Data[TokenSecretKey]), 64)

	_, usableErr := passthroughlink.DecodeHexKey(getSecret(t, c, v1alpha1.PassthroughLinkSigningKeySecret).Data["key"])
	assert.NoError(t, usableErr, "passthrough key usable")

	sp := getSecret(t, c, spiceDBTokenSecretName)
	assert.Equal(t, string(sp.Data["token"]), string(sp.Data["preshared_key"]), "spicedb token == preshared_key")
	assert.Contains(t, string(sp.Data["datastore_uri"]), "/spicedb", "datastore URI set from postgres password")

	pg := getSecret(t, c, postgresTokenSecretName)
	assert.NotEmpty(t, pg.Data["password"])
	assert.True(t, strings.Contains(string(pg.Data["uri"]), "/memory"))
}

func TestMintAllInternalSecrets_SkipsExternalTokens(t *testing.T) {
	c := newFakeClient()
	require.NoError(t, MintAllInternalSecrets(context.Background(), c, c, MintOptions{SkipSpiceDBToken: true, SkipPostgresToken: true}))

	// External instances: those two tokens are the user's to provide.
	var sp corev1.Secret
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: TokenSecretNamespace, Name: spiceDBTokenSecretName}, &sp), "spicedb token NOT minted when external")
	var pg corev1.Secret
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: TokenSecretNamespace, Name: postgresTokenSecretName}, &pg), "postgres token NOT minted when external")

	// But the always-internal ones are still minted.
	assert.Len(t, string(getSecret(t, c, "spicebox-webd-memory-token").Data[TokenSecretKey]), 64)
}

func TestMintAllInternalSecrets_Idempotent(t *testing.T) {
	c := newFakeClient()
	require.NoError(t, MintAllInternalSecrets(context.Background(), c, c, MintOptions{}))
	before := getSecret(t, c, spiceDBTokenSecretName).Data["token"]
	require.NoError(t, MintAllInternalSecrets(context.Background(), c, c, MintOptions{}))
	after := getSecret(t, c, spiceDBTokenSecretName).Data["token"]
	assert.Equal(t, string(before), string(after), "second run does not rotate the spicedb token")
}

func TestEnsurePostgresToken_IdempotentNoRotate(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: postgresTokenSecretName, Namespace: TokenSecretNamespace},
		Data:       map[string][]byte{"password": []byte("keepme"), "uri": []byte("postgres://x/memory")},
	}
	c := newFakeClient(existing)
	require.NoError(t, EnsurePostgresToken(context.Background(), c, c))
	assert.Equal(t, "keepme", string(getSecret(t, c, postgresTokenSecretName).Data["password"]), "existing password untouched")
}
