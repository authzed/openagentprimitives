package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

func TestEnsurePassthroughLinkKey_CreatesUsableWhenAbsent(t *testing.T) {
	c := newFakeClient()
	minted, err := EnsurePassthroughLinkKey(context.Background(), c, c, nil)
	require.NoError(t, err)
	assert.True(t, minted, "a fresh key is minted")

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: TokenSecretNamespace, Name: v1alpha1.PassthroughLinkSigningKeySecret}, &got))
	_, usable := passthroughlink.DecodeHexKey(got.Data["key"])
	assert.NoError(t, usable, "minted key passes DecodeHexKey")
}

func TestEnsurePassthroughLinkKey_LeavesUsableUntouched(t *testing.T) {
	good := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") // 64 hex = 32 bytes
	_, err := passthroughlink.DecodeHexKey(good)
	require.NoError(t, err, "fixture key must be usable")
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PassthroughLinkSigningKeySecret, Namespace: TokenSecretNamespace},
		Data:       map[string][]byte{"key": good},
	}
	c := newFakeClient(existing)
	minted, err := EnsurePassthroughLinkKey(context.Background(), c, c, nil)
	require.NoError(t, err)
	assert.False(t, minted, "a usable key is never rotated")

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: TokenSecretNamespace, Name: v1alpha1.PassthroughLinkSigningKeySecret}, &got))
	assert.Equal(t, string(good), string(got.Data["key"]))
}

func TestEnsurePassthroughLinkKey_RepairsUnusable(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PassthroughLinkSigningKeySecret, Namespace: TokenSecretNamespace},
		Data:       map[string][]byte{"key": []byte("too-short")}, // not hex / too short
	}
	c := newFakeClient(existing)
	minted, err := EnsurePassthroughLinkKey(context.Background(), c, c, nil)
	require.NoError(t, err)
	assert.True(t, minted, "an unusable key is repaired (reported as minted so consumers roll)")

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: TokenSecretNamespace, Name: v1alpha1.PassthroughLinkSigningKeySecret}, &got))
	_, usable := passthroughlink.DecodeHexKey(got.Data["key"])
	assert.NoError(t, usable, "repaired key is usable")
}
