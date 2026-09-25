package clusteridentityprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/googlekind"   // register "google" kind for registry.Get in tests
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind"     // register "oidc" kind for registry.Get in tests
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind" // register "password" kind for registry.Get in tests
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

// baseSpec returns a well-formed ClusterIdentityProviderSpec that the
// webhook would accept.
func baseSpec(issuerURL string) v1.ClusterIdentityProviderSpec {
	return v1.ClusterIdentityProviderSpec{
		Kind:                "oidc",
		Issuer:              issuerURL,
		ClientID:            "test-client",
		ClientSecretRef:     v1.ClusterSecretKeyRef{Namespace: "default", Name: "idp-secret", Key: "secret"},
		AllowedEmailDomains: []string{"example.com"},
	}
}

// reconcileOnce builds the fake client, creates the CR (and optionally the
// Secret), wires the test's http.Client, and runs one reconcile. It returns
// the result and the CR post-reconcile.
//
// The helper pre-adopts any supplied secret (stamps AdoptedLabel) and wires
// a SecretReader (Warn mode, no allowlist) so the reconciler's adopt+read
// path exercises the guarded reader. localCluster is threaded straight to
// Reconciler.LocalCluster (the gate that fences local-only idp.Kinds like
// password off of non-local clusters).
func reconcileOnce(
	t *testing.T,
	cr *v1.ClusterIdentityProvider,
	secret *corev1.Secret,
	httpClient *http.Client,
	localCluster bool,
) (ctrl.Result, v1.ClusterIdentityProvider) {
	t.Helper()

	// Pre-stamp the adoption label so the fake cache returns it as adopted.
	if secret != nil {
		adoptguard.WithAdoptedLabel(secret)
	}

	objs := []runtime.Object{cr}
	if secret != nil {
		objs = append(objs, secret)
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&v1.ClusterIdentityProvider{}).
		Build()

	// Construct a SecretReader (Warn, no allowlist) backed by the fake client.
	// The fake client serves as both the cache and the API reader in tests.
	secretReader := adoptguard.NewSecretReader(
		c, c, adoptguard.Warn,
		func(types.NamespacedName) bool { return false },
	)

	r := &Reconciler{
		Client:       c,
		HTTPClient:   httpClient,
		SecretReader: secretReader,
		LocalCluster: localCluster,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1.ClusterIdentityProviderName},
	})
	require.NoError(t, err, "Reconcile must not return a hard error")

	var got v1.ClusterIdentityProvider
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: v1.ClusterIdentityProviderName}, &got),
		"Get after Reconcile")

	return res, got
}

