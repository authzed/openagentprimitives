// pkg/channels/channelkinds/slack/identity.go
//
// Identity helpers for Slack: email→canonical_id derivation per the spec
// (base64url(lowercase(email)), with fallback to base64url("slack:<team>:<user>")
// when email is unresolvable). Plus a small LRU cache keyed by Slack user_id
// to avoid hitting users.info on every inbound event from a returning user.
package slack

import (
	"container/list"
	"encoding/base64"
	"strings"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// CanonicalID computes the SpiceDB canonical user_id for a Slack user.
// Email is preferred (allows cross-channel identity correlation); fallback
// is "slack:<team_id>:<user_id>" when the email isn't resolvable.
//
// Result is base64url-no-padding so the value is SpiceDB ObjectId-safe
// and won't collide with our agentsession:<ns>/<name> separator.
func CanonicalID(email, teamID, userID string) string {
	enc := base64.RawURLEncoding
	if email != "" {
		return enc.EncodeToString([]byte(strings.ToLower(email)))
	}
	return enc.EncodeToString([]byte("slack:" + teamID + ":" + userID))
}

// userInfo is what we cache for each Slack user_id.
type userInfo struct {
	UserID      string
	Email       string
	TeamID      string
	DisplayName string
	// Membership is the org standing derived from the same users.info
	// response the rest of the entry came from. Empty (an entry cached by
	// a path that never classified the user) is treated as guest on read.
	Membership channelkinds.OrgMembership
}

// IdentityCache is a small thread-safe LRU keyed by Slack user_id.
// Used by the listener to avoid hitting users.info on every event from
// a returning user.
type IdentityCache struct {
	mu      sync.Mutex
	max     int
	byID    map[string]*list.Element
	byEmail map[string]*list.Element // key = strings.ToLower(email); same Elements as byID
	order   *list.List
}

// NewIdentityCache creates a cache with the given max size. Capacity 0
// disables caching (Get always misses; Put is a no-op).
func NewIdentityCache(max int) *IdentityCache {
	return &IdentityCache{
		max:     max,
		byID:    map[string]*list.Element{},
		byEmail: map[string]*list.Element{},
		order:   list.New(),
	}
}

// Get returns the cached userInfo for userID, or (nil, false) on miss.
// Hit moves the entry to the front of the LRU.
func (c *IdentityCache) Get(userID string) (userInfo, bool) {
	if c.max <= 0 {
		return userInfo{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byID[userID]
	if !ok {
		return userInfo{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(userInfo), true
}

// Put inserts the userInfo into the cache, evicting the LRU entry if at
// capacity. No-op if max <= 0.
func (c *IdentityCache) Put(info userInfo) {
	if c.max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byID[info.UserID]; ok {
		oldEmail := strings.ToLower(el.Value.(userInfo).Email)
		newEmail := strings.ToLower(info.Email)
		if oldEmail != "" && oldEmail != newEmail {
			delete(c.byEmail, oldEmail)
		}
		el.Value = info
		c.order.MoveToFront(el)
		if newEmail != "" {
			c.byEmail[newEmail] = el
		}
		return
	}
	el := c.order.PushFront(info)
	c.byID[info.UserID] = el
	if e := strings.ToLower(info.Email); e != "" {
		c.byEmail[e] = el
	}
	for c.order.Len() > c.max {
		old := c.order.Back()
		if old == nil {
			break
		}
		c.order.Remove(old)
		oldInfo := old.Value.(userInfo)
		delete(c.byID, oldInfo.UserID)
		if e := strings.ToLower(oldInfo.Email); e != "" {
			delete(c.byEmail, e)
		}
	}
}

// GetByEmail returns the cached userInfo whose email matches (case-
// insensitively), or (zero, false) on miss. Hit moves the entry to the
// front of the LRU.
func (c *IdentityCache) GetByEmail(email string) (userInfo, bool) {
	if c.max <= 0 || email == "" {
		return userInfo{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byEmail[strings.ToLower(email)]
	if !ok {
		return userInfo{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(userInfo), true
}
