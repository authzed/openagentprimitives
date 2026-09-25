package icons

import (
	"sync"
	"time"
)

// CacheConfig wires a Cache's behavior. Every field has a NewCache default;
// NowFn nil means time.Now, and tests inject a fake clock there.
type CacheConfig struct {
	Cap    int
	PosTTL time.Duration
	NegTTL time.Duration
	NowFn  func() time.Time
}

// Cache is a concurrent-safe in-process LRU keyed by credName, bounded by Cap.
// Entries expire after PosTTL, or NegTTL when they hold a fallback icon.
type Cache struct {
	mu     sync.RWMutex
	items  map[string]*Entry
	order  []string // recency: index 0 = LRU, len-1 = MRU
	cap    int
	posTTL time.Duration
	negTTL time.Duration
	now    func() time.Time
}

// NewCache constructs a Cache from CacheConfig.
func NewCache(cfg CacheConfig) *Cache {
	if cfg.Cap <= 0 {
		cfg.Cap = 256
	}
	if cfg.PosTTL <= 0 {
		cfg.PosTTL = 24 * time.Hour
	}
	if cfg.NegTTL <= 0 {
		cfg.NegTTL = time.Hour
	}
	now := cfg.NowFn
	if now == nil {
		now = time.Now
	}
	return &Cache{
		items:  make(map[string]*Entry, cfg.Cap),
		order:  make([]string, 0, cfg.Cap),
		cap:    cfg.Cap,
		posTTL: cfg.PosTTL,
		negTTL: cfg.NegTTL,
		now:    now,
	}
}

// Get returns the cached Entry for credName, or (zero, false) on miss or
// expiry. A hit becomes most-recently-used, so this takes the write lock.
func (c *Cache) Get(credName string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[credName]
	if !ok {
		return Entry{}, false
	}
	ttl := c.posTTL
	if e.Negative {
		ttl = c.negTTL
	}
	if c.now().Sub(e.FetchedAt) > ttl {
		// Lazy expiry: evict on access.
		c.removeLocked(credName)
		return Entry{}, false
	}
	c.touchLocked(credName)
	return *e, true
}

// Put stores an Entry, stamping FetchedAt to now and evicting the LRU entry
// when the cache is at cap. Replacing an entry resets it to MRU.
func (c *Cache) Put(credName string, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.FetchedAt = c.now()
	if _, ok := c.items[credName]; ok {
		c.items[credName] = &e
		c.touchLocked(credName)
		return
	}
	if len(c.items) >= c.cap && len(c.order) > 0 {
		lru := c.order[0]
		c.removeLocked(lru)
	}
	c.items[credName] = &e
	c.order = append(c.order, credName)
}

func (c *Cache) removeLocked(credName string) {
	delete(c.items, credName)
	for i, k := range c.order {
		if k == credName {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func (c *Cache) touchLocked(credName string) {
	for i, k := range c.order {
		if k == credName {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, credName)
			return
		}
	}
}
