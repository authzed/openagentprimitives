package useridentity

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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/backoff"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/identityrefresh"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
)

// ---------- shared helpers ----------

// requireRefreshCondition fetches a condition by type from a UserIdentity
// and asserts its Status + (optionally) Reason match. Empty reason skips
// the reason check (matches any).
func requireRefreshCondition(t *testing.T, u *spiceboxv1alpha1.UserIdentity, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(u.Status.Conditions, condType)
	require.NotNilf(t, c, "condition %q must be present", condType)
	assert.Equal(t, status, c.Status, "condition %q Status", condType)
	if reason != "" {
		assert.Equal(t, reason, c.Reason, "condition %q Reason", condType)
	}
}

// refreshOnce drives the RefreshReconciler once against the cluster-scoped
// UserIdentity named name.
func refreshOnce(t *testing.T, r *RefreshReconciler, name string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Name: name},
	})
	require.NoError(t, err, "Reconcile")
	return res
}

// installRefreshClient routes pkg/platform/identity/refresh's package-global HTTP
// client through srv for the test's lifetime. Tests calling it must NOT
// run in parallel with each other (package-level mutation).
func installRefreshClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	refresh.SetHTTPClient(srv.Client())
	t.Cleanup(func() { refresh.SetHTTPClient(http.DefaultClient) })
}

// buildOAuthUI returns a single-credential oauth UserIdentity plus the
// Secret it references, in IdentitiesNamespace, with the given expiry
// offset. If tokenEndpoint == "", no token_endpoint key is written.
func buildOAuthUI(secretName, tokenEndpoint string, expiresIn time.Duration, withRefreshToken bool) (*spiceboxv1alpha1.UserIdentity, *corev1.Secret) {
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
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data:       data,
	}
	adoptguard.WithAdoptedLabel(sec)
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-x"},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: "user:abc",
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "o", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName},
				},
			}},
		},
	}
	return ui, sec
}

func newRefreshReconciler(c client.Client, threshold time.Duration) *RefreshReconciler {
	return &RefreshReconciler{
		Client:           c,
		APIReader:        c,
		SecretReader:     adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
		DefaultThreshold: threshold,
	}
}

// ---------- table-driven: classification + status-write paths ----------
//
// These cases exercise the Reconcile loop WITHOUT needing a real token
// endpoint: each path either skips refresh entirely (no-oauth, no-token,
// not-yet-expiring) or short-circuits (deletion). Bundled into one suite
// because they share the same shape: build state, reconcile, assert
// (condition, RequeueAfter).

func TestUserIdentityRefresh_ClassificationPaths(t *testing.T) {
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
				return []client.Object{&spiceboxv1alpha1.UserIdentity{
					ObjectMeta: metav1.ObjectMeta{Name: "u-x"},
					Spec: spiceboxv1alpha1.UserIdentitySpec{
						Subject: "user:abc",
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
				ui, sec := buildOAuthUI("u-x-o", "", 1*time.Minute, false)
				return []client.Object{ui, sec}
			},
			wantRequeue:     0,
			wantCondPresent: true,
			wantStatus:      metav1.ConditionTrue,
			wantReason:      spiceboxv1alpha1.ReasonNoRefreshToken,
		},
		{
			name: "oauth cred 20m to expiry vs 5m threshold → Refresh=True/AllOAuthFresh, requeue ≈ 15m",
			objects: func() []client.Object {
				ui, sec := buildOAuthUI("u-x-o", "https://example.invalid/token", 20*time.Minute, true)
				return []client.Object{ui, sec}
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
				return []client.Object{&spiceboxv1alpha1.UserIdentity{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "u-x",
						DeletionTimestamp: &now,
						Finalizers:        []string{"keep-ui-around"}, // fake client purges deleting objects without finalizers
					},
					Spec: spiceboxv1alpha1.UserIdentitySpec{Subject: "user:abc"},
				}}
			},
			wantRequeue:     0,
			wantCondPresent: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := buildClient(t, tc.objects()...)
			r := newRefreshReconciler(c, 5*time.Minute)
			res := refreshOnce(t, r, "u-x")

			// RequeueAfter check tolerates ±15s skew for the scheduling math.
			if tc.wantRequeue == 0 {
				assert.Equal(t, time.Duration(0), res.RequeueAfter, "RequeueAfter")
			} else {
				assert.InDelta(t, tc.wantRequeue, res.RequeueAfter, float64(15*time.Second),
					"RequeueAfter should be ≈ %v, got %v", tc.wantRequeue, res.RequeueAfter)
			}

			var got spiceboxv1alpha1.UserIdentity
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "u-x"}, &got))
			if tc.wantCondPresent {
				requireRefreshCondition(t, &got, spiceboxv1alpha1.UserIdentityConditionRefresh, tc.wantStatus, tc.wantReason)
			} else {
				cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.UserIdentityConditionRefresh)
				assert.Nil(t, cond, "Refresh condition should be absent")
			}
		})
	}
}

