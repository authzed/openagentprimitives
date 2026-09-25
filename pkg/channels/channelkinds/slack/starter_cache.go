package slack

import (
	"sync"
)

// starterCoords are the coordinates of a thread's "started" bootstrap message,
// recorded by the listener when it posts the starter and read by the
// thread_title sender when it edits the starter in place. AgentName +
// StartedUnix let the title sender reproduce the exact "started <!date…>"
// clause (with the original timestamp) when it prepends the title.
type starterCoords struct {
	ChannelID   string
	MessageTS   string
	AgentName   string
	StartedUnix int64
}

// starterCache is a per-process, best-effort map of session key
// (ns/name) → starterCoords. Shared on the *Kind singleton between the
// listener (writer) and the thread_title sender (reader). Lost on a
// channelsd restart, which is acceptable: titles then no-op in channels.
type starterCache struct {
	mu sync.Mutex
	m  map[string]starterCoords
}

func newStarterCache() *starterCache { return &starterCache{m: map[string]starterCoords{}} }

func (c *starterCache) put(key string, v starterCoords) {
	c.mu.Lock()
	c.m[key] = v
	c.mu.Unlock()
}

func (c *starterCache) get(key string) (starterCoords, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}