func TestReconciler(t *testing.T) {
	cases := []struct {
		name             string
		cr               *v1.ClusterIdentityProvider
		secret           *corev1.Secret
		httpClient       func(srv *httptest.Server) *http.Client
		wantStatus       metav1.ConditionStatus
		wantReason       string
		wantRequeueAfter time.Duration // exact expected RequeueAfter; 0 means no requeue
	}{
		{
			name: "spec invalid (no domains) → Valid=False/ConfigInvalid",
			cr: &v1.ClusterIdentityProvider{
				ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
				Spec: v1.ClusterIdentityProviderSpec{
					Kind:            "oidc",
					Issuer:          "https://issuer.example.com",
					ClientID:        "client-id",
					ClientSecretRef: v1.ClusterSecretKeyRef{Namespace: "default", Name: "s", Key: "k"},
					// AllowedEmailDomains empty + AllowAnyEmail false → invalid
				},
			},
			secret:           nil,
			httpClient:       nil,
			wantStatus:       metav1.ConditionFalse,
			wantReason:       v1.ReasonIdPConfigInvalid,
			wantRequeueAfter: 0,
		},
		{
			name: "unknown kind → Valid=False/ConfigInvalid (registry dispatch, no matching Kind)",
			cr: &v1.ClusterIdentityProvider{
				ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
				Spec: v1.ClusterIdentityProviderSpec{
					Kind:            "not-a-real-kind",
					Issuer:          "https://issuer.example.com",
					ClientID:        "client-id",
					ClientSecretRef: v1.ClusterSecretKeyRef{Namespace: "default", Name: "s", Key: "k"},
					AllowAnyEmail:   true,
				},
			},
			secret:           nil,
			httpClient:       nil,
			wantStatus:       metav1.ConditionFalse,
			wantReason:       v1.ReasonIdPConfigInvalid,
			wantRequeueAfter: 0,
		},
		{
			name: "secret missing → Valid=False/SecretMissing, RequeueAfter=30s",
			cr: &v1.ClusterIdentityProvider{
				ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
				Spec:       baseSpec("https://issuer.example.com"),
			},
			secret:           nil, // deliberately absent
			httpClient:       nil,
			wantStatus:       metav1.ConditionFalse,
			wantReason:       v1.ReasonIdPSecretMissing,
			wantRequeueAfter: 30 * time.Second,
		},
		{
			name: "secret key missing → Valid=False/SecretMissing, RequeueAfter=30s",
			cr: &v1.ClusterIdentityProvider{
				ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
				Spec:       baseSpec("https://issuer.example.com"),
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "idp-secret"},
				Data: map[string][]byte{
					"wrong-key": []byte("value"),
					// "secret" key is absent
				},
			},
			httpClient:       nil,
			wantStatus:       metav1.ConditionFalse,
			wantReason:       v1.ReasonIdPSecretMissing,
			wantRequeueAfter: 30 * time.Second,
		},
		{
			name: "oidc discovery 500 → Valid=False/DiscoveryFailed, RequeueAfter=1m",
			cr: &v1.ClusterIdentityProvider{
				ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "idp-secret"},
				Data:       map[string][]byte{"secret": []byte("s3cr3t")},
			},
			httpClient: func(srv *httptest.Server) *http.Client {
				return &http.Client{
					Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						rec := httptest.NewRecorder()
						rec.WriteHeader(http.StatusInternalServerError)
						return rec.Result(), nil
					}),
				}
			},
			wantStatus:       metav1.ConditionFalse,
			wantReason:       v1.ReasonIdPDiscoveryFailed,
			wantRequeueAfter: time.Minute,
		},
		{
			name: "happy oidc → Valid=True/Ready",
			cr: &v1.ClusterIdentityProvider{
				ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "idp-secret"},
				Data:       map[string][]byte{"secret": []byte("s3cr3t")},
			},
			httpClient: func(srv *httptest.Server) *http.Client {
				return &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					body, _ := json.Marshal(map[string]string{"issuer": srv.URL})
					rec := httptest.NewRecorder()
					rec.WriteHeader(http.StatusOK)
					rec.Write(body) //nolint:errcheck
					return rec.Result(), nil
				})}
			},
			wantStatus:       metav1.ConditionTrue,
			wantReason:       v1.ReasonIdPReady,
			wantRequeueAfter: 0,
		},
	}

	// Case 4 and 5 need a spec with the srv.URL as issuer, built per-test.
	// We fix those up below in the server-dependent loop.
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var srv *httptest.Server
			if tc.httpClient != nil {
				// Start a dummy server; its URL is used for issuer in the
				// spec for cases that need server-side issuer resolution.
				srv = httptest.NewServer(http.NotFoundHandler())
				t.Cleanup(srv.Close)

				// Patch the issuer in the spec to the test server URL for
				// cases that reach the discovery step.
				if cases[i].cr.Spec.Kind == "" {
					// Hasn't been set yet: use baseSpec with srv URL.
					cases[i].cr.Spec = baseSpec(srv.URL)
				}
			}

			var hc *http.Client
			if tc.httpClient != nil {
				hc = tc.httpClient(srv)
			}

			// All cases in this table use kind=oidc (AllowedNonLocal()==true), so
			// LocalCluster=false must never reject them via the new local-only
			// gate — this doubles as the "oidc + LocalCluster=false → NOT
			// rejected by this rule" coverage.
			res, got := reconcileOnce(t, tc.cr, tc.secret, hc, false)

			cond := conditions.Find(got.Status.Conditions, v1.ConditionIdPValid)
			require.NotNil(t, cond, "Valid condition must be set")
			assert.Equal(t, tc.wantStatus, cond.Status, "condition Status")
			assert.Equal(t, tc.wantReason, cond.Reason, "condition Reason")
			assert.Equal(t, tc.wantRequeueAfter, res.RequeueAfter, "RequeueAfter")
		})
	}
}

// TestDiscovery_SSRFGuardedByDefault proves the production HTTP client (no
// injected test client) routes discovery through the SSRF guard: a loopback
// issuer is refused before any dial, surfacing as a discovery failure. Gates
// 1+2 pass (valid spec + present secret) so reconcile reaches discovery.
func TestDiscovery_SSRFGuardedByDefault(t *testing.T) {
	cr := &v1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
		Spec:       baseSpec("http://127.0.0.1:1"), // loopback — the guard must refuse it
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "idp-secret"},
		Data:       map[string][]byte{"secret": []byte("s3cr3t")},
	}

	_, got := reconcileOnce(t, cr, secret, nil, false) // nil → production safehttp client

	cond := conditions.Find(got.Status.Conditions, v1.ConditionIdPValid)
	require.NotNil(t, cond, "Valid condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v1.ReasonIdPDiscoveryFailed, cond.Reason)
	assert.Contains(t, cond.Message, "blocked", "discovery must be refused by the SSRF guard, not dialed")
}

