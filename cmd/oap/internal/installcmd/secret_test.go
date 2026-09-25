package installcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// namespacedName builds a types.NamespacedName in the agentprimitives-system
// namespace, matching where the ensure* helpers under test operate.
func namespacedName(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "agentprimitives-system", Name: name}
}

func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	require.NoError(t, c.Get(context.Background(), namespacedName(name), &s))
	return &s
}

func TestEnsureSpiceDBDatastoreURI_BuildsSpicedbDBURIFromPostgresPassword(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-postgres-token"},
			Data:       map[string][]byte{"password": []byte("pw123"), "uri": []byte("postgres://postgres:pw123@spicebox-postgres.agentprimitives-system.svc:5432/memory?sslmode=disable")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-spicedb-token"},
			Data:       map[string][]byte{"token": []byte("tok"), "preshared_key": []byte("tok")},
		},
	).Build()

	require.NoError(t, ensureSpiceDBDatastoreURIWithClient(context.Background(), c))

	sp := getSecret(t, c, "spicebox-spicedb-token")
	assert.Equal(t,
		"postgres://postgres:pw123@spicebox-postgres.agentprimitives-system.svc:5432/spicedb?sslmode=disable",
		string(sp.Data["datastore_uri"]))
	assert.Equal(t, "tok", string(sp.Data["token"]), "existing token must not be rotated")
}

func TestEnsureSpiceDBToken_AddsPresharedKeyToLegacySecret(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-spicedb-token"},
			Data:       map[string][]byte{"token": []byte("legacy")},
		},
	).Build()

	require.NoError(t, ensureSpiceDBTokenWithClient(context.Background(), c))

	sp := getSecret(t, c, "spicebox-spicedb-token")
	assert.Equal(t, "legacy", string(sp.Data["token"]), "token preserved, never rotated")
	assert.Equal(t, "legacy", string(sp.Data["preshared_key"]), "preshared_key mirrors token")
}

func TestEnsureSpiceDBToken_CreatesSecretWithMatchingTokenAndPresharedKey(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	require.NoError(t, ensureSpiceDBTokenWithClient(context.Background(), c))

	sp := getSecret(t, c, "spicebox-spicedb-token")
	require.NotEmpty(t, sp.Data["token"])
	assert.Equal(t, string(sp.Data["token"]), string(sp.Data["preshared_key"]))
}

func TestEnsureSpiceDBToken_ConvergedSecretIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-spicedb-token"},
			Data:       map[string][]byte{"token": []byte("tok"), "preshared_key": []byte("tok")},
		},
	).Build()

	require.NoError(t, ensureSpiceDBTokenWithClient(ctx, c), "first call on an already-converged secret must succeed")

	first := getSecret(t, c, "spicebox-spicedb-token")
	firstRV := first.ResourceVersion

	require.NoError(t, ensureSpiceDBTokenWithClient(ctx, c), "second call on an already-converged secret must be a no-op")

	second := getSecret(t, c, "spicebox-spicedb-token")
	assert.Equal(t, firstRV, second.ResourceVersion, "converged secret must not be written again")
	assert.Equal(t, "tok", string(second.Data["token"]), "token must not be rotated")
	assert.Equal(t, "tok", string(second.Data["preshared_key"]), "preshared_key must remain unchanged")
}

// presharedOnlySecret is the shape the spicedb-operator documents, and what an
// External Secrets Operator / GitOps pre-provision produces: the key SpiceDB
// itself reads, with no ap-specific `token` alongside it.
func presharedOnlySecret(key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-spicedb-token"},
		Data:       map[string][]byte{"preshared_key": []byte(key)},
	}
}

func TestEnsureSpiceDBToken_AdoptsPresharedKeyOnlySecret(t *testing.T) {
	const preshared = "operator-provisioned-key"
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(presharedOnlySecret(preshared)).Build()

	require.NoError(t, ensureSpiceDBTokenWithClient(context.Background(), c))

	sp := getSecret(t, c, "spicebox-spicedb-token")
	assert.Equal(t, preshared, string(sp.Data["preshared_key"]),
		"a pre-seeded preshared_key must be adopted, not blanked — SpiceDB reads only this key")
	assert.Equal(t, preshared, string(sp.Data["token"]),
		"token must be backfilled from the adopted preshared_key for oap's own components")
}

func TestEnsureSpiceDBToken_PresharedKeyOnlySecretIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(presharedOnlySecret("operator-provisioned-key")).Build()

	require.NoError(t, ensureSpiceDBTokenWithClient(ctx, c), "first call must converge the secret")
	firstRV := getSecret(t, c, "spicebox-spicedb-token").ResourceVersion

	require.NoError(t, ensureSpiceDBTokenWithClient(ctx, c), "second call must be a no-op")

	second := getSecret(t, c, "spicebox-spicedb-token")
	assert.Equal(t, firstRV, second.ResourceVersion, "a converged secret must not be written again")
	assert.Equal(t, "operator-provisioned-key", string(second.Data["preshared_key"]))
}

// TestEnsureSpiceDBToken_DisagreeingKeysConvergeOnToken pins which half wins
// when both are present but differ: `token` is what every oap component reads,
// so it is authoritative and preshared_key is re-mirrored from it.
func TestEnsureSpiceDBToken_DisagreeingKeysConvergeOnToken(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-spicedb-token"},
			Data:       map[string][]byte{"token": []byte("tok"), "preshared_key": []byte("stale")},
		},
	).Build()

	require.NoError(t, ensureSpiceDBTokenWithClient(context.Background(), c))

	sp := getSecret(t, c, "spicebox-spicedb-token")
	assert.Equal(t, "tok", string(sp.Data["token"]), "token is authoritative and must not be rotated")
	assert.Equal(t, "tok", string(sp.Data["preshared_key"]), "preshared_key must be re-mirrored from token")
}

func TestEnsureSpiceDBToken_EmptySecretMintsBothKeys(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-spicedb-token"},
		},
	).Build()

	require.NoError(t, ensureSpiceDBTokenWithClient(context.Background(), c))

	sp := getSecret(t, c, "spicebox-spicedb-token")
	require.NotEmpty(t, sp.Data["token"], "an existing but keyless secret must be filled, not left blank")
	assert.Equal(t, string(sp.Data["token"]), string(sp.Data["preshared_key"]))
}

func TestEnsureSpiceDBDatastoreURI_ConvergedURIIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-postgres-token"},
			Data:       map[string][]byte{"password": []byte("pw123")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-spicedb-token"},
			Data: map[string][]byte{
				"token":         []byte("tok"),
				"preshared_key": []byte("tok"),
				"datastore_uri": []byte("postgres://postgres:pw123@spicebox-postgres.agentprimitives-system.svc:5432/spicedb?sslmode=disable"),
			},
		},
	).Build()

	require.NoError(t, ensureSpiceDBDatastoreURIWithClient(ctx, c), "first call on an already-converged datastore_uri must succeed")

	first := getSecret(t, c, "spicebox-spicedb-token")
	firstRV := first.ResourceVersion

	require.NoError(t, ensureSpiceDBDatastoreURIWithClient(ctx, c), "second call on an already-converged datastore_uri must be a no-op")

	second := getSecret(t, c, "spicebox-spicedb-token")
	assert.Equal(t, firstRV, second.ResourceVersion, "converged secret must not be written again")
	assert.Equal(t, string(first.Data["datastore_uri"]), string(second.Data["datastore_uri"]))
}
