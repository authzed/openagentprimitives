package installcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

func TestEnsureNATSIdentityIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	require.NoError(t, ensureNATSIdentityWithClient(ctx, c), "first call must succeed")

	var first corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-nats-identity",
	}, &first), "identity Secret must exist after first call")

	firstSeed := first.Data["account-signing-seed"]
	require.NotEmpty(t, firstSeed, "account-signing-seed must be populated")

	authConf := first.Data["auth.conf"]
	require.NotEmpty(t, authConf, "auth.conf must be populated")
	assert.Contains(t, string(authConf), "operator:")
	assert.Contains(t, string(authConf), "resolver: MEMORY")
	assert.Contains(t, string(authConf), "resolver_preload")

	require.NoError(t, ensureNATSIdentityWithClient(ctx, c), "second call must succeed")

	var second corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-nats-identity",
	}, &second), "identity Secret must still exist after second call")

	assert.True(t, bytes.Equal(firstSeed, second.Data["account-signing-seed"]),
		"re-install must not rotate the account signing seed")
}

func TestEnsureNATSTLSIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	require.NoError(t, ensureNATSTLSWithClient(ctx, c), "first call must succeed")

	var first corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-nats-tls",
	}, &first), "tls Secret must exist after first call")

	for _, k := range []string{"ca.crt", "tls.crt", "tls.key"} {
		assert.NotEmpty(t, first.Data[k], "tls Secret must have %s", k)
	}
	firstKey := first.Data["tls.key"]

	require.NoError(t, ensureNATSTLSWithClient(ctx, c), "second call must succeed")

	var second corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-nats-tls",
	}, &second), "tls Secret must still exist after second call")

	assert.True(t, bytes.Equal(firstKey, second.Data["tls.key"]),
		"re-install must not rotate the server key")
}

func TestEnsureNATSClientCredsMintsBothSecrets(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	require.NoError(t, ensureNATSIdentityWithClient(ctx, c), "seed identity")
	require.NoError(t, ensureNATSTLSWithClient(ctx, c), "seed tls")

	require.NoError(t, ensureNATSClientCredsWithClient(ctx, c, nil), "mint client creds")

	for _, name := range []string{"spicebox-channelsd-nats-creds", "spicebox-cli-nats-creds", "spicebox-webd-nats-creds"} {
		var sec corev1.Secret
		require.NoError(t, c.Get(ctx, types.NamespacedName{
			Namespace: "agentprimitives-system", Name: name,
		}, &sec), "%s must exist", name)

		assert.NotEmpty(t, sec.Data["nats.creds"], "%s must have nats.creds", name)
		assert.NotEmpty(t, sec.Data["ca.crt"], "%s must have ca.crt", name)
		assert.True(t, strings.Contains(string(sec.Data["nats.creds"]), "BEGIN NATS USER JWT"),
			"%s nats.creds must be a decorated NATS creds file", name)
	}
}

func TestEnsureNATSClientCredsFailsWithoutPrerequisites(t *testing.T) {
	cases := []struct {
		name  string
		setup func(ctx context.Context, c client.Client)
	}{
		{
			name:  "no Secrets at all: identity missing",
			setup: func(_ context.Context, _ client.Client) {},
		},
		{
			name: "only identity seeded: TLS missing",
			setup: func(ctx context.Context, c client.Client) {
				require.NoError(t, ensureNATSIdentityWithClient(ctx, c), "seed identity")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
			tc.setup(ctx, c)
			err := ensureNATSClientCredsWithClient(ctx, c, nil)
			require.Error(t, err, "must fail when prerequisites are not fully seeded")
		})
	}
}

func TestEnsureNATSClientCredsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	require.NoError(t, ensureNATSIdentityWithClient(ctx, c), "seed identity")
	require.NoError(t, ensureNATSTLSWithClient(ctx, c), "seed tls")

	require.NoError(t, ensureNATSClientCredsWithClient(ctx, c, nil), "first call must succeed")

	// Capture creds bytes from both Secrets after the first call.
	var firstChannelsd, firstCLI corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-channelsd-nats-creds",
	}, &firstChannelsd), "channelsd creds Secret must exist after first call")
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-cli-nats-creds",
	}, &firstCLI), "cli creds Secret must exist after first call")

	firstChannelsdCreds := firstChannelsd.Data["nats.creds"]
	firstCLICreds := firstCLI.Data["nats.creds"]
	require.NotEmpty(t, firstChannelsdCreds, "channelsd nats.creds must be populated")
	require.NotEmpty(t, firstCLICreds, "cli nats.creds must be populated")

	require.NoError(t, ensureNATSClientCredsWithClient(ctx, c, nil), "second call must succeed")

	// Assert that neither Secret was modified — stored creds are byte-identical.
	var secondChannelsd, secondCLI corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-channelsd-nats-creds",
	}, &secondChannelsd), "channelsd creds Secret must still exist after second call")
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "agentprimitives-system", Name: "spicebox-cli-nats-creds",
	}, &secondCLI), "cli creds Secret must still exist after second call")

	assert.True(t, bytes.Equal(firstChannelsdCreds, secondChannelsd.Data["nats.creds"]),
		"re-install must not re-mint/rotate the stored channelsd creds")
	assert.True(t, bytes.Equal(firstCLICreds, secondCLI.Data["nats.creds"]),
		"re-install must not re-mint/rotate the stored cli creds")
}
