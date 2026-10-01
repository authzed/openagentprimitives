package mcpfront

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
)

const testAccessTokenNamespace = "agentprimitives-system"

// fakeDeps is the auth.go test double for Deps: a real controller-runtime
// fake client plus a fixed namespace/base URL. AccessTokenAuthz/OperatorURL/
// MemoryToken/Artifacts/LookupReadableSessions are never exercised by the
// bearer middleware itself (they exist for registerTools, Task 10) and are
// stubbed to their zero values.
type fakeDeps struct {
	k8s             client.Client
	ns              string
	externalBaseURL string
}

func (f *fakeDeps) K8s() client.Client                 { return f.k8s }
func (f *fakeDeps) AccessTokenAuthz() AccessTokenAuthz { return nil }
func (f *fakeDeps) AccessTokenNamespace() string       { return f.ns }
func (f *fakeDeps) OperatorURL() string                { return "" }
func (f *fakeDeps) MemoryToken() string                { return "" }
func (f *fakeDeps) Artifacts() *artifacts.Service      { return nil }
func (f *fakeDeps) LookupReadableSessions(_ context.Context, _ identity.CanonicalUserID) (spicedb.InteractableSessions, error) {
	return spicedb.InteractableSessions{}, nil
}
func (f *fakeDeps) FetchArtifact(_ context.Context, _, _, _ string) ([]byte, string, error) {
	return nil, "", nil
}
func (f *fakeDeps) ExternalBaseURL() string { return f.externalBaseURL }
func (f *fakeDeps) Logger() logr.Logger     { return logr.Discard() }

var _ Deps = (*fakeDeps)(nil)

func newFakeDeps(c client.Client) *fakeDeps {
	return &fakeDeps{k8s: c, ns: testAccessTokenNamespace, externalBaseURL: "https://example.test"}
}

func mustNewTokenValue(t *testing.T) string {
	t.Helper()
	v, err := accesstoken.NewTokenValue()
	require.NoError(t, err)
	return v
}

// accessTokenCR builds a test AccessToken CR: name, the bearer value it
// authenticates (stored only as its hash — the plaintext is never
// persisted, matching the real mint path), the owner, and its expiry.
func accessTokenCR(name, value, owner string, expiresAt time.Time) *spiceboxv1alpha1.AccessToken {
	return &spiceboxv1alpha1.AccessToken{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testAccessTokenNamespace},
		Spec: spiceboxv1alpha1.AccessTokenSpec{
			TokenHash: accesstoken.HashTokenValue(value),
			Owner:     owner,
			ExpiresAt: metav1.NewTime(expiresAt),
		},
	}
}

func newAuthTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newMintScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AccessToken{}).
		WithObjects(objects...).
		Build()
}

// recordingHandler is the inner http.Handler every middleware test wraps: it
// records whether it ran at all and what actingFrom returned, so a test can
// assert both "did the request reach the application" and "with what
// identity" in one place.
type recordingHandler struct {
	ran    bool
	acting acting
	ok     bool
}

