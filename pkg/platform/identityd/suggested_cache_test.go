package identityd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSuggestedCache_HitMissExpiry(t *testing.T) {
	c := newSuggestedCache(50 * time.Millisecond)

	// Miss on first lookup.
	got, ok := c.get("k1")
	assert.False(t, ok)
	assert.Nil(t, got)

	// Put + immediate get → hit.
	c.put("k1", []portalCredential{{Name: "github-pat", Label: "GitHub"}})
	got, ok = c.get("k1")
	assert.True(t, ok)
	assert.Equal(t, []portalCredential{{Name: "github-pat", Label: "GitHub"}}, got)

	// Different key → miss.
	_, ok = c.get("k2")
	assert.False(t, ok)

	// Wait past TTL → expired → miss.
	time.Sleep(60 * time.Millisecond)
	_, ok = c.get("k1")
	assert.False(t, ok, "expired entry must not be returned")
}

func TestKeyForAlreadyLinked_OrderIndependent(t *testing.T) {
	a := keyForAlreadyLinked(map[string]bool{"github-pat": true, "linear-oauth": true})
	b := keyForAlreadyLinked(map[string]bool{"linear-oauth": true, "github-pat": true})
	assert.Equal(t, a, b, "map iteration order must not affect the cache key")
}

func TestKeyForAlreadyLinked_DistinctSetsDistinctKeys(t *testing.T) {
	a := keyForAlreadyLinked(map[string]bool{"github-pat": true})
	b := keyForAlreadyLinked(map[string]bool{"linear-oauth": true})
	c := keyForAlreadyLinked(map[string]bool{"github-pat": true, "linear-oauth": true})
	d := keyForAlreadyLinked(map[string]bool{})
	assert.NotEqual(t, a, b)
	assert.NotEqual(t, a, c)
	assert.NotEqual(t, b, c)
	assert.NotEqual(t, c, d)
	assert.NotEqual(t, a, d)
}

func TestKeyForAlreadyLinked_PrefixCollisionSafe(t *testing.T) {
	// Without a delimiter, {"ab", "cd"} and {"abc", "d"} could hash to
	// the same key. The delimiter byte in keyForAlreadyLinked prevents
	// that.
	a := keyForAlreadyLinked(map[string]bool{"ab": true, "cd": true})
	b := keyForAlreadyLinked(map[string]bool{"abc": true, "d": true})
	assert.NotEqual(t, a, b, "delimiter must prevent prefix collisions")
}
