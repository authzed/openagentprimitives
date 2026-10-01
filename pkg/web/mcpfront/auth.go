package mcpfront

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
)

const (
	// tokenCacheTTL bounds how long the bearer middleware trusts its
	// in-process hash->identity map before re-listing AccessTokens. A hash
	// MISS always triggers an immediate re-list regardless of this TTL (see
	// tokenCache.resolve) — this TTL only bounds how stale a HIT may be, i.e.
	// how long a REVOKED token (its CR deleted) keeps authenticating purely
	// from cache. Revocation itself is still enforced immediately by the
	// SpiceDB leg checks a tool call makes afterward (the finalizer deletes
	// the token's tuples on CR delete) — this cache only gates
	// AUTHENTICATION (does a hash resolve to a live, unexpired token at
	// all), never authorization.
	tokenCacheTTL = 30 * time.Second

	// lastUsedResolution is the minimum gap between two status.lastUsedAt
	// patches for the same token — an observation, not an enforcement signal,
	// so it is written on a coarse cadence rather than on every request (see
	// AGENTS.md's server-side-apply idempotency note: a write per request is
	// exactly the pattern to avoid).
	lastUsedResolution = time.Hour

	// tokenRateLimitRPS / tokenRateLimitBurst bound each individual token's
	// request rate, independent of every other token.
	tokenRateLimitRPS   = 10
	tokenRateLimitBurst = 20

	// bearerPrefix is the only Authorization scheme this middleware accepts.
	bearerPrefix = "Bearer "

	// logHashPrefixLen is how many hex characters of a token's hash are safe
	// to log — enough to correlate repeated failures without ever writing
	// the full hash (AGENTS.md: "never the hash") to a log an operator might
	// grep and paste elsewhere.
	logHashPrefixLen = 8
)

// acting is who a /mcp request is authenticated as: the presented
// AccessToken's id and the owner it acts on behalf of. Carried on the
// request context by the bearer middleware; read back via actingFrom by
// every tool handler (Task 10) that needs to run a SpiceDB check.
type acting struct {
	TokenID string
	Owner   identity.CanonicalUserID
}

type actingKeyT struct{}

var actingKey actingKeyT

func withActing(ctx context.Context, a acting) context.Context {
	return context.WithValue(ctx, actingKey, a)
}

// actingFrom returns the acting identity the bearer middleware attached to
// ctx. ok is false for any context that didn't pass through the middleware
// (e.g. a test calling a tool handler directly without it).
func actingFrom(ctx context.Context) (acting, bool) {
	a, ok := ctx.Value(actingKey).(acting)
	return a, ok
}

// cachedToken is one AccessToken's authentication-relevant state, keyed by
// its hash in tokenCache.byHash.
type cachedToken struct {
	TokenID    string
	Owner      identity.CanonicalUserID
	ExpiresAt  time.Time
	LastUsedAt time.Time // zero means "never patched" (or not yet observed in this cache generation)
}

// tokenCache is the /mcp bearer middleware's in-process view of every live
// AccessToken in d.AccessTokenNamespace(), plus the per-token rate limiters
// that share its lifecycle. See tokenCacheTTL's doc for exactly what staleness
// here does and does not mean for revocation.
type tokenCache struct {
	mu       sync.Mutex
	byHash   map[string]cachedToken
	expires  time.Time
	limiters map[string]*rate.Limiter
}

func newTokenCache() *tokenCache {
	return &tokenCache{byHash: map[string]cachedToken{}, limiters: map[string]*rate.Limiter{}}
}