// TestReconciler_GoogleKind verifies that for kind=google the discovery
// request is sent to accounts.google.com regardless of spec.Issuer (which
// must be empty for google). It also runs with LocalCluster=false to prove
// google (AllowedNonLocal()==true) is NOT rejected by the local-only gate on
// a non-local cluster.
func TestReconciler_GoogleKind(t *testing.T) {
	var recordedHost string
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		recordedHost = req.URL.Host
		body, _ := json.Marshal(map[string]string{"issuer": "https://accounts.google.com"})
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusOK)
		rec.Write(body) //nolint:errcheck
		return rec.Result(), nil
	})

	cr := &v1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
		Spec: v1.ClusterIdentityProviderSpec{
			Kind:                "google",
			Issuer:              "", // must be empty for google; pinned by the kind
			ClientID:            "google-client",
			ClientSecretRef:     v1.ClusterSecretKeyRef{Namespace: "default", Name: "idp-secret", Key: "secret"},
			AllowedEmailDomains: []string{"example.com"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "idp-secret"},
		Data:       map[string][]byte{"secret": []byte("s3cr3t")},
	}

	hc := &http.Client{Transport: rt}
	_, got := reconcileOnce(t, cr, secret, hc, false) // LocalCluster=false — google must still pass

	cond := conditions.Find(got.Status.Conditions, v1.ConditionIdPValid)
	require.NotNil(t, cond, "Valid condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, v1.ReasonIdPReady, cond.Reason)
	assert.Equal(t, "accounts.google.com", recordedHost,
		"google kind must probe accounts.google.com, not a custom issuer")
}

// TestReconciler_PasswordKind covers the local, non-federated password IdP:
// gate 6 (OIDC discovery) must be skipped entirely — no HTTPClient is wired
// at all, so a reconcile that somehow tried to reach it would nil-panic —
// and Valid=True must follow directly from a resolvable Secret plus
// allowAnyEmail=true, PROVIDED the cluster is local. Every case here sets
// localCluster=true except the dedicated "non-local cluster" case, which
// proves the new local-only gate (gate 3) rejects password outright — a
// public-cluster password CR is provably Valid=False regardless of how
// well-formed the rest of its spec is.
func TestReconciler_PasswordKind(t *testing.T) {
	cases := []struct {
		name             string
		spec             v1.ClusterIdentityProviderSpec
		secret           *corev1.Secret
		localCluster     bool
		wantStatus       metav1.ConditionStatus
		wantReason       string
		wantRequeueAfter time.Duration
		wantMsgContains  string
	}{
		{
			name: "well-formed password CR + hash secret present + LocalCluster=true → Valid=True/Ready",
			spec: v1.ClusterIdentityProviderSpec{
				Kind:            "password",
				ClientID:        "admin@ap.local",
				ClientSecretRef: v1.ClusterSecretKeyRef{Namespace: "default", Name: "spicebox-admin-password", Key: "hash"},
				AllowAnyEmail:   true,
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "spicebox-admin-password"},
				Data:       map[string][]byte{"hash": []byte("$2a$12$abcdefghijklmnopqrstuv")},
			},
			localCluster:     true,
			wantStatus:       metav1.ConditionTrue,
			wantReason:       v1.ReasonIdPReady,
			wantRequeueAfter: 0,
		},
		{
			name: "well-formed password CR + LocalCluster=false → Valid=False/ConfigInvalid (local-only gate)",
			spec: v1.ClusterIdentityProviderSpec{
				Kind:            "password",
				ClientID:        "admin@ap.local",
				ClientSecretRef: v1.ClusterSecretKeyRef{Namespace: "default", Name: "spicebox-admin-password", Key: "hash"},
				AllowAnyEmail:   true,
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "spicebox-admin-password"},
				Data:       map[string][]byte{"hash": []byte("$2a$12$abcdefghijklmnopqrstuv")},
			},
			localCluster:     false,
			wantStatus:       metav1.ConditionFalse,
			wantReason:       v1.ReasonIdPConfigInvalid,
			wantRequeueAfter: 0,
			wantMsgContains:  "only permitted on a local",
		},
		{
			name: "password kind, secret missing, LocalCluster=true → Valid=False/SecretMissing, RequeueAfter=30s",
			spec: v1.ClusterIdentityProviderSpec{
				Kind:            "password",
				ClientID:        "admin@ap.local",
				ClientSecretRef: v1.ClusterSecretKeyRef{Namespace: "default", Name: "spicebox-admin-password", Key: "hash"},
				AllowAnyEmail:   true,
			},
			secret:           nil, // deliberately absent
			localCluster:     true,
			wantStatus:       metav1.ConditionFalse,
			wantReason:       v1.ReasonIdPSecretMissing,
			wantRequeueAfter: 30 * time.Second,
		},
		{
			name: "password kind, hash key empty, LocalCluster=true → Valid=False/SecretMissing, RequeueAfter=30s",
			spec: v1.ClusterIdentityProviderSpec{
				Kind:            "password",
				ClientID:        "admin@ap.local",
				ClientSecretRef: v1.ClusterSecretKeyRef{Namespace: "default", Name: "spicebox-admin-password", Key: "hash"},
				AllowAnyEmail:   true,
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "spicebox-admin-password"},
				Data:       map[string][]byte{"hash": []byte("")},
			},
			localCluster:     true,
			wantStatus:       metav1.ConditionFalse,
			wantReason:       v1.ReasonIdPSecretMissing,
			wantRequeueAfter: 30 * time.Second,
		},
		{
			name: "password kind, allowAnyEmail=false, LocalCluster=true → Valid=False/ConfigInvalid",
			spec: v1.ClusterIdentityProviderSpec{
				Kind:            "password",
				ClientID:        "admin@ap.local",
				ClientSecretRef: v1.ClusterSecretKeyRef{Namespace: "default", Name: "spicebox-admin-password", Key: "hash"},
				AllowAnyEmail:   false,
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "spicebox-admin-password"},
				Data:       map[string][]byte{"hash": []byte("$2a$12$abcdefghijklmnopqrstuv")},
			},
			localCluster:     true,
			wantStatus:       metav1.ConditionFalse,
			wantReason:       v1.ReasonIdPConfigInvalid,
			wantRequeueAfter: 0,
			wantMsgContains:  "allowAnyEmail",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &v1.ClusterIdentityProvider{
				ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
				Spec:       tc.spec,
			}
			// No HTTPClient: password-kind reconciles must never dial out. A nil
			// HTTPClient falls back to discoveryClient() only if gate 6 is
			// actually reached, which would panic-free but wrongly succeed/fail
			// against a real network call — the absence of any stub here is the
			// test's own proof that gate 6 was skipped for kind=password whenever
			// the outcome is Valid=True.
			res, got := reconcileOnce(t, cr, tc.secret, nil, tc.localCluster)

			cond := conditions.Find(got.Status.Conditions, v1.ConditionIdPValid)
			require.NotNil(t, cond, "Valid condition must be set")
			assert.Equal(t, tc.wantStatus, cond.Status, "condition Status")
			assert.Equal(t, tc.wantReason, cond.Reason, "condition Reason")
			if tc.wantMsgContains != "" {
				assert.Contains(t, cond.Message, tc.wantMsgContains, "condition Message")
			}
			assert.Equal(t, tc.wantRequeueAfter, res.RequeueAfter, "RequeueAfter")
		})
	}
}

