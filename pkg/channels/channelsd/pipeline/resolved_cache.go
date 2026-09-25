package pipeline

import (
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// resolvedDecisionCache remembers recently-resolved interaction/approval
// decisions so a late spectator click (after the matching pending entry has
// been cleared) can be informed who actually approved/denied. Keyed by the
// decision's RequestRef; 30-minute TTL — long enough for slow approver UI edits
// (Slack response_url is valid ~30m), short enough that the cache doesn't grow
// unbounded. Consumed by the generic decision pipe (HandleInteractionDecision).
type resolvedDecisionCache struct {
	mu      sync.Mutex
	entries map[string]resolvedDecision
	ttl     time.Duration
}

type resolvedDecision struct {
	Approver   channelevents.ExternalIdentity
	Decision   string
	ResolvedAt time.Time
}

func newResolvedDecisionCache() *resolvedDecisionCache {
	return &resolvedDecisionCache{
		entries: map[string]resolvedDecision{},
		ttl:     30 * time.Minute,
	}
}

// resolvedKey scopes a resolution to the CATEGORY that made it, not to the
// request id alone.
//
// Keyed on the id alone, a resolution recorded by ONE category blocked every
// other category's click on the same id. A participant could therefore post a
// participant-policy decision carrying an owner-policy card's RequestRef,
// resolve nothing of consequence under its own handler, and have the owner's
// real click come back "already_resolved" -- a veto by anyone, on any pending
// interaction, without ever passing that interaction's standing check.
//
// The category witness upstream refuses that substitution for anything that
// leaves a durable record. This closes the same hole for the categories that
// leave none, and does so without needing to enumerate them -- which matters,
// because an earlier comment enumerating them got the list wrong.
//
// Idempotency is unaffected: a second click on the SAME card carries the same
// category, so it still finds the first resolution.
func resolvedKey(category, reqID string) string { return category + "\x00" + reqID }

func (c *resolvedDecisionCache) put(category, reqID string, e resolvedDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[resolvedKey(category, reqID)] = e
	// Best-effort GC: drop expired entries on each put.
	cutoff := time.Now().Add(-c.ttl)
	for k, v := range c.entries {
		if v.ResolvedAt.Before(cutoff) {
			delete(c.entries, k)
		}
	}
}

func (c *resolvedDecisionCache) get(category, reqID string) (resolvedDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := resolvedKey(category, reqID)
	e, ok := c.entries[k]
	if !ok {
		return resolvedDecision{}, false
	}
	if time.Since(e.ResolvedAt) > c.ttl {
		delete(c.entries, k)
		return resolvedDecision{}, false
	}
	return e, true
}
