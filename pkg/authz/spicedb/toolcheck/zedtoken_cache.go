package toolcheck

import "sync"

// ZedTokenCache is a per-session in-memory map from
// `<resourceType>/<resourceID>` to the most recent ZedToken seen for
// that resource. It also tracks the most recent Set across all
// resources for use as an at-least-as-fresh-as floor when no
// resource-specific token is known.
//
// In-memory only — runner restarts lose the cache; the runner falls
// back to MinimizeLatency until fresh tokens accumulate.
//
// Safe for concurrent use; tool-call dispatch fans out across
// goroutines.
type ZedTokenCache struct {
	mu        sync.RWMutex
	perRes    map[string]string
	latestTok string
}

// NewZedTokenCache returns a fresh cache.
func NewZedTokenCache() *ZedTokenCache {
	return &ZedTokenCache{perRes: map[string]string{}}
}

// Set records tok as the most recent ZedToken for (resourceType,
// resourceID) and updates Latest().
func (c *ZedTokenCache) Set(resourceType, resourceID, tok string) {
	if tok == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.perRes[resourceType+"/"+resourceID] = tok
	c.latestTok = tok
}

// Get returns the most recent ZedToken seen for (resourceType,
// resourceID), or "" when none has been recorded.
func (c *ZedTokenCache) Get(resourceType, resourceID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.perRes[resourceType+"/"+resourceID]
}

// Latest returns the most recent ZedToken Set against this cache,
// regardless of resource. Used as an at-least-as-fresh-as floor for
// reads on resources we have no token for.
func (c *ZedTokenCache) Latest() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestTok
}
