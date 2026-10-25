package mcpfront

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/singleflight"
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
	// MISS triggers a re-list sooner than this — as soon as the cache is
	// older than missRelistFloor (see tokenCache.resolve) — so this TTL only
	// bounds how stale a HIT may be, i.e. how long a REVOKED token (its CR
	// deleted) keeps authenticating purely from cache. Revocation itself is
	// still enforced immediately by the SpiceDB leg checks a tool call makes
	// afterward (the finalizer deletes the token's tuples on CR delete) —
	// this cache only gates AUTHENTICATION (does a hash resolve to a live,
	// unexpired token at all), never authorization.
	tokenCacheTTL = 30 * time.Second

	// missRelistFloor bounds how often an UNKNOWN hash may trigger a re-list:
	// a miss re-lists only when the cache is older than this floor; a miss
	// against a younger cache trusts the recent authoritative set and 401s
	// with NO K8s round trip. This is the amplification bound for
	// unauthenticated garbage bearers — without it, every junk request is a
	// namespace-wide List an attacker controls, entirely upstream of any rate
	// limit (the limiter is per TOKEN, and a garbage bearer never resolves to
	// one). A single rule, no per-hash bookkeeping: distinct garbage hashes
	// within one floor window share the same "the list is seconds old; you
	// are not in it" answer.
	//
	// The accepted cost: a token minted mid-window authenticates up to this
	// long after mint, not instantly. In practice the mint flow's first /mcp
	// call (the OAuth client finishing its token exchange, then connecting)
	// lands later than this anyway.
	missRelistFloor = 2 * time.Second

	// lastUsedResolution is the minimum gap between two status.lastUsedAt
	// patches for the same token — an observation, not an enforcement signal,
	// so it is written on a coarse cadence rather than on every request (see
	// AGENTS.md's server-side-apply idempotency note: a write per request is
	// exactly the pattern to avoid).
	lastUsedResolution = time.Hour

	// lastUsedRetryBackoff is the minimum gap between two lastUsedAt write
	// ATTEMPTS for the same token, successful or not. Without it a write that
	// keeps failing (conflict, RBAC drift, a slow apiserver) would be retried
	// — and logged — on every request the token makes.
	lastUsedRetryBackoff = time.Minute

	// lastUsedWriteTimeout bounds one background lastUsedAt write. The write
	// runs detached from the request (it must not block it, nor die with it),
	// so this is the only thing stopping a hung apiserver call from pinning a
	// goroutine.
	lastUsedWriteTimeout = 10 * time.Second

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
// AccessToken's id, the owner it acts on behalf of, and when it expires.
//
// It reaches a tool handler through the MCP SDK's per-request
// auth.TokenInfo (req.Extra.TokenInfo), NEVER through the handler's ctx. The
// stateful StreamableHTTPHandler connects a session on `initialize` with THAT
// request's context, and every later tool call on the session runs on the
// connection's context — so a value put on r.Context() by this middleware
// would be the INITIALIZE request's identity for the whole session: a second
// token replaying the first's Mcp-Session-Id would run as the first, and a
// re-minted token would keep acting as the revoked one. TokenInfo, by
// contrast, is stamped onto each JSON-RPC message from the HTTP request that
// carried it.
type acting struct {
	TokenID   string
	Owner     identity.CanonicalUserID
	ExpiresAt time.Time
}

// tokenInfoActingKey is the auth.TokenInfo.Extra key carrying the acting
// value itself.
const tokenInfoActingKey = "mcpfront.acting"

// tokenInfoFor is the auth.TokenInfo the SDK binds to a request. UserID is
// the OWNER, which turns on the SDK's session-hijack guard (a session
// created by one user refuses a request from another with 403) while
// letting the same person's re-minted token keep its session — identity is
// re-read per request anyway, so the session carries no stale authority.
func tokenInfoFor(a acting) *auth.TokenInfo {
	return &auth.TokenInfo{
		UserID:     a.Owner.String(),
		Expiration: a.ExpiresAt,
		Extra:      map[string]any{tokenInfoActingKey: a},
	}
}