func (h *recordingHandler) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ran = true
		h.acting, h.ok = actingFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func doBearerRequest(mw func(http.Handler) http.Handler, inner http.Handler, header string) *httptest.ResponseRecorder {
	h := mw(inner)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBearerMiddleware(t *testing.T) {
	now := time.Now()
	good := mustNewTokenValue(t)
	goodCR := accessTokenCR("at-aaaaaaaaaaaa", good, "owner-canonical", now.Add(time.Hour))
	expiredVal := mustNewTokenValue(t)
	expiredCR := accessTokenCR("at-bbbbbbbbbbbb", expiredVal, "owner-canonical", now.Add(-time.Minute))

	cases := []struct {
		name       string
		header     string
		objects    []client.Object
		wantStatus int
		wantActing string // expected TokenID when 200
	}{
		{name: "no header: 401 + challenge", header: "", objects: []client.Object{goodCR}, wantStatus: http.StatusUnauthorized},
		{name: "malformed header: 401", header: "Token abc", objects: []client.Object{goodCR}, wantStatus: http.StatusUnauthorized},
		{name: "unknown token: 401", header: "Bearer " + mustNewTokenValue(t), objects: []client.Object{goodCR}, wantStatus: http.StatusUnauthorized},
		{name: "expired CR: 401 even though hash matches", header: "Bearer " + expiredVal, objects: []client.Object{expiredCR}, wantStatus: http.StatusUnauthorized},
		{name: "deleted CR (revocation race): 401", header: "Bearer " + good, objects: nil, wantStatus: http.StatusUnauthorized},
		{name: "valid: 200, acting carries token id and owner", header: "Bearer " + good, objects: []client.Object{goodCR}, wantStatus: http.StatusOK, wantActing: "at-aaaaaaaaaaaa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAuthTestClient(t, tc.objects...)
			d := newFakeDeps(c)
			mw := newBearerMiddleware(d, func() time.Time { return now })

			rh := &recordingHandler{}
			rec := doBearerRequest(mw, rh.handler(), tc.header)

			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantStatus == http.StatusOK {
				assert.True(t, rh.ran, "inner handler must run on success")
				require.True(t, rh.ok, "acting must be present in context")
				assert.Equal(t, tc.wantActing, rh.acting.TokenID)
				assert.Equal(t, "owner-canonical", rh.acting.Owner.String())
			} else {
				assert.False(t, rh.ran, "inner handler must NOT run on a rejected request")
			}
		})
	}
}

// TestBearerMiddleware401IsUniform asserts the 401 body and WWW-Authenticate
// header are byte-identical for missing, unknown, and expired tokens — no
// revocation oracle telling a caller which of the three actually happened.
func TestBearerMiddleware401IsUniform(t *testing.T) {
	now := time.Now()
	goodCR := accessTokenCR("at-aaaaaaaaaaaa", mustNewTokenValue(t), "owner-canonical", now.Add(time.Hour))
	expiredVal := mustNewTokenValue(t)
	expiredCR := accessTokenCR("at-cccccccccccc", expiredVal, "owner-canonical", now.Add(-time.Minute))

	headers := []string{
		"",                               // missing
		"Bearer " + mustNewTokenValue(t), // unknown
		"Bearer " + expiredVal,           // expired
	}

	type captured struct {
		body    []byte
		wwwAuth string
	}
	var results []captured
	for _, h := range headers {
		c := newAuthTestClient(t, goodCR, expiredCR)
		d := newFakeDeps(c)
		mw := newBearerMiddleware(d, func() time.Time { return now })

		rh := &recordingHandler{}
		rec := doBearerRequest(mw, rh.handler(), h)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.False(t, rh.ran)
		results = append(results, captured{body: rec.Body.Bytes(), wwwAuth: rec.Header().Get("WWW-Authenticate")})
	}

	for i := 1; i < len(results); i++ {
		assert.Equal(t, results[0].body, results[i].body, "401 body must be byte-identical across every failure mode")
		assert.Equal(t, results[0].wwwAuth, results[i].wwwAuth, "WWW-Authenticate must be byte-identical across every failure mode")
	}
	assert.Empty(t, results[0].body, "401 body must be empty")
	assert.Equal(t, `Bearer resource_metadata="https://example.test/.well-known/oauth-protected-resource"`, results[0].wwwAuth)
}

