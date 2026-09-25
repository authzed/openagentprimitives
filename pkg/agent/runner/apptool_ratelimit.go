package runner

import (
	"time"

	"golang.org/x/time/rate"
)

// AppToolRateLimiter caps autonomous app-tool calls per MCPServer origin
// (mcpUiAppTools.maxCallsPerMin). It is enforced ONLY on the app-tool path
// (HandleAppToolCall), deliberately NOT via toolguard — so a server's circuit
// breaker and its LLM-visible tools' guards are untouched. Origins without a
// configured limit are unlimited. Concurrency-safe (rate.Limiter is; the map
// is built once and read-only).
type AppToolRateLimiter struct {
	limiters map[string]*rate.Limiter // origin ("mcpserver/<name>") → limiter
	now      func() time.Time
}

// NewAppToolRateLimiter builds a limiter from origin→maxCallsPerMin. Origins
// with n<=0 are omitted (unlimited). now defaults to time.Now when nil.
func NewAppToolRateLimiter(perMinByOrigin map[string]int32, now func() time.Time) *AppToolRateLimiter {
	if now == nil {
		now = time.Now
	}
	m := make(map[string]*rate.Limiter, len(perMinByOrigin))
	for origin, n := range perMinByOrigin {
		if n <= 0 {
			continue
		}
		// n per minute, burst n (a full minute's allowance can arrive at once, then refills over the minute).
		m[origin] = rate.NewLimiter(rate.Limit(float64(n)/60.0), int(n))
	}
	return &AppToolRateLimiter{limiters: m, now: now}
}

// Allow reports whether a call to origin is within budget now. A nil receiver,
// or an origin with no configured limit, always allows.
func (r *AppToolRateLimiter) Allow(origin string) bool {
	if r == nil {
		return true
	}
	lim, ok := r.limiters[origin]
	if !ok {
		return true
	}
	return lim.AllowN(r.now(), 1)
}
