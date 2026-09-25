package runner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fixedClock returns a now func pinned to t, so AllowN's token-bucket refill
// never advances — the exact behavior TestAppToolRateLimiter_Allow needs to
// deterministically exhaust a burst without a real sleep.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestAppToolRateLimiter_Allow(t *testing.T) {
	fixed := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	t.Run("configured origin at limit N=2: first 2 allow, 3rd denies (clock held fixed)", func(t *testing.T) {
		lim := NewAppToolRateLimiter(map[string]int32{"mcpserver/widgets": 2}, fixedClock(fixed))

		assert.True(t, lim.Allow("mcpserver/widgets"), "1st call within burst")
		assert.True(t, lim.Allow("mcpserver/widgets"), "2nd call within burst")
		assert.False(t, lim.Allow("mcpserver/widgets"), "3rd call exceeds the N=2 burst before the clock advances")
	})

	t.Run("unconfigured origin: always allowed", func(t *testing.T) {
		lim := NewAppToolRateLimiter(map[string]int32{"mcpserver/widgets": 1}, fixedClock(fixed))

		for i := 0; i < 5; i++ {
			assert.True(t, lim.Allow("mcpserver/other"), "an origin with no configured limit is unlimited")
		}
	})

	t.Run("nil receiver: always allowed", func(t *testing.T) {
		var lim *AppToolRateLimiter
		assert.True(t, lim.Allow("mcpserver/widgets"), "a nil limiter must never block (unlimited default)")
	})

	t.Run("n<=0 is omitted (unlimited)", func(t *testing.T) {
		lim := NewAppToolRateLimiter(map[string]int32{"mcpserver/zero": 0, "mcpserver/neg": -1}, fixedClock(fixed))

		for i := 0; i < 5; i++ {
			assert.True(t, lim.Allow("mcpserver/zero"), "n=0 must be treated as unlimited, not always-deny")
			assert.True(t, lim.Allow("mcpserver/neg"), "a negative configured value must be treated as unlimited")
		}
	})

	t.Run("nil now func defaults to time.Now (does not panic)", func(t *testing.T) {
		lim := NewAppToolRateLimiter(map[string]int32{"mcpserver/widgets": 3}, nil)
		assert.True(t, lim.Allow("mcpserver/widgets"))
	})
}