// TestUserIdentityRefresh_ThresholdFor pins the per-identity threshold
// override and the fallback to DefaultThreshold when unset.
func TestUserIdentityRefresh_ThresholdFor(t *testing.T) {
	override := metav1.Duration{Duration: 30 * time.Minute}
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-x"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:abc", RefreshThreshold: &override},
	}
	r := newRefreshReconciler(nil, 5*time.Minute)
	assert.Equal(t, override.Duration, r.core().ThresholdFor(refreshTarget{ui}), "should use spec override")
	ui.Spec.RefreshThreshold = nil
	assert.Equal(t, 5*time.Minute, r.core().ThresholdFor(refreshTarget{ui}), "should fall back to DefaultThreshold")
}

// ---------- table-driven: real refresh.Run outcomes ----------
//
// All three cases share setup (Secret 1m from expiry, 5m threshold →
// credAttempt; httptest token endpoint installed via the refresh
// package's test seam). They differ only in the endpoint's response and
// the expected (condition, side-effect) outcome.

func TestUserIdentityRefresh_RealRefreshOutcomes(t *testing.T) {
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

				var gotUI spiceboxv1alpha1.UserIdentity
				require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "u-x"}, &gotUI))
				requireRefreshCondition(t, &gotUI, spiceboxv1alpha1.UserIdentityConditionRefresh,
					metav1.ConditionTrue, spiceboxv1alpha1.ReasonRefreshSucceeded)
				assert.NotNil(t, gotUI.Status.LastRefreshAt, "LastRefreshAt should be set after a successful refresh")

				var gotSec corev1.Secret
				require.NoError(t, c.Get(context.Background(),
					client.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: "u-x-o"}, &gotSec))
				assert.Equal(t, newAccessToken, string(gotSec.Data["access_token"]), "access_token must rotate")

				assert.Equal(t, time.Duration(0),
					r.core().Backoff.NextDelay(identityrefresh.BackoffKey("", "u-x", "o")),
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
				var gotUI spiceboxv1alpha1.UserIdentity
				require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "u-x"}, &gotUI))
				requireRefreshCondition(t, &gotUI, spiceboxv1alpha1.UserIdentityConditionRefresh,
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
					r.core().Backoff.NextDelay(identityrefresh.BackoffKey("", "u-x", "o")),
					"backoff entry should be recorded on failure")
			},
		},
		{
			name: "malformed response: Refresh=False/RefreshResponseInvalid",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`not-json`))
			},
			check: func(t *testing.T, c client.Client, r *RefreshReconciler, res reconcile.Result) {
				var gotUI spiceboxv1alpha1.UserIdentity
				require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "u-x"}, &gotUI))
				requireRefreshCondition(t, &gotUI, spiceboxv1alpha1.UserIdentityConditionRefresh,
					metav1.ConditionFalse, spiceboxv1alpha1.ReasonRefreshResponseInvalid)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			installRefreshClient(t, srv)

			ui, sec := buildOAuthUI("u-x-o", srv.URL, 1*time.Minute, true)
			c := buildClient(t, ui, sec)
			r := newRefreshReconciler(c, 5*time.Minute)
			res := refreshOnce(t, r, "u-x")

			tc.check(t, c, r, res)
		})
	}
}

// TestUserIdentityRefresh_PermanentFailureBacksOff is the UserIdentity twin
// of the AgentIdentity backoff test: both kinds share one Core, so both must
// pin that a permanently-rejected credential (revoked refresh_token) retries
// on the exponential backoff rather than at MinRequeueFloor, and that a pass
// arriving before the backoff elapsed makes no round trip at all. See the
// AgentIdentity test for the full defect description.
func TestUserIdentityRefresh_PermanentFailureBacksOff(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	t.Cleanup(srv.Close)
	installRefreshClient(t, srv)

	ui, sec := buildOAuthUI("u-x-o", srv.URL, 1*time.Minute, true)
	c := buildClient(t, ui, sec)
	r := newRefreshReconciler(c, 5*time.Minute)

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
		res := refreshOnce(t, r, "u-x")
		assert.Equalf(t, w, res.RequeueAfter,
			"reconcile #%d: RequeueAfter must follow the backoff, not clamp to MinRequeueFloor (%v)",
			i+1, identityrefresh.MinRequeueFloor)
		scheduled = res.RequeueAfter
	}
	assert.Equal(t, int64(len(want)), posts.Load(),
		"every scheduled reconcile must have made its own token-endpoint round trip")

	// An unscheduled trigger inside the backoff must not reach the provider.
	early := refreshOnce(t, r, "u-x")
	assert.Equal(t, int64(len(want)), posts.Load(),
		"a reconcile inside the backoff must not reach the provider at all")
	assert.Equal(t, 5*time.Minute, early.RequeueAfter,
		"a deferred credential reschedules at what is left of its backoff")
}

