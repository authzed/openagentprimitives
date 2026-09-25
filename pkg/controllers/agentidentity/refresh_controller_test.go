package agentidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/backoff"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/identityrefresh"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
)

// ---------- shared helpers ----------

// requireConditionReason fetches a condition by type from an
// AgentIdentity and asserts its Status + (optionally) Reason match.
// Empty reason skips the reason check (matches any).
func requireConditionReason(t *testing.T, a *spiceboxv1alpha1.AgentIdentity, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(a.Status.Conditions, condType)
	require.NotNilf(t, c, "condition %q must be present", condType)
	assert.Equal(t, status, c.Status, "condition %q Status", condType)
	if reason != "" {
		assert.Equal(t, reason, c.Reason, "condition %q Reason", condType)
	}
}

func reconcileOnce(t *testing.T, r *RefreshReconciler, name string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: name},
	})
	require.NoError(t, err, "Reconcile")
	return res
}

// installRefreshClient routes pkg/platform/identity/refresh's package-global
// HTTP client through srv for the test's lifetime.
func installRefreshClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	refresh.SetHTTPClient(srv.Client())
	t.Cleanup(func() { refresh.SetHTTPClient(http.DefaultClient) })
}

// buildOAuthAI returns a single-credential single-binding oauth
// AgentIdentity referencing a Secret with the given expiry offset.
// If tokenEndpoint == "", no token_endpoint key is written.
func buildOAuthAI(secretName, tokenEndpoint string, expiresIn time.Duration, withRefreshToken bool) (*spiceboxv1alpha1.AgentIdentity, *corev1.Secret) {
	data := map[string][]byte{
		"access_token": []byte("at"),
		"expires_at":   []byte(time.Now().Add(expiresIn).UTC().Format(time.RFC3339)),
		"client_id":    []byte("cid"),
	}
	if tokenEndpoint != "" {
		data["token_endpoint"] = []byte(tokenEndpoint)
	}
	if withRefreshToken {
		data["refresh_token"] = []byte("rt")
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
		Data:       data,
	}
	adoptguard.WithAdoptedLabel(sec)
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "o", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName},
				},
			}},
		},
	}
	return ai, sec
}

// oauthCredWithSecret returns one oauth credential plus the Secret it
// references, so a test can compose an identity holding SEVERAL credentials
// with independent token endpoints and expiries. buildOAuthAI covers the
// single-credential shape; this covers the rest.
func oauthCredWithSecret(credName, secretName, tokenEndpoint string, expiresIn time.Duration) (spiceboxv1alpha1.AgentCredential, *corev1.Secret) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
		Data: map[string][]byte{
			"access_token":   []byte("at-" + credName),
			"refresh_token":  []byte("rt-" + credName),
			"expires_at":     []byte(time.Now().Add(expiresIn).UTC().Format(time.RFC3339)),
			"client_id":      []byte("cid-" + credName),
			"token_endpoint": []byte(tokenEndpoint),
		},
	}
	adoptguard.WithAdoptedLabel(sec)
	return spiceboxv1alpha1.AgentCredential{
		Name: credName, Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName},
		},
	}, sec
}

func newReconciler(c client.Client, threshold time.Duration) *RefreshReconciler {
	return &RefreshReconciler{
		Client:           c,
		APIReader:        c,
		SecretReader:     adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
		DefaultThreshold: threshold,
	}
}

func buildClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).
		Build()
}

// ---------- table-driven: classification + status-write paths ----------
//
// These cases exercise the Reconcile loop WITHOUT needing a real token
// endpoint: each path either skips refresh entirely (no-oauth, no-token,
// not-yet-expiring) or short-circuits (deletion). Bundled into one
// suite because they share the same shape: build state, reconcile,
// assert (condition, RequeueAfter).