// TestTokenCacheRefreshesOnMiss: a request with token B misses the cache
// warmed with only A and, once the cache is older than missRelistFloor,
// triggers a re-list and succeeds — a token minted after warm-up (by
// identityd's /oauth/token, running in a different request against the same
// K8s namespace) works on its first use after the floor, long before the
// 30s TTL would next lapse. The pre-floor request pins the documented
// trade-off: a miss against a cache younger than the floor is answered 401
// from the cache alone (the unauthenticated-List amplification bound), so a
// just-minted token has a ≤missRelistFloor first-use delay.
func TestTokenCacheRefreshesOnMiss(t *testing.T) {
	cur := time.Now()
	clock := func() time.Time { return cur }
	valA := mustNewTokenValue(t)
	crA := accessTokenCR("at-aaaaaaaaaaaa", valA, "owner-a", cur.Add(time.Hour))
	c := newAuthTestClient(t, crA)
	d := newFakeDeps(c)
	mw := newBearerMiddleware(d, clock)

	// Warm the cache with A.
	rhA := &recordingHandler{}
	recA := doBearerRequest(mw, rhA.handler(), "Bearer "+valA)
	require.Equal(t, http.StatusOK, recA.Code)

	// Mint B directly against the SAME fake client — simulating a token
	// minted elsewhere after the cache warmed, never going through the
	// middleware itself.
	valB := mustNewTokenValue(t)
	crB := accessTokenCR("at-bbbbbbbbbbbb", valB, "owner-b", cur.Add(time.Hour))
	require.NoError(t, c.Create(context.Background(), crB))

	// Within the floor: the cache is seconds old, so the miss is answered
	// from it without a re-list — B is not yet usable (the documented,
	// accepted first-use delay).
	recEarly := doBearerRequest(mw, (&recordingHandler{}).handler(), "Bearer "+valB)
	assert.Equal(t, http.StatusUnauthorized, recEarly.Code,
		"a miss against a fresher-than-floor cache must be refused without a re-list")

	// Past the floor: the miss re-lists and B authenticates.
	cur = cur.Add(missRelistFloor + time.Millisecond)
	rhB := &recordingHandler{}
	recB := doBearerRequest(mw, rhB.handler(), "Bearer "+valB)
	assert.Equal(t, http.StatusOK, recB.Code, "a token minted after cache warm-up must authenticate via the on-miss re-list once the floor has elapsed")
	assert.True(t, rhB.ran)
}

// TestRateLimiterSurvivesCacheRebuild is the regression test for the
// limiter-reset-on-demand bug: rebuildLocked used to WIPE the limiter map,
// and any stale-cache miss triggers a rebuild — so an attacker who exhausted
// their token's budget could send one garbage bearer and collect a fresh
// burst of 20. The limiter map must survive a rebuild for every token still
// in the listed set.
func TestRateLimiterSurvivesCacheRebuild(t *testing.T) {
	cur := time.Now()
	clock := func() time.Time { return cur }

	valA := mustNewTokenValue(t)
	crA := accessTokenCR("at-aaaaaaaaaaaa", valA, "owner-a", cur.Add(time.Hour))

	var listCount atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(newMintScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AccessToken{}).
		WithObjects(crA).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				listCount.Add(1)
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()
	d := newFakeDeps(c)
	mw := newBearerMiddleware(d, clock)

	doReq := func(value string) *httptest.ResponseRecorder {
		return doBearerRequest(mw, (&recordingHandler{}).handler(), "Bearer "+value)
	}

	// Warm the cache, then advance PAST the floor before exhausting, so the
	// garbage bearer below is able to trigger a rebuild with no further
	// clock movement (no movement ⇒ no limiter refill between exhaustion and
	// the post-rebuild probe — the refill and the reset must not be
	// conflated).
	require.Equal(t, http.StatusOK, doReq(valA).Code)
	cur = cur.Add(missRelistFloor + time.Second)

	for i := 0; i < tokenRateLimitBurst; i++ {
		require.Equal(t, http.StatusOK, doReq(valA).Code, "request %d within burst should succeed", i)
	}
	require.Equal(t, http.StatusTooManyRequests, doReq(valA).Code, "the budget must be spent before the rebuild is forced")

	// Force a rebuild: an unknown bearer against a stale-enough cache
	// re-lists. Prove the rebuild actually happened via the List count.
	before := listCount.Load()
	require.Equal(t, http.StatusUnauthorized, doReq(mustNewTokenValue(t)).Code)
	require.Equal(t, before+1, listCount.Load(), "the garbage bearer must have triggered a re-list for this test to prove anything")

	// The hot token's spent budget must have survived the rebuild.
	assert.Equal(t, http.StatusTooManyRequests, doReq(valA).Code,
		"an exhausted token must STILL be refused after a rebuild — a wiped limiter map hands the attacker a fresh burst on demand")
}