// resolve answers whether hash names a live, unexpired AccessToken. A cache
// HIT inside the TTL is served with no K8s round trip — this runs on every
// request. A MISS, whether the hash is simply unknown or the TTL has lapsed,
// re-lists every AccessToken in d.AccessTokenNamespace(), rebuilds the cache,
// and retries once: a token minted a moment ago (by identityd's /oauth/token,
// running in a different request) is visible on its very first use, not only
// after the TTL next lapses.
func (c *tokenCache) resolve(ctx context.Context, d Deps, hash string, now time.Time) (cachedToken, bool, error) {
	c.mu.Lock()
	tok, hit := c.byHash[hash]
	fresh := hit && now.Before(c.expires)
	c.mu.Unlock()
	if fresh {
		return checkExpiry(tok, now)
	}

	var list spiceboxv1alpha1.AccessTokenList
	if err := d.K8s().List(ctx, &list, client.InNamespace(d.AccessTokenNamespace())); err != nil {
		return cachedToken{}, false, fmt.Errorf("mcpfront: list access tokens: %w", err)
	}

	c.mu.Lock()
	c.rebuildLocked(list, now)
	tok, hit = c.byHash[hash]
	c.mu.Unlock()
	if !hit {
		return cachedToken{}, false, nil
	}
	return checkExpiry(tok, now)
}

// checkExpiry denies a cache hit whose AccessToken has passed its own
// spec.expiresAt — the "expired CR: 401 even though hash matches" case: the
// CR can still exist (GC hasn't swept it yet) while the token itself must no
// longer authenticate.
func checkExpiry(tok cachedToken, now time.Time) (cachedToken, bool, error) {
	if !tok.ExpiresAt.IsZero() && !now.Before(tok.ExpiresAt) {
		return cachedToken{}, false, nil
	}
	return tok, true, nil
}

// rebuildLocked replaces the cache's hash map wholesale from a freshly-listed
// set and resets the TTL. The limiter map is dropped too: a revoked or
// rotated token's rate-limit state must not linger once it's no longer live,
// and a re-minted token (a fresh TokenID — see mint.go's TOUCH-vs-role-change
// doc) starts its own budget rather than inheriting a stranger's. Caller
// holds c.mu.
func (c *tokenCache) rebuildLocked(list spiceboxv1alpha1.AccessTokenList, now time.Time) {
	fresh := make(map[string]cachedToken, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		var lastUsed time.Time
		if item.Status.LastUsedAt != nil {
			lastUsed = item.Status.LastUsedAt.Time
		}
		fresh[item.Spec.TokenHash] = cachedToken{
			TokenID: item.Name,
			Owner: identity.CanonicalFromTrusted(item.Spec.Owner,
				"read back from the AccessToken CR's spec.owner, written only by the mint path"),
			ExpiresAt:  item.Spec.ExpiresAt.Time,
			LastUsedAt: lastUsed,
		}
	}
	c.byHash = fresh
	c.expires = now.Add(tokenCacheTTL)
	c.limiters = map[string]*rate.Limiter{}
}

// limiterFor returns tokenID's rate limiter, creating a fresh one (full
// burst) on first use within this cache generation.
func (c *tokenCache) limiterFor(tokenID string) *rate.Limiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.limiters[tokenID]
	if !ok {
		l = rate.NewLimiter(rate.Limit(tokenRateLimitRPS), tokenRateLimitBurst)
		c.limiters[tokenID] = l
	}
	return l
}