func TestRefreshReconcile_ClassificationPaths(t *testing.T) {
	cases := []struct {
		name            string
		objects         func() []client.Object
		wantRequeue     time.Duration
		wantCondPresent bool // false → assert no Refresh condition was written (deletion path)
		wantStatus      metav1.ConditionStatus
		wantReason      string
	}{
		{
			name: "no oauth credentials → Refresh=True/NoOAuthCredentials, no requeue",
			objects: func() []client.Object {
				return []client.Object{&spiceboxv1alpha1.AgentIdentity{
					ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: "default"},
					Spec: spiceboxv1alpha1.AgentIdentitySpec{
						Credentials: []spiceboxv1alpha1.AgentCredential{{
							Name: "x", Type: "static",
							Static: &spiceboxv1alpha1.StaticCredentialSource{
								SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "s", Key: "k"},
							},
						}},
					},
				}}
			},
			wantRequeue:     0,
			wantCondPresent: true,
			wantStatus:      metav1.ConditionTrue,
			wantReason:      spiceboxv1alpha1.ReasonNoOAuthCredentials,
		},
		{
			name: "oauth cred missing refresh_token → Refresh=True/NoRefreshToken, no requeue",
			objects: func() []client.Object {
				ai, sec := buildOAuthAI("oauth-sec", "", 1*time.Minute, false)
				return []client.Object{ai, sec}
			},
			wantRequeue:     0,
			wantCondPresent: true,
			wantStatus:      metav1.ConditionTrue,
			wantReason:      spiceboxv1alpha1.ReasonNoRefreshToken,
		},
		{
			name: "oauth cred 20m to expiry vs 5m threshold → Refresh=True/AllOAuthFresh, requeue ≈ 15m",
			objects: func() []client.Object {
				ai, sec := buildOAuthAI("oauth-sec", "https://example.invalid/token", 20*time.Minute, true)
				return []client.Object{ai, sec}
			},
			wantRequeue:     15 * time.Minute, // tolerated within ±15s below
			wantCondPresent: true,
			wantStatus:      metav1.ConditionTrue,
			wantReason:      spiceboxv1alpha1.ReasonAllOAuthFresh,
		},
		{
			name: "DeletionTimestamp set → no-op, no condition write",
			objects: func() []client.Object {
				now := metav1.Now()
				return []client.Object{&spiceboxv1alpha1.AgentIdentity{
					ObjectMeta: metav1.ObjectMeta{
						Name: "ai", Namespace: "default",
						DeletionTimestamp: &now,
						Finalizers:        []string{"keep-ai-around"}, // fake client purges deleting objects without finalizers
					},
				}}
			},
			wantRequeue:     0,
			wantCondPresent: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := buildClient(t, tc.objects()...)
			r := newReconciler(c, 5*time.Minute)
			res := reconcileOnce(t, r, "ai")

			// RequeueAfter check tolerates ±15s skew for the scheduling math.
			if tc.wantRequeue == 0 {
				assert.Equal(t, time.Duration(0), res.RequeueAfter, "RequeueAfter")
			} else {
				assert.InDelta(t, tc.wantRequeue, res.RequeueAfter, float64(15*time.Second),
					"RequeueAfter should be ≈ %v, got %v", tc.wantRequeue, res.RequeueAfter)
			}

			var got spiceboxv1alpha1.AgentIdentity
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "ai"}, &got))
			if tc.wantCondPresent {
				requireConditionReason(t, &got, spiceboxv1alpha1.AgentIdentityConditionRefresh, tc.wantStatus, tc.wantReason)
			} else {
				cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionRefresh)
				assert.Nil(t, cond, "Refresh condition should be absent")
			}
		})
	}
}

// TestRefreshReconcile_ThresholdFor pins the per-identity threshold
// override fallback to DefaultThreshold when unset, through the adapter
// that feeds spec.refreshThreshold to the shared policy.
func TestRefreshReconcile_ThresholdFor(t *testing.T) {
	override := metav1.Duration{Duration: 30 * time.Minute}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentIdentitySpec{RefreshThreshold: &override},
	}
	r := newReconciler(nil, 5*time.Minute)
	assert.Equal(t, override.Duration, r.core().ThresholdFor(refreshTarget{ai}), "should use spec override")
	ai.Spec.RefreshThreshold = nil
	assert.Equal(t, 5*time.Minute, r.core().ThresholdFor(refreshTarget{ai}), "should fall back to DefaultThreshold")
}

// ---------- table-driven: real refresh.Run outcomes ----------
//
// All three cases share setup (Secret 1m from expiry, 5m threshold →
// credAttempt; httptest token endpoint installed via the refresh
// package's test seam). They differ only in the endpoint's response
// and the expected (condition, side-effect) outcome.

