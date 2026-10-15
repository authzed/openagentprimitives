//go:build integration

package bootstrap_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
)

func TestMintAllInternalSecretsEnvtest_MintsOnceIdempotent(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootstrap.TokenSecretNamespace}}
	if err := env.Client.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create namespace")
	}

	require.NoError(t, bootstrap.MintAllInternalSecrets(ctx, env.Client, env.Client, bootstrap.MintOptions{}))

	want := append(append([]string{}, bootstrap.MemoryTokenSecretNames...),
		bootstrap.AdmindTokenSecretName, v1alpha1.PassthroughLinkSigningKeySecret,
		"spicebox-spicedb-token", "spicebox-postgres-token")
	for _, n := range want {
		var s corev1.Secret
		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: bootstrap.TokenSecretNamespace, Name: n}, &s), "minted %s", n)
	}

	// Second run is idempotent — no rotation of the spicedb token.
	var sp1 corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: bootstrap.TokenSecretNamespace, Name: "spicebox-spicedb-token"}, &sp1))
	require.NoError(t, bootstrap.MintAllInternalSecrets(ctx, env.Client, env.Client, bootstrap.MintOptions{}))
	var sp2 corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: bootstrap.TokenSecretNamespace, Name: "spicebox-spicedb-token"}, &sp2))
	assert.Equal(t, string(sp1.Data["token"]), string(sp2.Data["token"]), "spicedb token not rotated on re-run")
}

func TestMintAllInternalSecretsEnvtest_SkipsExternal(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bootstrap.TokenSecretNamespace}}
	if err := env.Client.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}

	require.NoError(t, bootstrap.MintAllInternalSecrets(ctx, env.Client, env.Client, bootstrap.MintOptions{SkipSpiceDBToken: true, SkipPostgresToken: true}))

	var sp corev1.Secret
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: bootstrap.TokenSecretNamespace, Name: "spicebox-spicedb-token"}, &sp)
	assert.True(t, apierrors.IsNotFound(err), "spicedb token not minted when external")
}