// TestUserIdentityRefresh_MapSecretToUsers pins the Secret→UserIdentity
// watch mapping, including the namespace pre-filter that keeps a
// same-named Secret in an unrelated namespace from enqueueing every
// UserIdentity that references that name.
func TestUserIdentityRefresh_MapSecretToUsers(t *testing.T) {
	ui, sec := buildOAuthUI("u-x-o", "", 20*time.Minute, true)
	c := buildClient(t, ui, sec)
	r := newRefreshReconciler(c, 5*time.Minute)

	cases := []struct {
		name      string
		secret    *corev1.Secret
		wantNames []string
	}{
		{
			name:      "referenced Secret in IdentitiesNamespace → enqueues its UserIdentity",
			secret:    sec,
			wantNames: []string{"u-x"},
		},
		{
			name: "same-named Secret in another namespace → enqueues nothing",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "u-x-o", Namespace: "default"},
			},
			wantNames: nil,
		},
		{
			name: "unreferenced Secret in IdentitiesNamespace → enqueues nothing",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: spiceboxv1alpha1.IdentitiesNamespace},
			},
			wantNames: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs := r.mapSecretToUsers(context.Background(), tc.secret)
			var got []string
			for _, rq := range reqs {
				got = append(got, rq.Name)
			}
			assert.Equal(t, tc.wantNames, got, "enqueued UserIdentity names")
		})
	}
}

// TestUserIdentityRefresh_MigratesCoLocatedRedemptionMaterial pins the upgrade
// half of the Secret split.
//
// Splitting the credential into a token master and a redemption-material
// sibling only buys anything if EXISTING credentials get split too. A master
// written before the split still carries token_endpoint / client_id /
// client_secret alongside the tokens, and a userPassthrough runner is granted
// `get` on that master by name — a Kubernetes read returns every key — so for
// every pre-split credential the runner holds a self-contained, offline-usable
// refresh grant for the user's upstream account, exactly what the split exists
// to deny it. Nothing else migrates: PutOAuthToken strips the co-located keys
// only on a manual re-link.
//
// The reconciler is the migration's home because it is the one component that
// can read both Secrets and write both. Ordering is the load-bearing part: the
// sibling must be durably written BEFORE the master is stripped, or an
// interrupted migration destroys the only copy of the redemption material.
func TestUserIdentityRefresh_MigratesCoLocatedRedemptionMaterial(t *testing.T) {
	ctx := context.Background()
	// Expiry well outside the threshold: this reconcile schedules, it does not
	// refresh, so the migration is observed on its own with no token endpoint.
	ui, sec := buildOAuthUI("u-x-o", "https://idp.example.invalid/token", 2*time.Hour, true)
	sec.Data["client_secret"] = []byte("cs")
	c := buildClient(t, ui, sec)
	r := newRefreshReconciler(c, time.Hour)

	refreshOnce(t, r, "u-x")

	var master corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: "u-x-o",
	}, &master))
	for _, k := range []string{"token_endpoint", "client_id", "client_secret"} {
		assert.NotContainsf(t, master.Data, k,
			"the runner is granted `get` on this Secret; %q must not survive on it", k)
	}
	assert.Equal(t, []byte("rt"), master.Data["refresh_token"], "the tokens must stay on the master")
	assert.Equal(t, []byte("at"), master.Data["access_token"], "the tokens must stay on the master")

	var sib corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: refresh.MaterialSecretName("u-x-o"),
	}, &sib), "the redemption material must be moved, not dropped")
	assert.Equal(t, []byte("https://idp.example.invalid/token"), sib.Data["token_endpoint"])
	assert.Equal(t, []byte("cid"), sib.Data["client_id"])
	assert.Equal(t, []byte("cs"), sib.Data["client_secret"])
	assert.Contains(t, sib.Labels, refresh.MaterialSecretLabel,
		"the derived name is a hint; the label is what makes the lookup safe")
	assert.Contains(t, sib.Labels, adoptguard.AdoptedLabel,
		"the operator's Secret informer is label-filtered; without this its own refresh path cannot read the sibling")
	require.Len(t, sib.OwnerReferences, 1, "unlinking the credential must reap the redemption material")
	assert.Equal(t, "u-x-o", sib.OwnerReferences[0].Name)

	// Idempotent: a second pass finds nothing to move and changes nothing.
	refreshOnce(t, r, "u-x")
	var sibAgain corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: refresh.MaterialSecretName("u-x-o"),
	}, &sibAgain))
	assert.Equal(t, sib.ResourceVersion, sibAgain.ResourceVersion,
		"a migrated credential must not be rewritten on every reconcile")
}
