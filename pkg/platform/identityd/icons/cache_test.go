package icons

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCache_GetMiss(t *testing.T) {
	c := NewCache(CacheConfig{Cap: 10, PosTTL: time.Hour, NegTTL: time.Minute})
	_, ok := c.Get("nope")
	assert.False(t, ok)
}

func TestCache_PutGetHit(t *testing.T) {
	c := NewCache(CacheConfig{Cap: 10, PosTTL: time.Hour, NegTTL: time.Minute})
	e := Entry{Bytes: []byte("png-bytes"), ContentType: "image/png", ETag: `"abc"`, Negative: false}
	c.Put("linear", e)
	got, ok := c.Get("linear")
	require.True(t, ok)
	assert.Equal(t, e.Bytes, got.Bytes)
	assert.Equal(t, e.ContentType, got.ContentType)
	assert.Equal(t, e.ETag, got.ETag)
	assert.False(t, got.Negative)
}

func TestCache_PositiveExpiry(t *testing.T) {
	now := time.Now()
	c := NewCache(CacheConfig{Cap: 10, PosTTL: 100 * time.Millisecond, NegTTL: time.Hour, NowFn: func() time.Time { return now }})
	c.Put("linear", Entry{Bytes: []byte("x"), ContentType: "image/png"})
	// advance the clock 200ms
	now = now.Add(200 * time.Millisecond)
	_, ok := c.Get("linear")
	assert.False(t, ok, "expired entry must miss")
}

func TestCache_NegativeExpiry(t *testing.T) {
	now := time.Now()
	c := NewCache(CacheConfig{Cap: 10, PosTTL: time.Hour, NegTTL: 100 * time.Millisecond, NowFn: func() time.Time { return now }})
	c.Put("nope", Entry{Bytes: []byte("svg"), ContentType: "image/svg+xml", Negative: true})
	now = now.Add(50 * time.Millisecond)
	_, ok := c.Get("nope")
	require.True(t, ok, "negative entry within TTL must hit")
	now = now.Add(200 * time.Millisecond)
	_, ok = c.Get("nope")
	assert.False(t, ok, "negative entry past TTL must miss")
}

func TestCache_LRUEvictionAtCap(t *testing.T) {
	c := NewCache(CacheConfig{Cap: 2, PosTTL: time.Hour, NegTTL: time.Minute})
	c.Put("a", Entry{Bytes: []byte("a")})
	c.Put("b", Entry{Bytes: []byte("b")})
	// Touch "a" to mark it MRU
	_, _ = c.Get("a")
	// Adding "c" should evict "b" (LRU)
	c.Put("c", Entry{Bytes: []byte("c")})
	_, aOk := c.Get("a")
	_, bOk := c.Get("b")
	_, cOk := c.Get("c")
	assert.True(t, aOk, "a was touched, must remain")
	assert.False(t, bOk, "b was LRU at insert time, must be evicted")
	assert.True(t, cOk, "c just inserted, must remain")
}
