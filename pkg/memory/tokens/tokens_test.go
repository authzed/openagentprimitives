package tokens_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

func TestRegistryRoundTrip(t *testing.T) {
	r := tokens.NewRegistry()
	k := memory.NamespacedName{Namespace: "default", Name: "s1"}
	r.Set(k, "tok-1", "")

	got, ok := r.Lookup("tok-1")
	require.True(t, ok, "Lookup(tok-1) should hit")
	assert.Equal(t, k, got)

	_, ok = r.Lookup("nope")
	assert.False(t, ok, "unknown token should not match")
}

func TestRegistryRevoke(t *testing.T) {
	r := tokens.NewRegistry()
	k := memory.NamespacedName{Namespace: "default", Name: "s1"}
	r.Set(k, "tok-1", "")
	r.Revoke(k)
	_, ok := r.Lookup("tok-1")
	assert.False(t, ok, "revoked token should not match")
}

func TestRegistryRotate(t *testing.T) {
	r := tokens.NewRegistry()
	k := memory.NamespacedName{Namespace: "default", Name: "s1"}
	r.Set(k, "tok-1", "")
	r.Set(k, "tok-2", "") // rotation: replaces tok-1
	_, ok := r.Lookup("tok-1")
	assert.False(t, ok, "old token should be invalidated after rotation")
	got, ok := r.Lookup("tok-2")
	require.True(t, ok, "new token should resolve")
	assert.Equal(t, k, got)
}

func TestRegistryLookupInfo(t *testing.T) {
	r := tokens.NewRegistry()
	k := memory.NamespacedName{Namespace: "default", Name: "s1"}
	r.Set(k, "tok-1", "user-abc")

	info, ok := r.LookupInfo("tok-1")
	require.True(t, ok)
	assert.Equal(t, k, info.Session)
	assert.Equal(t, "user-abc", info.CallerID)

	_, ok = r.LookupInfo("nope")
	assert.False(t, ok)
}

func TestRegistrySet_CallerIDRotation(t *testing.T) {
	r := tokens.NewRegistry()
	k := memory.NamespacedName{Namespace: "default", Name: "s1"}
	r.Set(k, "tok-1", "user-old")
	r.Set(k, "tok-2", "user-new")

	_, ok := r.LookupInfo("tok-1")
	assert.False(t, ok, "old token should be invalidated")

	info, ok := r.LookupInfo("tok-2")
	require.True(t, ok)
	assert.Equal(t, "user-new", info.CallerID)
}

func TestChannelsdToken(t *testing.T) {
	r := tokens.NewRegistry()
	assert.False(t, r.IsChannelsdToken("anything"), "registry should not accept anything when channelsd token unset")
	r.SetChannelsdToken("sys")
	assert.True(t, r.IsChannelsdToken("sys"), "set token should be accepted")
	assert.False(t, r.IsChannelsdToken("wrong"), "wrong token should not be accepted")
	assert.False(t, r.IsChannelsdToken(""), "empty token should not be accepted")
	// Setting empty disables.
	r.SetChannelsdToken("")
	assert.False(t, r.IsChannelsdToken(""), "after disable, empty should not be accepted")
	assert.False(t, r.IsChannelsdToken("sys"), "after disable, previously-set token should not be accepted")
}

func TestRegistry_WebdToken(t *testing.T) {
	r := tokens.NewRegistry()
	assert.False(t, r.IsWebdToken("anything"), "unset webd token accepts nothing")
	r.SetWebdToken("webd-secret")
	assert.True(t, r.IsWebdToken("webd-secret"))
	assert.False(t, r.IsWebdToken("wrong"))
	assert.False(t, r.IsWebdToken(""), "empty never accepted even when set")
	// Distinct from the other system tokens (no cross-acceptance).
	assert.False(t, r.IsChannelsdToken("webd-secret"), "webd token is not a channelsd token")
	assert.False(t, r.IsAuthzdToken("webd-secret"), "webd token is not an authzd token")
	// Empty disables.
	r.SetWebdToken("")
	assert.False(t, r.IsWebdToken("webd-secret"), "previously-set token rejected after disable")
}

func TestRegistry_AuthzdToken(t *testing.T) {
	r := tokens.NewRegistry()
	assert.False(t, r.IsAuthzdToken("anything"), "registry should not accept anything when authzd token unset")
	r.SetAuthzdToken("authzd-secret")
	assert.True(t, r.IsAuthzdToken("authzd-secret"), "set token should be accepted")
	assert.False(t, r.IsAuthzdToken("wrong"), "wrong token should not be accepted")
	assert.False(t, r.IsAuthzdToken(""), "empty token should not be accepted even when token is set")
}

func TestRegistry_AuthzdToken_EmptyDisables(t *testing.T) {
	r := tokens.NewRegistry()
	r.SetAuthzdToken("")
	assert.False(t, r.IsAuthzdToken(""), "empty string should not be accepted when disabled")
	// Set a real token, then disable again.
	r.SetAuthzdToken("authzd-secret")
	r.SetAuthzdToken("")
	assert.False(t, r.IsAuthzdToken("authzd-secret"), "previously-set token should not be accepted after disable")
}

// pubKey returns a deterministic Ed25519 public key seeded by b.
func pubKey(b byte) ed25519.PublicKey {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = b
	}
	return ed25519.NewKeyFromSeed(s).Public().(ed25519.PublicKey)
}

func TestRegistry_PublisherKey(t *testing.T) {
	const pubA = "session:ns/a"
	keyA1, keyA2, keyB1 := pubKey(1), pubKey(2), pubKey(3)

	r := tokens.NewRegistry()
	r.SetPublisherKey(pubA, "k1", keyA1)
	r.SetPublisherKey(pubA, "k2", keyA2) // second key for same publisher
	r.SetPublisherKey("session:ns/b", "k1", keyB1)

	cases := []struct {
		name           string
		publisher, key string
		want           ed25519.PublicKey
		wantOK         bool
	}{
		{name: "first key for publisher A: found", publisher: pubA, key: "k1", want: keyA1, wantOK: true},
		{name: "second key for publisher A: found", publisher: pubA, key: "k2", want: keyA2, wantOK: true},
		{name: "publisher B's key is isolated: found", publisher: "session:ns/b", key: "k1", want: keyB1, wantOK: true},
		{name: "unknown keyID for known publisher: miss", publisher: pubA, key: "nope", wantOK: false},
		{name: "unknown publisher: miss", publisher: "session:ns/z", key: "k1", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := r.PublisherKey(tc.publisher, tc.key)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}