// actingFromTokenInfo recovers the acting identity tokenInfoFor stored.
func actingFromTokenInfo(ti *auth.TokenInfo) (acting, bool) {
	if ti == nil {
		return acting{}, false
	}
	a, ok := ti.Extra[tokenInfoActingKey].(acting)
	return a, ok
}

// actingFrom returns the identity of the HTTP request that carried req. ok is
// false for a request that didn't pass through the bearer middleware (e.g. a
// test calling a tool handler directly without it).
func actingFrom(req *mcp.CallToolRequest) (acting, bool) {
	if req == nil || req.Extra == nil {
		return acting{}, false
	}
	return actingFromTokenInfo(req.Extra.TokenInfo)
}

// authenticatedKey carries the middleware's resolved identity across the one
// hop to the SDK's bearer wrapper (see bindTokenInfo), and nowhere else.
type authenticatedKey struct{}

// bindTokenInfo wraps next in the SDK's own RequireBearerToken — the only
// exported way to put an auth.TokenInfo on a request context — with a
// verifier that does no verification of its own: it hands back what the
// bearer middleware already resolved. Every real failure has already 401'd
// through writeUnauthorized before this runs; the ErrInvalidToken branch is
// unreachable unless something mounts this wrapper without the middleware.
func bindTokenInfo(next http.Handler) http.Handler {
	return auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		a, ok := ctx.Value(authenticatedKey{}).(acting)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return tokenInfoFor(a), nil
	}, nil)(next)
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
// AccessToken in d.AccessTokenNamespace(), plus the per-token rate limiters.
// The two share one mutex but deliberately NOT one lifecycle — see
// rebuildLocked for why the limiters are pruned rather than wiped. See
// tokenCacheTTL's doc for exactly what staleness here does and does not mean
// for revocation.
type tokenCache struct {
	mu     sync.Mutex
	byHash map[string]cachedToken
	// listedAt is when byHash was last rebuilt from a List. Both freshness
	// rules derive from it: a HIT is served until listedAt+tokenCacheTTL, and
	// a MISS re-lists only once the cache is older than missRelistFloor.
	listedAt time.Time
	limiters map[string]*rate.Limiter
	// lastUsedAttempts records, per token id, when a lastUsedAt write was
	// last STARTED (see lastUsedRetryBackoff). Same lifecycle as limiters:
	// pruned, never wiped, on rebuild.
	lastUsedAttempts map[string]time.Time
	// touches tracks in-flight background lastUsedAt writes, so a test can
	// wait for them.
	touches sync.WaitGroup
	// relistGroup collapses concurrent re-lists into ONE List call: N
	// concurrent requests bearing unknown tokens (or arriving just as the TTL
	// lapses) share a single K8s round trip rather than issuing N.
	relistGroup singleflight.Group
}

func newTokenCache() *tokenCache {
	return &tokenCache{
		byHash:           map[string]cachedToken{},
		limiters:         map[string]*rate.Limiter{},
		lastUsedAttempts: map[string]time.Time{},
	}
}

// resolve answers whether hash names a live, unexpired AccessToken. A cache
// HIT inside the TTL is served with no K8s round trip — this runs on every
// request. A stale cache (TTL lapsed) re-lists before answering either way.
// A MISS against a cache younger than missRelistFloor is answered 401 from
// the cache alone — the authoritative set is seconds old, and a hash absent
// from it is a miss WITHOUT another List (the unauthenticated-amplification
// bound; see missRelistFloor's doc). A miss against an older cache re-lists
// (through singleflight, so concurrent misses share one List) and retries
// once: a token minted after the floor elapsed (by identityd's /oauth/token,
// running in a different request) is visible on its very first use, not only
// after the TTL next lapses.
func (c *tokenCache) resolve(ctx context.Context, d Deps, hash string, now time.Time) (cachedToken, bool, error) {
	c.mu.Lock()
	tok, hit := c.byHash[hash]
	age := now.Sub(c.listedAt)
	c.mu.Unlock()

	if hit && age < tokenCacheTTL {
		return checkExpiry(tok, now)
	}
	if !hit && age < missRelistFloor {
		return cachedToken{}, false, nil
	}

	if err := c.relist(ctx, d, now); err != nil {
		return cachedToken{}, false, err
	}

	c.mu.Lock()
	tok, hit = c.byHash[hash]
	c.mu.Unlock()
	if !hit {
		return cachedToken{}, false, nil
	}
	return checkExpiry(tok, now)
}