func TestRefreshReconcile_RealRefreshOutcomes(t *testing.T) {
	const newAccessToken = "new-at"

	cases := []struct {
		name    string
		handler http.HandlerFunc
		check   func(t *testing.T, c client.Client, r *RefreshReconciler, res reconcile.Result)
	}{
		{
			name: "happy refresh: Secret rotates, Refresh=True/RefreshSucceeded, LastRefreshAt set, clamps requeue",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":  newAccessToken,
					"refresh_token": "new-rt",
					"expires_in":    3600,
					"token_type":    "Bearer",
				})
			},
			check: func(t *testing.T, c client.Client, r *RefreshReconciler, res reconcile.Result) {
				// (1m − 5m) = −4m → clamped to the floor.
				assert.Equal(t, identityrefresh.MinRequeueFloor, res.RequeueAfter, "RequeueAfter must clamp to floor")

				var gotAI spiceboxv1alpha1.AgentIdentity
				require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "ai"}, &gotAI))
				requireConditionReason(t, &gotAI, spiceboxv1alpha1.AgentIdentityConditionRefresh,
					metav1.ConditionTrue, spiceboxv1alpha1.ReasonRefreshSucceeded)
				assert.NotNil(t, gotAI.Status.LastRefreshAt, "LastRefreshAt should be set after a successful refresh")

				var gotSec corev1.Secret
				require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "oauth-sec"}, &gotSec))
				assert.Equal(t, newAccessToken, string(gotSec.Data["access_token"]), "access_token must rotate")

				assert.Equal(t, time.Duration(0),
					r.core().Backoff.NextDelay(identityrefresh.BackoffKey("default", "ai", "o")),
					"backoff should be cleared on success")
			},
		},
		{
			name: "5xx failure: Refresh=False/TokenEndpointError, RequeueAfter == backoff.BaseDelay, backoff entry recorded",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"backend_outage"}`))
			},
			check: func(t *testing.T, c client.Client, r *RefreshReconciler, res reconcile.Result) {
				var gotAI spiceboxv1alpha1.AgentIdentity
				require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "ai"}, &gotAI))
				requireConditionReason(t, &gotAI, spiceboxv1alpha1.AgentIdentityConditionRefresh,
					metav1.ConditionFalse, spiceboxv1alpha1.ReasonTokenEndpointError)
				// Pinned to the base delay, not just "> 0": MinRequeueFloor
				// (15s) is also > 0, and it is what a failure requeues at if
				// the ≤ 0 expiresAt-threshold candidate ever beats the backoff
				// in computeRequeueAfter's min. "> 0" cannot tell them apart.
				// The tolerance covers the round trip: the requeue is the
				// backoff REMAINDER on the real clock, so it is a hair under
				// BaseDelay — still nowhere near the floor.
				assert.InDelta(t, backoff.BaseDelay, res.RequeueAfter, float64(time.Second),
					"a first failure must requeue on the backoff's base delay")
				assert.NotEqual(t, time.Duration(0),
					r.core().Backoff.NextDelay(identityrefresh.BackoffKey("default", "ai", "o")),
					"backoff entry should be recorded on failure")
			},
		},
		{
			name: "malformed response: Refresh=False/RefreshResponseInvalid",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`not-json`))
			},
			check: func(t *testing.T, c client.Client, r *RefreshReconciler, res reconcile.Result) {
				var gotAI spiceboxv1alpha1.AgentIdentity
				require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "ai"}, &gotAI))
				requireConditionReason(t, &gotAI, spiceboxv1alpha1.AgentIdentityConditionRefresh,
					metav1.ConditionFalse, spiceboxv1alpha1.ReasonRefreshResponseInvalid)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			installRefreshClient(t, srv)

			ai, sec := buildOAuthAI("oauth-sec", srv.URL, 1*time.Minute, true)
			c := buildClient(t, ai, sec)
			r := newReconciler(c, 5*time.Minute)
			res := reconcileOnce(t, r, "ai")

			tc.check(t, c, r, res)
		})
	}
}

// TestRefreshReconcile_PermanentFailureBacksOff pins the exponential
// backoff of a credential the provider has permanently rejected — the
// revoked-refresh_token case, where every pass fails and no pass will
// ever succeed without a human re-linking the credential.
//
// The hazard this pins: `time.Until(expiresAt) - threshold` is ≤ 0 for
// any credAttempt by construction, since classify only produces one when
// expires_at is already inside the threshold. Were computeRequeueAfter
// to offer that candidate on the failure path, it would win the min,
// clamp to MinRequeueFloor, and leave the recorded (and logged) backoff
// unused — the operator would re-hit the provider's token endpoint every
// 15s forever, 20× the intended steady-state rate.
//
// Sequential reconciles also demonstrate that pkg/platform/identity/refresh's
// single-flight bounds nothing here: it collapses only CONCURRENT
// redemptions, so each SCHEDULED pass issues its own POST. Passes are
// scheduled by advancing an injected clock: the attempt itself is gated on the
// backoff, so a pass arriving early makes no round trip — the closing
// assertions pin that half, which is what keeps an unscheduled trigger (a
// sibling Secret write, a spec edit, a resync) from bypassing the backoff.
func TestRefreshReconcile_PermanentFailureBacksOff(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	t.Cleanup(srv.Close)
	installRefreshClient(t, srv)

	ai, sec := buildOAuthAI("oauth-sec", srv.URL, 1*time.Minute, true)
	c := buildClient(t, ai, sec)
	r := newReconciler(c, 5*time.Minute)

	now := time.Now()
	r.core().Backoff.SetClock(func() time.Time { return now })

	// backoff.BaseDelay doubling, capped at the reconciler's threshold (5m).
	want := []time.Duration{
		backoff.BaseDelay,
		2 * backoff.BaseDelay,
		4 * backoff.BaseDelay,
		8 * backoff.BaseDelay,
		5 * time.Minute, // cap
	}
	var scheduled time.Duration // what the previous pass asked to wait
	for i, w := range want {
		now = now.Add(scheduled) // arrive exactly when that wait elapsed
		res := reconcileOnce(t, r, "ai")
		assert.Equalf(t, w, res.RequeueAfter,
			"reconcile #%d: RequeueAfter must follow the backoff, not clamp to MinRequeueFloor (%v)",
			i+1, identityrefresh.MinRequeueFloor)
		scheduled = res.RequeueAfter
	}
	assert.Equal(t, int64(len(want)), posts.Load(),
		"every scheduled reconcile must have made its own token-endpoint round trip")

	// An unscheduled trigger, arriving with the full cap still to run.
	early := reconcileOnce(t, r, "ai")
	assert.Equal(t, int64(len(want)), posts.Load(),
		"a reconcile inside the backoff must not reach the provider at all")
	assert.Equal(t, 5*time.Minute, early.RequeueAfter,
		"a deferred credential reschedules at what is left of its backoff")

	// Halfway through the cap: still deferred, and the schedule tracks the
	// remainder rather than restarting the full delay.
	now = now.Add(150 * time.Second)
	half := reconcileOnce(t, r, "ai")
	assert.Equal(t, int64(len(want)), posts.Load(), "still inside the backoff")
	assert.Equal(t, 150*time.Second, half.RequeueAfter, "requeue must track the remainder")
}

// TestRefreshReconcile_SiblingSuccessDoesNotDefeatBackoff is the
// two-credential case the single-credential backoff tests cannot see.
//
// An identity holding a revoked credential AND a healthy one inside the
// threshold reconciles both in one pass. The healthy refresh writes its Secret,
// and that write re-enqueues the identity through the Secret→AgentIdentity
// watch immediately — so the revoked credential is offered a fresh attempt with
// no delay at all, and its expires_at never advances (a failed refresh leaves
// the Secret untouched), so it stays credAttempt forever. Unless the attempt
// itself is gated, the operator hammers the provider's token endpoint at the
// healthy credential's cadence.
//
// RequeueAfter cannot carry that gate, and the assertions below show why:
// computeRequeueAfter's SUCCESS candidate is MinRequeueFloor (the pre-refresh
// expiresAt it is derived from is stale, and stale-minus-threshold is ≤ 0 for
// any credAttempt by construction), so it wins the min while the sibling's 30s
// backoff sits unused. The gate has to be on the ATTEMPT.
func TestRefreshReconcile_SiblingSuccessDoesNotDefeatBackoff(t *testing.T) {
	var revokedPosts, healthyPosts atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/revoked", func(w http.ResponseWriter, r *http.Request) {
		revokedPosts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	mux.HandleFunc("/healthy", func(w http.ResponseWriter, r *http.Request) {
		healthyPosts.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-at",
			"refresh_token": "fresh-rt",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	installRefreshClient(t, srv)

	revokedCred, revokedSec := oauthCredWithSecret("revoked", "sec-revoked", srv.URL+"/revoked", 1*time.Minute)
	healthyCred, healthySec := oauthCredWithSecret("healthy", "sec-healthy", srv.URL+"/healthy", 1*time.Minute)
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{revokedCred, healthyCred},
		},
	}
	c := buildClient(t, ai, revokedSec, healthySec)
	r := newReconciler(c, 5*time.Minute)
	revokedKey := identityrefresh.BackoffKey("default", "ai", "revoked")

	first := reconcileOnce(t, r, "ai")
	require.Equal(t, int64(1), revokedPosts.Load(), "first pass must attempt the revoked credential once")
	require.Equal(t, int64(1), healthyPosts.Load(), "first pass must attempt the healthy credential once")
	assert.Equal(t, backoff.BaseDelay, r.core().Backoff.NextDelay(revokedKey),
		"the revoked credential's failure must record a backoff")
	assert.Equal(t, identityrefresh.MinRequeueFloor, first.RequeueAfter,
		"the sibling success's ≤ 0 candidate wins the min and clamps to the floor — "+
			"which is exactly why the backoff cannot be enforced through RequeueAfter alone")

	// The healthy credential's Secret write re-enqueues the identity through
	// the Secret watch, with no delay at all. The revoked credential's backoff
	// has not elapsed, so it must NOT be re-attempted.
	second := reconcileOnce(t, r, "ai")
	assert.Equal(t, int64(1), revokedPosts.Load(),
		"a credential whose backoff has not elapsed must not be re-attempted")
	assert.Equal(t, int64(1), healthyPosts.Load(),
		"the healthy credential now expires an hour out; it must not be re-attempted either")
	assert.Equal(t, backoff.BaseDelay, r.core().Backoff.NextDelay(revokedKey),
		"a skipped attempt is not a failure; it must not escalate the backoff")
	assert.InDelta(t, backoff.BaseDelay, second.RequeueAfter, float64(2*time.Second),
		"the deferred credential must schedule its own retry at the backoff remainder")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "ai"}, &got))
	requireConditionReason(t, &got, spiceboxv1alpha1.AgentIdentityConditionRefresh,
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonTokenEndpointError)
}

// TestRefreshReconcile_MapSecretToAgents pins the Secret→AgentIdentity
// watch mapping: only same-namespace identities that reference the Secret
// as an oauth credential are enqueued.
func TestRefreshReconcile_MapSecretToAgents(t *testing.T) {
	ai, sec := buildOAuthAI("oauth-sec", "", 20*time.Minute, true)
	other := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: "other"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "o", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: "oauth-sec"},
				},
			}},
		},
	}
	c := buildClient(t, ai, sec, other)
	r := newReconciler(c, 5*time.Minute)

	cases := []struct {
		name      string
		secret    *corev1.Secret
		wantNS    string
		wantNames []string
	}{
		{
			name:      "referenced Secret → enqueues only the same-namespace AgentIdentity",
			secret:    sec,
			wantNS:    "default",
			wantNames: []string{"ai"},
		},
		{
			name: "same-named Secret in another namespace → enqueues only that namespace's identity",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "oauth-sec", Namespace: "other"},
			},
			wantNS:    "other",
			wantNames: []string{"ai"},
		},
		{
			name: "unreferenced Secret → enqueues nothing",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"},
			},
			wantNames: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs := r.mapSecretToAgents(context.Background(), tc.secret)
			var got []string
			for _, rq := range reqs {
				assert.Equal(t, tc.wantNS, rq.Namespace, "enqueued request namespace")
				got = append(got, rq.Name)
			}
			assert.Equal(t, tc.wantNames, got, "enqueued AgentIdentity names")
		})
	}
}
