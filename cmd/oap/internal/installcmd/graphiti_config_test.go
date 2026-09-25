package installcmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

func TestEnsureGraphitiConfigWithClient_CreatesSecretFromEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-real-key")
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	configured, err := ensureGraphitiConfigWithClient(context.Background(), &bytes.Buffer{}, c)
	require.NoError(t, err)
	assert.True(t, configured)

	sec := getSecret(t, c, "spicebox-graphiti-config")
	assert.Equal(t, "sk-real-key", string(sec.Data["openai-api-key"]))
}

func TestEnsureGraphitiConfigWithClient_WarnsAndCreatesEmptySecretWhenEnvUnset(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	var out bytes.Buffer

	configured, err := ensureGraphitiConfigWithClient(context.Background(), &out, c)
	require.NoError(t, err)
	assert.False(t, configured)

	sec := getSecret(t, c, "spicebox-graphiti-config")
	assert.Empty(t, string(sec.Data["openai-api-key"]))
	assert.Contains(t, out.String(), "OPENAI_API_KEY not set")
}

// TestEnsureGraphitiConfigWithClient_RefreshesStaleEmptyKeyWhenEnvNowSet is the
// regression test for the bug: a first install without OPENAI_API_KEY left the
// Secret's key permanently empty because "the Secret already exists" was
// treated as sufficient, exactly the failure mode ensureNATSCredsSecret's own
// doc comment warns about. A later install exporting the key correctly must
// actually reach the cluster.
func TestEnsureGraphitiConfigWithClient_RefreshesStaleEmptyKeyWhenEnvNowSet(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-fixed-key")
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-graphiti-config"},
			Data:       map[string][]byte{"openai-api-key": []byte("")},
		},
	).Build()

	configured, err := ensureGraphitiConfigWithClient(context.Background(), &bytes.Buffer{}, c)
	require.NoError(t, err)
	assert.True(t, configured, "a re-install with the key now set must report graphiti as configured")

	sec := getSecret(t, c, "spicebox-graphiti-config")
	assert.Equal(t, "sk-fixed-key", string(sec.Data["openai-api-key"]),
		"the stale empty key must be refreshed from the current environment")
}

func TestEnsureGraphitiConfigWithClient_LeavesExistingKeyAloneWhenEnvUnset(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-graphiti-config"},
			Data:       map[string][]byte{"openai-api-key": []byte("sk-already-good")},
		},
	).Build()

	configured, err := ensureGraphitiConfigWithClient(context.Background(), &bytes.Buffer{}, c)
	require.NoError(t, err)
	assert.True(t, configured)

	sec := getSecret(t, c, "spicebox-graphiti-config")
	assert.Equal(t, "sk-already-good", string(sec.Data["openai-api-key"]),
		"an unset local env must never clobber a good key already stored")
}

func TestEnsureGraphitiConfigWithClient_NoopWhenKeyAlreadyMatches(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-same-key")
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-graphiti-config"},
			Data:       map[string][]byte{"openai-api-key": []byte("sk-same-key")},
		},
	).Build()
	before := getSecret(t, c, "spicebox-graphiti-config")

	configured, err := ensureGraphitiConfigWithClient(context.Background(), &bytes.Buffer{}, c)
	require.NoError(t, err)
	assert.True(t, configured)

	after := getSecret(t, c, "spicebox-graphiti-config")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"a matching key must not trigger a write")
}