// relist re-lists every AccessToken in d.AccessTokenNamespace() and rebuilds
// the cache, through singleflight so concurrent callers share one List. The
// double-check inside the flight handles the caller that lost the race: a
// flight that completed between its outer age check and its Do call has
// already rebuilt, so re-listing again would defeat the floor — it skips
// instead and lets the caller re-read the fresh cache. A canceled or failed
// leader fails every sharer closed (their requests 401, logged); the next
// request simply starts a new flight.
func (c *tokenCache) relist(ctx context.Context, d Deps, now time.Time) error {
	_, err, _ := c.relistGroup.Do("relist", func() (any, error) {
		c.mu.Lock()
		rebuiltMoments := now.Sub(c.listedAt) < missRelistFloor
		c.mu.Unlock()
		if rebuiltMoments {
			return nil, nil
		}
		var list spiceboxv1alpha1.AccessTokenList
		if err := d.K8s().List(ctx, &list, client.InNamespace(d.AccessTokenNamespace())); err != nil {
			return nil, fmt.Errorf("mcpfront: list access tokens: %w", err)
		}
		c.mu.Lock()
		c.rebuildLocked(list, now)
		c.mu.Unlock()
		return nil, nil
	})
	return err
}

// checkExpiry denies a cache hit whose AccessToken has passed its own
// spec.expiresAt — the "expired CR: 401 even though hash matches" case: the
// CR can still exist (GC hasn't swept it yet) while the token itself must no
// longer authenticate.
//
// A zero expiresAt is refused too: the CRD requires the field, and the SDK's
// bearer wrapper (bindTokenInfo) would refuse a zero TokenInfo.Expiration with
// a 401 of its own wording — so a zero must be caught here, where every
// failure reads the same.
func checkExpiry(tok cachedToken, now time.Time) (cachedToken, bool, error) {
	if tok.ExpiresAt.IsZero() || !now.Before(tok.ExpiresAt) {
		return cachedToken{}, false, nil
	}
	return tok, true, nil
}

// rebuildLocked replaces the cache's hash map wholesale from a freshly-listed
// set and resets listedAt. The limiter map is PRUNED, never wiped: only
// entries whose token id is absent from the fresh list are dropped — a
// revoked or rotated token's rate-limit state must not linger once it's no
// longer live, but a LIVE token's spent budget must survive every rebuild.
// Wiping the whole map here was a real bug: a rebuild is triggerable by any
// stale-cache miss, so an attacker who exhausted their token's budget could
// send one garbage bearer and collect a fresh burst — the rate limit reset on
// demand, by the very party it limits. Caller holds c.mu.
func (c *tokenCache) rebuildLocked(list spiceboxv1alpha1.AccessTokenList, now time.Time) {
	fresh := make(map[string]cachedToken, len(list.Items))
	live := make(map[string]bool, len(list.Items))
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
		live[item.Name] = true
	}
	c.byHash = fresh
	c.listedAt = now
	for id := range c.limiters {
		if !live[id] {
			delete(c.limiters, id)
		}
	}
	for id := range c.lastUsedAttempts {
		if !live[id] {
			delete(c.lastUsedAttempts, id)
		}
	}
}