// TestReconciler_NoStatusWriteOnUnchangedState verifies that a second Reconcile
// with identical inputs does not perform a status write. The fake client
// increments ResourceVersion on every Patch call, so an unchanged
// ResourceVersion after the second reconcile proves no write occurred.
func TestReconciler_NoStatusWriteOnUnchangedState(t *testing.T) {
	cr := &v1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: v1.ClusterIdentityProviderName},
		Spec:       baseSpec("https://issuer.example.com"),
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "idp-secret"},
		Data:       map[string][]byte{"secret": []byte("s3cr3t")},
	}

	// Happy-path HTTP client so both reconciles reach Valid=True.
	hc := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			body, _ := json.Marshal(map[string]string{"issuer": "https://issuer.example.com"})
			rec := httptest.NewRecorder()
			rec.WriteHeader(http.StatusOK)
			rec.Write(body) //nolint:errcheck
			return rec.Result(), nil
		}),
	}

	adoptguard.WithAdoptedLabel(secret)

	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithRuntimeObjects(cr, secret).
		WithStatusSubresource(&v1.ClusterIdentityProvider{}).
		Build()

	secretReader := adoptguard.NewSecretReader(
		c, c, adoptguard.Warn,
		func(types.NamespacedName) bool { return false },
	)

	r := &Reconciler{Client: c, HTTPClient: hc, SecretReader: secretReader}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: v1.ClusterIdentityProviderName}}

	// First reconcile — should write status and set Valid=True.
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err, "first Reconcile must not error")

	var afterFirst v1.ClusterIdentityProvider
	require.NoError(t, c.Get(context.Background(), req.NamespacedName, &afterFirst))
	rvAfterFirst := afterFirst.ResourceVersion

	// Second reconcile — state is identical; guard must skip the status write.
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err, "second Reconcile must not error")

	var afterSecond v1.ClusterIdentityProvider
	require.NoError(t, c.Get(context.Background(), req.NamespacedName, &afterSecond))
	assert.Equal(t, rvAfterFirst, afterSecond.ResourceVersion,
		"ResourceVersion must not change on second reconcile with unchanged state")
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
