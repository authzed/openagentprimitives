package slack

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// Proactive client-side pacing for the directory sync's Slack Web API calls.
//
// pkg/controllers/relationshipsource only ever paced REACTIVELY: it waits out
// a Retry-After AFTER Slack has already refused a call, and only BETWEEN
// passes (see that package's passPacer). WITHIN a pass, ListScopes/FetchScope
// used to fire conversations.list/.info/.members, auth.test and one users.info
// per member back-to-back with no spacing at all — so a workspace with more
// than a tier's worth of channels blew straight through Slack's per-method
// limit every cycle, failing a shifting subset of scopes with
// "rate limit exceeded, retry after 10s" and raising a PartialFailure
// condition that never cleared because the next cycle re-hit the same wall.
//
// This file adds the missing proactive half: every Slack call is paced under
// its method's rate-limit tier before it is made, so the 429s do not happen
// in the first place. It mirrors what the live listener already does for its
// own Slack calls (lookup.go's memberSnapshotTTL, sized off "users.list is
// Tier 2; 20/min").

// Slack's Web API rate-limit tiers, expressed per second and sized just UNDER
// Slack's documented per-minute ceilings so a pass stays below the throttle
// rather than discovering the limit by eating a 429. The published tiers are
// roughly Tier 2 ~20/min, Tier 3 ~50/min, Tier 4 ~100/min
// (https://api.slack.com/apis/rate-limits); each value below leaves ~10%
// headroom for clock skew and Slack's own accounting granularity.
const (
	tier2PerSecond = 18.0 / 60.0 // conversations.list
	tier3PerSecond = 45.0 / 60.0 // conversations.info
	tier4PerSecond = 90.0 / 60.0 // conversations.members, users.info, auth.test
)

// directoryRateLimitBurst is the token-bucket burst applied to every method.
// One means strict pacing: two calls of the same method are never emitted
// closer together than the tier allows, so we cannot momentarily exceed the
// ceiling even for a single request. A background directory sync has a whole
// cycle to make progress and no reason to burst, and bursting is the one way
// a proactive limiter can still trip the very 429 it exists to avoid.
const directoryRateLimitBurst = 1

// slackMethod names one Slack Web API method and the client-side rate it is
// paced at. Slack throttles PER METHOD, not per tier — two Tier-3 methods
// have independent budgets — so each method carries its own limit and gets
// its own token bucket. Modelling one shared "Tier 3 bucket" would throttle
// unrelated methods against each other and converge slower for no safety
// gain, since Slack would never have counted them together anyway.
type slackMethod struct {
	name  string
	limit rate.Limit
}

// The methods ListScopes/FetchScope call, each at its Slack tier. auth.test
// shares Tier 4 with the members/users.info calls only in the sense that its
// bucket is sized the same; it is still its own independent bucket.
var (
	methodConversationsList    = slackMethod{name: "conversations.list", limit: rate.Limit(tier2PerSecond)}
	methodConversationsInfo    = slackMethod{name: "conversations.info", limit: rate.Limit(tier3PerSecond)}
	methodConversationsMembers = slackMethod{name: "conversations.members", limit: rate.Limit(tier4PerSecond)}
	methodUsersInfo            = slackMethod{name: "users.info", limit: rate.Limit(tier4PerSecond)}
	methodAuthTest             = slackMethod{name: "auth.test", limit: rate.Limit(tier4PerSecond)}
)

// directoryRateLimiter waits out the client-side rate limit for one Slack
// method before a call to it is made, keyed per workspace token so two
// workspaces never share a bucket.
type directoryRateLimiter interface {
	wait(ctx context.Context, fingerprint string, m slackMethod) error
}

// directoryPacer is the process-wide pacer every ListScopes/FetchScope call
// site consults. It is package-global rather than a *SyncKind field for the
// same reason userInfoCache's scoping key is a token fingerprint: the
// registered production kind is itself a package-global singleton
// (relsync.Register(&SyncKind{}), see relsync_kind.go) whose zero value must
// already pace, while the package's tests build their own zero-value
// &SyncKind{} and must NOT be throttled by real per-minute tiers. The seam
// that disables pacing therefore has to sit somewhere both share.
// startDirectoryTestServer swaps in a no-op for the life of a test, exactly
// the shape directoryClientFactory already uses.
var directoryPacer directoryRateLimiter = newTierLimiter()

// directoryRateLimiterMax bounds the live bucket set — one entry per
// (workspace token, method). A crude cap-and-reset rather than an LRU,
// mirroring slackUserInfoCache: a rotated credential leaves a dead bucket
// behind, and resetting at the cap costs at most one fresh burst per method
// per workspace the next time each is used, which is a request or two above
// steady state, never a throttle.
const directoryRateLimiterMax = 4096

// tierLimiter is the real directoryRateLimiter: a lazily-populated map of
// per-(token, method) token buckets. Concurrency-safe — the map is guarded by
// mu, and rate.Limiter itself is safe for concurrent use.
type tierLimiter struct {
	mu    sync.Mutex
	byKey map[string]*rate.Limiter
}

func newTierLimiter() *tierLimiter {
	return &tierLimiter{byKey: map[string]*rate.Limiter{}}
}

// limiter returns the token bucket for (fingerprint, method), creating it on
// first use. Held only long enough to read or insert the bucket — never
// across the blocking Wait below, so a slow method for one workspace does not
// stall another workspace's lookup.
func (l *tierLimiter) limiter(fingerprint string, m slackMethod) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byKey == nil {
		l.byKey = map[string]*rate.Limiter{}
	}
	key := fingerprint + ":" + m.name
	if lim, ok := l.byKey[key]; ok {
		return lim
	}
	if len(l.byKey) >= directoryRateLimiterMax {
		l.byKey = map[string]*rate.Limiter{}
	}
	lim := rate.NewLimiter(m.limit, directoryRateLimitBurst)
	l.byKey[key] = lim
	return lim
}

// wait blocks until the (fingerprint, method) bucket admits one call, or ctx
// is done (a cancelled context or a deadline shorter than the wait returns an
// error, which the caller surfaces as an ordinary scope error — it is not a
// Slack 429 and deliberately carries no RetryAfter).
func (l *tierLimiter) wait(ctx context.Context, fingerprint string, m slackMethod) error {
	return l.limiter(fingerprint, m).Wait(ctx)
}