// limiterFor returns tokenID's rate limiter, creating a fresh one (full
// burst) on the token's first use. The limiter then lives for as long as the
// token stays in the listed set — rebuilds prune dead entries but never
// reset a live token's budget (see rebuildLocked).
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

// touchLastUsed records status.lastUsedAt when the cached value is zero or at
// least lastUsedResolution old. This is OBSERVATION, not enforcement, so it
// never costs the request anything: the write runs in the background, and at
// most one attempt per token starts per lastUsedRetryBackoff whether the
// previous one succeeded or not. A failed write is logged (AGENTS.md's
// no-silent-errors rule — logging is the allowed second option) and retried
// no sooner than the backoff; a successful one is remembered in the cache so
// the next lastUsedResolution of requests skip it entirely.
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
	c.mu.Lock()
	if last, ok := c.lastUsedAttempts[tok.TokenID]; ok && now.Sub(last) < lastUsedRetryBackoff {
		c.mu.Unlock()
		return
	}
	c.lastUsedAttempts[tok.TokenID] = now
	c.mu.Unlock()

	c.touches.Add(1)
	go func() {
		defer c.touches.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lastUsedWriteTimeout)
		defer cancel()
		if err := patchLastUsed(ctx, d, tok.TokenID, now); err != nil {
			d.Logger().Info("mcpfront: lastUsed not recorded; retrying no sooner than the backoff",
				"tokenID", tok.TokenID, "retryAfter", lastUsedRetryBackoff.String(), "err", err.Error())
			return
		}
		c.mu.Lock()
		if entry, ok := c.byHash[hash]; ok {
			entry.LastUsedAt = now
			c.byHash[hash] = entry
		}
		c.mu.Unlock()
	}()
}

// patchLastUsed writes status.lastUsedAt=now onto the named AccessToken.
func patchLastUsed(ctx context.Context, d Deps, tokenID string, now time.Time) error {
	var cr spiceboxv1alpha1.AccessToken
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: d.AccessTokenNamespace(), Name: tokenID}, &cr); err != nil {
		return fmt.Errorf("get: %w", err)
	}
	original := cr.DeepCopy()
	patchedAt := metav1.NewTime(now)
	cr.Status.LastUsedAt = &patchedAt
	if err := d.K8s().Status().Patch(ctx, &cr, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("status patch: %w", err)
	}
	return nil
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
// tokenCache), rate-limit per token, record lastUsedAt in the background, and
// bind the resolved acting identity to the request as the SDK's
// auth.TokenInfo (see acting's doc for why not the context). clock is
// time.Now in production and an injectable fake in tests, so the rate
// limiter's refill and the lastUsed resolution window are both deterministic
// under test.
func newBearerMiddleware(d Deps, clock func() time.Time) func(http.Handler) http.Handler {
	return newTokenCache().middleware(d, clock)
}

// middleware is newBearerMiddleware over an explicit cache, so a test can
// reach the cache (e.g. to wait on its background lastUsedAt writes).
func (c *tokenCache) middleware(d Deps, clock func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		next = bindTokenInfo(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			now := clock()

			value, ok := parseBearer(r.Header.Get("Authorization"))
			if !ok {
				d.Logger().Info("mcpfront: bearer rejected", "reason", "missing or malformed Authorization header")
				writeUnauthorized(w, d)
				return
			}

			hash := accesstoken.HashTokenValue(value)
			tok, found, err := c.resolve(r.Context(), d, hash, now)
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

			limiter := c.limiterFor(tok.TokenID)
			if !limiter.AllowN(now, 1) {
				d.Logger().Info("mcpfront: rate limit exceeded", "tokenID", tok.TokenID)
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}

			c.touchLastUsed(r.Context(), d, hash, tok, now)

			a := acting{TokenID: tok.TokenID, Owner: tok.Owner, ExpiresAt: tok.ExpiresAt}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authenticatedKey{}, a)))
		})
	}
}
