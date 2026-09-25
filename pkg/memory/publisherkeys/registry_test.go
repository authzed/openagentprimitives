package publisherkeys_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/publisherkeys"
)

// key returns a deterministic Ed25519 public key seeded by b.
func key(b byte) ed25519.PublicKey {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = b
	}
	return ed25519.NewKeyFromSeed(s).Public().(ed25519.PublicKey)
}

// kid is the content-address keyID for key(b) — the only keyID the registry
// accepts for that key (it enforces keyID == provenance.KeyID(pub)).
func kid(b byte) string { return provenance.KeyID(key(b)) }

func TestRegistry_AddLookup(t *testing.T) {
	r := publisherkeys.New()
	kChan, kAuthz := key(1), key(2)
	require.NoError(t, r.Add("system:channelsd", kid(1), kChan))
	require.NoError(t, r.Add("system:authzd", kid(2), kAuthz))

	got, ok := r.PublisherKey("system:channelsd", kid(1))
	require.True(t, ok, "registered channelsd key resolves")
	assert.Equal(t, kChan, got)

	got, ok = r.PublisherKey("system:authzd", kid(2))
	require.True(t, ok)
	assert.Equal(t, kAuthz, got)

	_, ok = r.PublisherKey("system:channelsd", "nope")
	assert.False(t, ok, "unknown keyID misses")
	_, ok = r.PublisherKey("system:unknown", kid(1))
	assert.False(t, ok, "unknown publisher misses")
}

// TestRegistry_Add_RejectsForgedKeyID is the core fail-closed property: a
// keyID that is not the content address of the key is refused, so a forged
// or tampered binding cannot enter the registry.
func TestRegistry_Add_RejectsForgedKeyID(t *testing.T) {
	r := publisherkeys.New()
	err := r.Add("system:channelsd", "deadbeefdeadbeefdeadbeefdeadbeef", key(1))
	require.Error(t, err, "a keyID not matching the key's hash must be rejected")
	_, ok := r.PublisherKey("system:channelsd", "deadbeefdeadbeefdeadbeefdeadbeef")
	assert.False(t, ok, "the rejected key must not be registered")
}

// TestRegistry_Add_Idempotent: re-registering the exact same key under the
// same (publisher, keyID) is a no-op success, not a conflict.
func TestRegistry_Add_Idempotent(t *testing.T) {
	r := publisherkeys.New()
	require.NoError(t, r.Add("system:channelsd", kid(1), key(1)))
	require.NoError(t, r.Add("system:channelsd", kid(1), key(1)), "byte-identical re-add is idempotent")
}

// TestRegistry_Add_RejectsWrongSize: a non-32-byte key is rejected (and
// would otherwise panic ed25519.Verify downstream).
func TestRegistry_Add_RejectsWrongSize(t *testing.T) {
	r := publisherkeys.New()
	require.Error(t, r.Add("system:channelsd", "k", ed25519.PublicKey{1, 2, 3}))
}

func TestRegistry_EntriesLoadRoundTrip(t *testing.T) {
	src := publisherkeys.New()
	require.NoError(t, src.Add("system:channelsd", kid(1), key(1)))
	require.NoError(t, src.Add("system:channelsd", kid(2), key(2))) // rotated/second key
	require.NoError(t, src.Add("system:authzd", kid(3), key(3)))

	snap := src.Entries()
	require.Len(t, snap, 3, "Entries flattens every (publisher,keyID) pair")

	// Round-trip through a fresh registry: every key must resolve identically.
	dst := publisherkeys.New()
	require.Empty(t, dst.Load(snap), "a clean snapshot loads without rejections")
	for _, want := range []struct {
		publisher, keyID string
		pub              ed25519.PublicKey
	}{
		{"system:channelsd", kid(1), key(1)},
		{"system:channelsd", kid(2), key(2)},
		{"system:authzd", kid(3), key(3)},
	} {
		got, ok := dst.PublisherKey(want.publisher, want.keyID)
		require.Truef(t, ok, "loaded %s/%s resolves", want.publisher, want.keyID)
		assert.Equal(t, want.pub, got)
	}
}

// TestRegistry_LoadRejectsBadEntries: a corrupt, wrong-length, or
// content-address-mismatched entry is rejected (returned as an error, never
// silently dropped) while a valid sibling still loads.
func TestRegistry_LoadRejectsBadEntries(t *testing.T) {
	r := publisherkeys.New()
	errs := r.Load([]publisherkeys.Entry{
		{Publisher: "system:channelsd", KeyID: kid(1), PubKeyB64: base64.StdEncoding.EncodeToString(key(1))},
		{Publisher: "system:bad", KeyID: kid(2), PubKeyB64: "not-base64!!"},
		{Publisher: "system:short", KeyID: "k", PubKeyB64: base64.StdEncoding.EncodeToString([]byte{1, 2, 3})},
		// Valid 32-byte key, but keyID does not match its content address.
		{Publisher: "system:forged", KeyID: "ffff", PubKeyB64: base64.StdEncoding.EncodeToString(key(4))},
	})

	assert.Len(t, errs, 3, "three bad entries are reported, not silently skipped")

	got, ok := r.PublisherKey("system:channelsd", kid(1))
	require.True(t, ok, "valid entry installed despite sibling corruption")
	assert.Equal(t, key(1), got)

	_, ok = r.PublisherKey("system:bad", kid(2))
	assert.False(t, ok, "undecodable base64 rejected")
	_, ok = r.PublisherKey("system:short", "k")
	assert.False(t, ok, "wrong-length key rejected")
	_, ok = r.PublisherKey("system:forged", "ffff")
	assert.False(t, ok, "content-address-mismatched key rejected")
}
