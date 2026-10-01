package mcpfront

import (
	"context"
	"net/http"
	"net/http/httptest"
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
// warmed with only A, triggers a re-list, and succeeds — a token minted
// after warm-up (by identityd's /oauth/token, running in a different
// request against the same K8s namespace) works on its very first use.
func TestTokenCacheRefreshesOnMiss(t *testing.T) {
	now := time.Now()
	valA := mustNewTokenValue(t)
	crA := accessTokenCR("at-aaaaaaaaaaaa", valA, "owner-a", now.Add(time.Hour))
	c := newAuthTestClient(t, crA)
	d := newFakeDeps(c)
	mw := newBearerMiddleware(d, func() time.Time { return now })

	// Warm the cache with A.
	rhA := &recordingHandler{}
	recA := doBearerRequest(mw, rhA.handler(), "Bearer "+valA)
	require.Equal(t, http.StatusOK, recA.Code)

	// Mint B directly against the SAME fake client — simulating a token
	// minted elsewhere after the cache warmed, never going through the
	// middleware itself.
	valB := mustNewTokenValue(t)
	crB := accessTokenCR("at-bbbbbbbbbbbb", valB, "owner-b", now.Add(time.Hour))
	require.NoError(t, c.Create(context.Background(), crB))

	rhB := &recordingHandler{}
	recB := doBearerRequest(mw, rhB.handler(), "Bearer "+valB)
	assert.Equal(t, http.StatusOK, recB.Code, "a token minted after cache warm-up must authenticate via the on-miss re-list")
	assert.True(t, rhB.ran)
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