// touchLastUsed patches status.lastUsedAt when the cached value is zero or at
// least lastUsedResolution old, then remembers the new value in the cache ON
// SUCCESS ONLY — a transient apiserver failure is retried on the very next
// request rather than silently debounced for an hour. This is OBSERVATION,
// not enforcement: a patch failure is logged and never fails the request
// (AGENTS.md's no-silent-errors rule — logging is the allowed second option).
//
// The Get-then-MergeFrom-Patch shape (rather than constructing a bare patch
// object by hand) mirrors pkg/web/webui/agentui's wake.go: diffing against a
// real, just-fetched copy means the computed patch touches only
// status.lastUsedAt, never clobbering Conditions or observedGeneration this
// process doesn't know about.
func (c *tokenCache) touchLastUsed(ctx context.Context, d Deps, hash string, tok cachedToken, now time.Time) {
	if !tok.LastUsedAt.IsZero() && now.Sub(tok.LastUsedAt) < lastUsedResolution {
		return
	}
	var cr spiceboxv1alpha1.AccessToken
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: d.AccessTokenNamespace(), Name: tok.TokenID}, &cr); err != nil {
		d.Logger().Info("mcpfront: lastUsed Get failed; status not patched", "tokenID", tok.TokenID, "err", err.Error())
		return
	}
	original := cr.DeepCopy()
	patchedAt := metav1.NewTime(now)
	cr.Status.LastUsedAt = &patchedAt
	if err := d.K8s().Status().Patch(ctx, &cr, client.MergeFrom(original)); err != nil {
		d.Logger().Info("mcpfront: lastUsed patch failed", "tokenID", tok.TokenID, "err", err.Error())
		return
	}
	c.mu.Lock()
	if entry, ok := c.byHash[hash]; ok {
		entry.LastUsedAt = now
		c.byHash[hash] = entry
	}
	c.mu.Unlock()
}

// parseBearer extracts the token value from an Authorization header,
// rejecting anything not of the exact form "Bearer <value>".
func parseBearer(header string) (string, bool) {
	v, ok := strings.CutPrefix(header, bearerPrefix)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// hashPrefix returns a short, log-safe prefix of a token hash — enough to
// correlate repeated failures across log lines without ever writing the full
// hash, which (per AGENTS.md) must never appear in a log.
func hashPrefix(hash string) string {
	if len(hash) <= logHashPrefixLen {
		return hash
	}
	return hash[:logHashPrefixLen]
}

// writeUnauthorized is the ONE place every bearer-auth failure writes its
// response: status 401, a WWW-Authenticate challenge pointing at the
// protected-resource metadata, and an empty body. Every caller (missing
// header, malformed header, unknown token, expired token, lookup error, a
// revoked/deleted CR) routes through this single function, which is what
// makes the 401 byte-identical across every failure mode — there is no
// oracle here telling a caller WHY authentication failed.
func writeUnauthorized(w http.ResponseWriter, d Deps) {
	base := strings.TrimSuffix(d.ExternalBaseURL(), "/")
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, base))
	w.WriteHeader(http.StatusUnauthorized)
}

// newBearerMiddleware builds the /mcp authentication middleware: hash the
// presented bearer, resolve it against d's AccessTokens (cached — see
// tokenCache), rate-limit per token, best-effort record lastUsedAt, and
// attach the resolved acting identity to the request context. clock is
// time.Now in production and an injectable fake in tests, so the rate
// limiter's refill and the lastUsed resolution window are both deterministic
// under test.
func newBearerMiddleware(d Deps, clock func() time.Time) func(http.Handler) http.Handler {
	cache := newTokenCache()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			now := clock()

			value, ok := parseBearer(r.Header.Get("Authorization"))
			if !ok {
				d.Logger().Info("mcpfront: bearer rejected", "reason", "missing or malformed Authorization header")
				writeUnauthorized(w, d)
				return
			}

			hash := accesstoken.HashTokenValue(value)
			tok, found, err := cache.resolve(r.Context(), d, hash, now)
			if err != nil {
				d.Logger().Info("mcpfront: bearer rejected", "reason", "token lookup errored", "hashPrefix", hashPrefix(hash), "err", err.Error())
				writeUnauthorized(w, d)
				return
			}
			if !found {
				d.Logger().Info("mcpfront: bearer rejected", "reason", "unknown, expired, or revoked token", "hashPrefix", hashPrefix(hash))
				writeUnauthorized(w, d)
				return
			}

			limiter := cache.limiterFor(tok.TokenID)
			if !limiter.AllowN(now, 1) {
				d.Logger().Info("mcpfront: rate limit exceeded", "tokenID", tok.TokenID)
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}

			cache.touchLastUsed(r.Context(), d, hash, tok, now)

			ctx := withActing(r.Context(), acting{TokenID: tok.TokenID, Owner: tok.Owner})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