// TestMissRelistFloorBoundsUnauthenticatedLists pins both halves of the
// List-amplification bound: a miss against a fresher-than-floor cache
// answers 401 with NO List at all (so repeated garbage bearers — same or
// distinct — within one window cost zero K8s round trips), and a miss
// against an older cache re-lists exactly once.
func TestMissRelistFloorBoundsUnauthenticatedLists(t *testing.T) {
	cur := time.Now()
	clock := func() time.Time { return cur }

	valA := mustNewTokenValue(t)
	crA := accessTokenCR("at-aaaaaaaaaaaa", valA, "owner-a", cur.Add(time.Hour))

	var listCount atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(newMintScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AccessToken{}).
		WithObjects(crA).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				listCount.Add(1)
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()
	d := newFakeDeps(c)
	mw := newBearerMiddleware(d, clock)

	// Warm-up costs the one bootstrap List.
	require.Equal(t, http.StatusOK, doBearerRequest(mw, (&recordingHandler{}).handler(), "Bearer "+valA).Code)
	require.Equal(t, int32(1), listCount.Load())

	// A burst of DISTINCT garbage bearers within the floor window: every one
	// is refused from the cache alone — zero further Lists.
	for i := 0; i < 5; i++ {
		rec := doBearerRequest(mw, (&recordingHandler{}).handler(), "Bearer "+mustNewTokenValue(t))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	assert.Equal(t, int32(1), listCount.Load(), "misses within the floor window must not List at all")

	// Past the floor, one more garbage bearer re-lists — exactly once.
	cur = cur.Add(missRelistFloor + time.Millisecond)
	require.Equal(t, http.StatusUnauthorized, doBearerRequest(mw, (&recordingHandler{}).handler(), "Bearer "+mustNewTokenValue(t)).Code)
	assert.Equal(t, int32(2), listCount.Load(), "a stale-cache miss must re-list")

	// And the window resets: further garbage at the same instant is again
	// answered without a List.
	require.Equal(t, http.StatusUnauthorized, doBearerRequest(mw, (&recordingHandler{}).handler(), "Bearer "+mustNewTokenValue(t)).Code)
	assert.Equal(t, int32(2), listCount.Load(), "the re-list must start a fresh floor window")
}

// TestConcurrentUnknownBearersShareOneList: N concurrent requests bearing the
// same unknown token against a never-listed cache produce exactly ONE List —
// singleflight collapses the in-flight callers, and the double-check inside
// the flight stops a caller that lost the race from re-listing a cache
// another flight just rebuilt.
func TestConcurrentUnknownBearersShareOneList(t *testing.T) {
	now := time.Now()

	var listCount atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(newMintScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AccessToken{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				listCount.Add(1)
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()
	d := newFakeDeps(c)
	mw := newBearerMiddleware(d, func() time.Time { return now })

	unknown := "Bearer " + mustNewTokenValue(t)
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doBearerRequest(mw, (&recordingHandler{}).handler(), unknown).Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		assert.Equal(t, http.StatusUnauthorized, code, "request %d", i)
	}
	assert.Equal(t, int32(1), listCount.Load(), "concurrent unknown bearers must share exactly one List")
}

// TestRebuildPrunesOnlyDeadLimiters pins rebuildLocked's limiter contract
// directly: a limiter whose token survives the rebuild is the SAME limiter
// (its spent budget intact), and one whose token is gone from the listed set
// is dropped.
func TestRebuildPrunesOnlyDeadLimiters(t *testing.T) {
	now := time.Now()
	cache := newTokenCache()

	liveLimiter := cache.limiterFor("at-live")
	_ = cache.limiterFor("at-dead")

	list := spiceboxv1alpha1.AccessTokenList{Items: []spiceboxv1alpha1.AccessToken{
		*accessTokenCR("at-live", mustNewTokenValue(t), "owner-a", now.Add(time.Hour)),
	}}
	cache.mu.Lock()
	cache.rebuildLocked(list, now)
	cache.mu.Unlock()

	cache.mu.Lock()
	defer cache.mu.Unlock()
	assert.Same(t, liveLimiter, cache.limiters["at-live"], "a live token's limiter must survive the rebuild untouched")
	_, deadKept := cache.limiters["at-dead"]
	assert.False(t, deadKept, "a token absent from the listed set must have its limiter pruned")
}

// TestPerTokenRateLimit: hammering one token past its burst (20) within one
// window yields 429 + Retry-After, a DIFFERENT token is unaffected (the
// limiter is per token id, not global), and after the fake clock advances
// the first token is admitted again.
func TestPerTokenRateLimit(t *testing.T) {
	cur := time.Now()
	clock := func() time.Time { return cur }

	valA := mustNewTokenValue(t)
	crA := accessTokenCR("at-aaaaaaaaaaaa", valA, "owner-a", cur.Add(time.Hour))
	valB := mustNewTokenValue(t)
	crB := accessTokenCR("at-bbbbbbbbbbbb", valB, "owner-b", cur.Add(time.Hour))
	c := newAuthTestClient(t, crA, crB)
	d := newFakeDeps(c)
	mw := newBearerMiddleware(d, clock)

	doReq := func(value string) *httptest.ResponseRecorder {
		rh := &recordingHandler{}
		return doBearerRequest(mw, rh.handler(), "Bearer "+value)
	}

	for i := 0; i < tokenRateLimitBurst; i++ {
		rec := doReq(valA)
		require.Equal(t, http.StatusOK, rec.Code, "request %d within burst should succeed", i)
	}

	overBudget := doReq(valA)
	assert.Equal(t, http.StatusTooManyRequests, overBudget.Code)
	assert.Equal(t, "1", overBudget.Header().Get("Retry-After"))

	// A different token's budget is independent.
	assert.Equal(t, http.StatusOK, doReq(valB).Code, "a different token's budget must be independent")

	// Advance the clock enough to refill at least one token (10 rps ==
	// 1 token per 100ms) and the first token is admitted again.
	cur = cur.Add(200 * time.Millisecond)
	assert.Equal(t, http.StatusOK, doReq(valA).Code, "after the clock advances, the token is admitted again")
}

// TestLastUsedPatchedOnMeaningfulChangeOnly: two requests in quick succession
// patch status.lastUsedAt exactly once; a request after the fake clock
// advances past lastUsedResolution patches a second time. The SSA/
// idempotency rule under test: never a write per request.
func TestLastUsedPatchedOnMeaningfulChangeOnly(t *testing.T) {
	cur := time.Now()
	clock := func() time.Time { return cur }

	val := mustNewTokenValue(t)
	// Expiry must outlive the clock advance below (2h) or the third request
	// would correctly 401 on token expiry rather than exercising the lastUsed
	// patch cadence this test is actually about.
	cr := accessTokenCR("at-aaaaaaaaaaaa", val, "owner-a", cur.Add(24*time.Hour))

	patchCount := 0
	c := fake.NewClientBuilder().
		WithScheme(newMintScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AccessToken{}).
		WithObjects(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if subResourceName == "status" {
					patchCount++
				}
				return cl.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	d := newFakeDeps(c)
	mw := newBearerMiddleware(d, clock)

	doReq := func() {
		rh := &recordingHandler{}
		rec := doBearerRequest(mw, rh.handler(), "Bearer "+val)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	doReq()
	doReq()
	assert.Equal(t, 1, patchCount, "two requests in quick succession must patch status.lastUsedAt exactly once")

	cur = cur.Add(2 * time.Hour)
	doReq()
	assert.Equal(t, 2, patchCount, "a request after lastUsedResolution has elapsed must patch again")
}
