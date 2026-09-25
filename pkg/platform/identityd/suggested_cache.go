// pkg/platform/identityd/suggested_cache.go — TTL cache for the suggested-
// credentials aggregator.
//
// The cluster-wide List of AgentClasses + MCPServers is expensive relative to a
// portal pageview. Staleness is cheap: the worst case is the user sees a
// credential they just linked and clicks "Link" again, which PutToken's
// idempotent upsert absorbs.
//
// Per-process, drained on TTL, keyed by a hash of the already-linked set — so
// two users with the same set share an entry.
package identityd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// suggestedCacheTTL bounds how long a cached suggestion-list is reused: short
// enough that a newly-added AgentClass reaches the portal within a minute, long
// enough that a run of pageviews shares one List call.
const suggestedCacheTTL = 1 * time.Minute

// suggestedCache memoizes suggestedCredentialsForUser results keyed by the
// already-linked-set hash. Concurrent-safe; entries past TTL drop on read.
type suggestedCache struct {
	mu      sync.Mutex
	entries map[string]suggestedCacheEntry
	ttl     time.Duration
}

type suggestedCacheEntry struct {
	value    []portalCredential
	cachedAt time.Time
}

func newSuggestedCache(ttl time.Duration) *suggestedCache {
	return &suggestedCache{
		entries: map[string]suggestedCacheEntry{},
		ttl:     ttl,
	}
}

// get returns the cached value for key, or (nil, false) on miss or expiry.
func (c *suggestedCache) get(key string) ([]portalCredential, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.cachedAt) > c.ttl {
		delete(c.entries, key)
		return nil, false
	}
	return e.value, true
}

// put stores a value under the key with the current time as cachedAt.
func (c *suggestedCache) put(key string, value []portalCredential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = suggestedCacheEntry{value: value, cachedAt: time.Now()}
}

// keyForAlreadyLinked produces a deterministic cache key for an already-linked
// set: sorted names, SHA-256'd to keep the key short and stable.
func keyForAlreadyLinked(alreadyLinked map[string]bool) string {
	names := make([]string, 0, len(alreadyLinked))
	for n := range alreadyLinked {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0}) // delimiter to prevent prefix collisions
	}
	return hex.EncodeToString(h.Sum(nil))
}

// suggestedCredentialsForUserCached is suggestedCredentialsForUser behind the
// Server's per-process TTL cache. Request paths use this, not the bare form.
func (s *Server) suggestedCredentialsForUserCached(
	ctx context.Context,
	alreadyLinked map[string]bool,
	logger logr.Logger,
) ([]portalCredential, error) {
	key := keyForAlreadyLinked(alreadyLinked)
	if cached, ok := s.suggestedCache.get(key); ok {
		return cached, nil
	}
	fresh, err := suggestedCredentialsForUser(ctx, s.deps.K8s, alreadyLinked, logger)
	if err != nil {
		return nil, err
	}
	// The cache and the caller share one slice here, and a hit hands back the
	// cached slice directly — so every caller must treat the result as
	// read-only.
	cached := make([]portalCredential, len(fresh))
	copy(cached, fresh)
	s.suggestedCache.put(key, cached)
	return cached, nil
}
