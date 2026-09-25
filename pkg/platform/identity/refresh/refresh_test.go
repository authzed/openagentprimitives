package refresh_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers the static/oauth/federated credkind Kinds so refresh.Run's
	// credkindregistry.Get(cred.Type) dispatch resolves in this package's tests.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// installRefreshClient routes refresh.Run's HTTP client through srv
// for the test's lifetime.
func installRefreshClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	refresh.SetHTTPClient(srv.Client())
	t.Cleanup(func() { refresh.SetHTTPClient(safehttp.Client()) })
}

// buildOAuthCred returns a Secret + matching AgentCredential pair. If
// staleExpiresAt is true, the Secret carries an already-past expires_at.
func buildOAuthCred(name, tokenEndpoint string, withRefreshToken, staleExpiresAt bool) (*corev1.Secret, spiceboxv1alpha1.AgentCredential) {
	data := map[string][]byte{
		"access_token":   []byte("old-at"),
		"token_endpoint": []byte(tokenEndpoint),
		"client_id":      []byte("cid"),
	}
	if withRefreshToken {
		data["refresh_token"] = []byte("old-rt")
	}
	if staleExpiresAt {
		data["expires_at"] = []byte(time.Now().Add(-time.Hour).Format(time.RFC3339))
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       data,
	}
	cred := spiceboxv1alpha1.AgentCredential{
		Name: "x", Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: name}},
	}
	return sec, cred
}

// TestRefreshTokenEndpointResponses sweeps the token-endpoint
// response surface: a happy 2xx must rotate the Secret atomically; a
// 401 must preserve the original; a 2xx without `expires_in` must
// clear any stale `expires_at`; a 2xx with a malformed body must wrap
// refresh.ErrDecodeResponse so callers can errors.Is-match.
func TestRefreshTokenEndpointResponses(t *testing.T) {
	cases := []struct {
		name string
		// secret seed flags.
		staleExpiresAt bool
		// server response handler.
		handler http.HandlerFunc
		// post-Run check.
		check func(t *testing.T, err error, sec *corev1.Secret)
	}{
		{
			name: "happy: 2xx rotates access_token + refresh_token + sets expires_at",
			handler: func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				assert.Equal(t, "refresh_token", r.Form.Get("grant_type"))
				assert.Equal(t, "old-rt", r.Form.Get("refresh_token"))
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":  "new-at",
					"refresh_token": "new-rt",
					"expires_in":    3600,
					"token_type":    "Bearer",
				})
			},
			check: func(t *testing.T, err error, sec *corev1.Secret) {
				require.NoError(t, err)
				assert.Equal(t, "new-at", string(sec.Data["access_token"]))
				assert.Equal(t, "new-rt", string(sec.Data["refresh_token"]))
				require.NotEmpty(t, sec.Data["expires_at"], "expires_at must be set")
				ts, perr := time.Parse(time.RFC3339, string(sec.Data["expires_at"]))
				require.NoError(t, perr, "expires_at must be RFC3339")
				assert.True(t, ts.After(time.Now()), "expires_at must be in the future: %v", ts)
			},
		},
		{
			name: "401 preserves Secret state (atomic-on-error contract)",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			},
			check: func(t *testing.T, err error, sec *corev1.Secret) {
				require.Error(t, err, "expected error on 401")
				assert.Equal(t, "old-at", string(sec.Data["access_token"]),
					"access_token must NOT mutate on failure")
			},
		},
		{
			name:           "2xx without expires_in clears any stale expires_at",
			staleExpiresAt: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":  "new-at",
					"refresh_token": "new-rt",
					"token_type":    "Bearer",
					// no expires_in → "never expires" per RFC 6749
				})
			},
			check: func(t *testing.T, err error, sec *corev1.Secret) {
				require.NoError(t, err)
				_, present := sec.Data["expires_at"]
				assert.False(t, present,
					"stale expires_at must be cleared when server omits expires_in; still present = %s",
					sec.Data["expires_at"])
			},
		},
		{
			name: "2xx with malformed body wraps ErrDecodeResponse for errors.Is matching",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("not-json"))
			},
			check: func(t *testing.T, err error, sec *corev1.Secret) {
				require.Error(t, err)
				assert.True(t, errors.Is(err, refresh.ErrDecodeResponse),
					"err must errors.Is-match refresh.ErrDecodeResponse; got %v", err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			installRefreshClient(t, srv)

			sec, cred := buildOAuthCred("creds", srv.URL, true, tc.staleExpiresAt)
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sec).Build()

			err := refresh.Run(context.Background(), c, "default", cred)

			var got corev1.Secret
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "creds"}, &got))
			tc.check(t, err, &got)
		})
	}
}

// TestRefreshSendsClientSecret verifies refresh.Run forwards a stored
// client_secret to the token endpoint — confidential OAuth clients
// (HubSpot's MCP auth apps among them) reject a refresh_token grant
// with invalid_client / BAD_CLIENT_SECRET when it is missing — while
// still omitting the field entirely for public clients that have no
// client_secret stored.
func TestRefreshSendsClientSecret(t *testing.T) {
	cases := []struct {
		name         string
		clientSecret string // "" → not stored in the Secret
		wantValue    string // expected client_secret value on the wire
		wantPresent  bool   // whether the form key must be present at all
	}{
		{name: "confidential client: stored client_secret is forwarded", clientSecret: "shhh", wantValue: "shhh", wantPresent: true},
		{name: "public client: no client_secret stored, key omitted", clientSecret: "", wantValue: "", wantPresent: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotValue string
			var gotPresent bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				gotValue = r.Form.Get("client_secret")
				_, gotPresent = r.Form["client_secret"]
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "new-at", "token_type": "Bearer", "expires_in": 3600,
				})
			}))
			t.Cleanup(srv.Close)
			installRefreshClient(t, srv)

			sec, cred := buildOAuthCred("creds", srv.URL, true, false)
			if tc.clientSecret != "" {
				sec.Data["client_secret"] = []byte(tc.clientSecret)
			}
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sec).Build()

			require.NoError(t, refresh.Run(context.Background(), c, "default", cred))
			assert.Equal(t, tc.wantPresent, gotPresent, "client_secret form key presence")
			assert.Equal(t, tc.wantValue, gotValue, "client_secret value on the wire")
		})
	}
}

// TestRefreshCollapsesConcurrentRedemptionsOfOneUpstreamCredential pins the
// scope of the single-flight guard. The thing that must not be redeemed twice
// is the *upstream credential* — the (token_endpoint, client_id,
// refresh_token) triple — not the Secret object that happens to carry it. Two
// disjoint Secrets holding one user's credential (an agent-scoped copy
// alongside the master, say) refreshed concurrently must produce exactly ONE
// token-endpoint round trip: a second presentation of the same refresh token
// is a reuse, which a rotating-RT provider answers with invalid_grant and an
// RFC 6819 reuse-detecting provider answers by revoking the whole token
// family. Both Secrets must still end up carrying the redeemed material —
// collapsing the redemption must not mean only one caller gets a write-back.
//
// Must not be t.Parallel(): installRefreshClient mutates package-level state.
func TestRefreshCollapsesConcurrentRedemptionsOfOneUpstreamCredential(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// assert (not require) throughout: this runs on the server's
		// goroutine, and require's FailNow off the test goroutine is
		// undefined behaviour.
		if !assert.NoError(t, r.ParseForm()) {
			return
		}
		assert.Equal(t, "shared-rt", r.Form.Get("refresh_token"))
		hits.Add(1)
		// Hold the redemption open so any second caller that reaches the
		// token endpoint does so unambiguously concurrently with the first.
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-at", "refresh_token": "new-rt",
			"expires_in": 3600, "token_type": "Bearer",
		})
	}))
	t.Cleanup(srv.Close)
	installRefreshClient(t, srv)

	// Two DISJOINT Secrets carrying the SAME upstream credential.
	secA, credA := buildOAuthCred("creds-a", srv.URL, true, true)
	secB, credB := buildOAuthCred("creds-b", srv.URL, true, true)
	secA.Data["refresh_token"] = []byte("shared-rt")
	secB.Data["refresh_token"] = []byte("shared-rt")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(secA, secB).Build()

	ctx := context.Background()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, cred := range []spiceboxv1alpha1.AgentCredential{credA, credB} {
		wg.Add(1)
		go func(cr spiceboxv1alpha1.AgentCredential) {
			defer wg.Done()
			errs <- refresh.Run(ctx, c, "default", cr)
		}(cred)
	}
	// Both goroutines only have to read an in-memory Secret before reaching
	// the redemption; the handler is blocked, so this window is generous.
	time.Sleep(250 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err, "both callers must succeed off the single redemption")
	}
	assert.Equal(t, int32(1), hits.Load(),
		"one upstream credential must be redeemed exactly once, however many Secrets carry it")

	for _, name := range []string{"creds-a", "creds-b"} {
		var got corev1.Secret
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &got))
		assert.Equal(t, "new-at", string(got.Data["access_token"]),
			"%s must receive the redeemed access_token", name)
		assert.Equal(t, "new-rt", string(got.Data["refresh_token"]),
			"%s must receive the redeemed refresh_token", name)
	}
}

// TestRefreshWriteBackDoesNotClobberAConcurrentPeer pins the cross-process
// half, which no in-process guard can see: the runner's in-process broker
// JIT-refreshes the same master Secrets the operator's refresh reconciler
// refreshes, and every runner pod for one user refreshes at once when a shared
// credential expires. The handler here writes the Secret out of band while our
// POST is in flight — a faithful stand-in for a peer process's write-back
// landing between our read and our write.
//
// A precondition-free MergeFrom patch can never 409, so it overwrites the
// peer's *newer* refresh token with our own already-consumed one and wedges
// the credential permanently. The write-back must carry a resourceVersion
// precondition, and on losing the race must leave the peer's material intact.
// Returning nil is correct: every refresh.Run call site re-reads the Secret
// after a nil return, so the caller still gets fresh material.
//
// Must not be t.Parallel(): installRefreshClient mutates package-level state.
func TestRefreshWriteBackDoesNotClobberAConcurrentPeer(t *testing.T) {
	ctx := context.Background()
	secKey := client.ObjectKey{Namespace: "default", Name: "creds"}

	sec, cred := buildOAuthCred("creds", "http://replaced-below.invalid/token", true, true)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sec).Build()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// assert (not require): this runs on the server's goroutine.
		var peer corev1.Secret
		if !assert.NoError(t, c.Get(ctx, secKey, &peer)) {
			return
		}
		peer.Data["access_token"] = []byte("peer-at")
		peer.Data["refresh_token"] = []byte("peer-rt")
		if !assert.NoError(t, c.Update(ctx, &peer)) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-at", "refresh_token": "new-rt",
			"expires_in": 3600, "token_type": "Bearer",
		})
	}))
	t.Cleanup(srv.Close)
	installRefreshClient(t, srv)

	// Point the Secret at the now-running endpoint (the server closes over the
	// client, so the client has to exist first).
	var live corev1.Secret
	require.NoError(t, c.Get(ctx, secKey, &live))
	live.Data["token_endpoint"] = []byte(srv.URL)
	require.NoError(t, c.Update(ctx, &live))

	err := refresh.Run(ctx, c, "default", cred)

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, secKey, &got))
	assert.NoError(t, err, "a peer's completed refresh satisfies this Run's intent")
	assert.Equal(t, "peer-rt", string(got.Data["refresh_token"]),
		"the peer's newer refresh_token must survive; overwriting it with ours wedges the credential")
	assert.Equal(t, "peer-at", string(got.Data["access_token"]),
		"the peer's access_token must survive alongside its refresh_token")
}

// TestRefreshPreflight verifies refresh.Run rejects credentials it
// cannot process — non-oauth type and oauth without a refresh_token —
// before issuing any HTTP request.
func TestRefreshPreflight(t *testing.T) {
	cases := []struct {
		name    string
		objects []client.Object
		cred    spiceboxv1alpha1.AgentCredential
		// wantErrContains, when set, is the substring that discriminates this
		// case's code path from every other rejection reason — asserting mere
		// error-existence would pass identically whether Run took the
		// registry-lookup branch or the NeedsRefresh/nil-OAuth branch.
		wantErrContains string
	}{
		{
			name:    "non-oauth credential is rejected",
			objects: nil,
			cred:    spiceboxv1alpha1.AgentCredential{Name: "x", Type: "static"},
		},
		{
			name:    "unregistered credential type is rejected, not silently treated as oauth",
			objects: nil,
			cred:    spiceboxv1alpha1.AgentCredential{Name: "x", Type: "unknown-shape"},
			// Only credkindregistry.Get's error path can produce this text; the
			// old cred.Type != "oauth" comparison and the new !k.NeedsRefresh()
			// check both also error on this input, so asserting mere
			// error-existence would pass unchanged if the registry dispatch were
			// reverted out from under it.
			wantErrContains: "unknown credential type",
		},
		{
			name: "oauth credential without refresh_token is rejected",
			objects: func() []client.Object {
				sec, _ := buildOAuthCred("creds", "http://example.invalid/token", false, false)
				return []client.Object{sec}
			}(),
			cred: spiceboxv1alpha1.AgentCredential{
				Name: "x", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "creds"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tc.objects...).Build()
			err := refresh.Run(context.Background(), c, "default", tc.cred)
			if tc.wantErrContains != "" {
				assert.ErrorContains(t, err, tc.wantErrContains)
				return
			}
			assert.Error(t, err)
		})
	}
}

// TestRefreshRedemptionMaterialLookup pins where refresh.Run reads the endpoint
// + client identity it redeems with. The split exists because Kubernetes RBAC
// has no per-key granularity: the token Secret is readable by a userPassthrough
// session's runner, so co-locating the redemption material there hands that
// runner an offline-usable refresh grant for the user's upstream account. The
// sibling is therefore authoritative, the co-located keys remain a fallback for
// the shapes that still write them (AgentIdentity setup, IdP-identity Secrets),
// and a caller that can read neither must be refused BEFORE the token endpoint
// is contacted — otherwise it consumes a rotating refresh token it cannot
// persist, wedging the credential.
//
// Must not be t.Parallel(): installRefreshClient mutates package-level state.
func TestRefreshRedemptionMaterialLookup(t *testing.T) {
	cases := []struct {
		name string
		// sibling, when non-nil, is created alongside the token Secret,
		// labelled as this credential's refresh material.
		sibling func(endpoint string) map[string][]byte
		// impostor, when non-nil, is created at the SAME derived name but
		// WITHOUT the label — another credential's Secret that happens to
		// collide.
		impostor func(endpoint string) map[string][]byte
		// coLocated is merged into the token Secret's own data.
		coLocated func(endpoint string) map[string][]byte
		// wantClientID is the client_id the endpoint must observe; empty means
		// the endpoint must never be contacted and Run must error.
		wantClientID string
		// wantClientSecret must come from the SAME Secret as wantClientID —
		// half the material from each would authenticate as nobody.
		wantClientSecret string
	}{
		{
			name:             "sibling only: redeemed with the sibling's client",
			sibling:          func(ep string) map[string][]byte { return oauthMaterial(ep, "sibling-cid", "sibling-secret") },
			wantClientID:     "sibling-cid",
			wantClientSecret: "sibling-secret",
		},
		{
			name:             "co-located only (AgentIdentity / IdP shape): still refreshable",
			coLocated:        func(ep string) map[string][]byte { return oauthMaterial(ep, "colocated-cid", "colocated-secret") },
			wantClientID:     "colocated-cid",
			wantClientSecret: "colocated-secret",
		},
		{
			name:             "both present: the sibling wins, so a stale co-located copy cannot re-authenticate",
			sibling:          func(ep string) map[string][]byte { return oauthMaterial(ep, "sibling-cid", "sibling-secret") },
			coLocated:        func(ep string) map[string][]byte { return oauthMaterial(ep, "colocated-cid", "colocated-secret") },
			wantClientID:     "sibling-cid",
			wantClientSecret: "sibling-secret",
		},
		{
			name:             "unlabelled Secret at the derived name is ignored, not redeemed with",
			impostor:         func(ep string) map[string][]byte { return oauthMaterial(ep, "impostor-cid", "impostor-secret") },
			coLocated:        func(ep string) map[string][]byte { return oauthMaterial(ep, "colocated-cid", "colocated-secret") },
			wantClientID:     "colocated-cid",
			wantClientSecret: "colocated-secret",
		},
		{
			name:         "neither: refused without contacting the endpoint",
			wantClientID: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			var gotClientID, gotClientSecret atomic.Value
			gotClientID.Store("")
			gotClientSecret.Store("")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if !assert.NoError(t, r.ParseForm()) {
					return
				}
				gotClientID.Store(r.Form.Get("client_id"))
				gotClientSecret.Store(r.Form.Get("client_secret"))
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "new-at", "refresh_token": "new-rt",
					"expires_in": 3600, "token_type": "Bearer",
				})
			}))
			t.Cleanup(srv.Close)
			installRefreshClient(t, srv)

			tokens := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
				Data:       map[string][]byte{"access_token": []byte("old-at"), "refresh_token": []byte("old-rt")},
			}
			objs := []client.Object{tokens}
			if tc.coLocated != nil {
				for k, v := range tc.coLocated(srv.URL) {
					tokens.Data[k] = v
				}
			}
			if tc.sibling != nil {
				objs = append(objs, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: refresh.MaterialSecretName("creds"), Namespace: "default",
						Labels: map[string]string{refresh.MaterialSecretLabel: "true"},
					},
					Data: tc.sibling(srv.URL),
				})
			}
			if tc.impostor != nil {
				objs = append(objs, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: refresh.MaterialSecretName("creds"), Namespace: "default",
					},
					Data: tc.impostor(srv.URL),
				})
			}
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
			cred := spiceboxv1alpha1.AgentCredential{
				Name: "x", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "creds"}},
			}

			err := refresh.Run(context.Background(), c, "default", cred)

			if tc.wantClientID == "" {
				require.Error(t, err, "no readable redemption material must be refused")
				assert.Contains(t, err.Error(), refresh.MaterialSecretName("creds"),
					"the error must name the Secret the material was looked for in")
				assert.Zero(t, hits.Load(), "the token endpoint must not be contacted: "+
					"a caller that cannot persist a rotated token must never consume one")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantClientID, gotClientID.Load(), "redeemed as the wrong client")
			assert.Equal(t, tc.wantClientSecret, gotClientSecret.Load(),
				"client_secret must come from the same Secret as the client_id")
		})
	}
}

// TestRefreshRefusesWhenTheRefreshMaterialSiblingIsUnreadable pins the case the
// lookup table above cannot express: the sibling Get does not fail with
// NotFound, it fails with FORBIDDEN.
//
// That is what a userPassthrough session's runner sees. Its Role pins
// resourceNames to the master Secrets only (pkg/controllers/agentsession/
// passthrough.go), so a Get of the derived refresh-material name is refused by
// RBAC — and the very same Role grants `get` and nothing else, so the runner
// cannot write the master either. Degrading to the master's own co-located keys
// there is the worst possible outcome: the material resolves, the fail-closed
// refusal never fires, the provider RETIRES the stored refresh token on
// redemption, and the write-back is Forbidden — so the rotated token is lost and
// the credential is wedged beyond the reconciler's reach.
//
// A caller denied the redemption material is denied the write-back too. It must
// be refused BEFORE the token endpoint is contacted.
//
// Must not be t.Parallel(): installRefreshClient mutates package-level state.
func TestRefreshRefusesWhenTheRefreshMaterialSiblingIsUnreadable(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-at", "refresh_token": "new-rt",
			"expires_in": 3600, "token_type": "Bearer",
		})
	}))
	t.Cleanup(srv.Close)
	installRefreshClient(t, srv)

	// A pre-split master: tokens AND redemption material co-located, exactly
	// what every credential linked before the Secret split still looks like.
	sec, cred := buildOAuthCred("creds", srv.URL, true, true)

	// The runner's grant, modelled: `get` on the master by name, nothing else.
	forbidden := func(verb, name string) error {
		return apierrors.NewForbidden(
			schema.GroupResource{Resource: "secrets"}, name,
			fmt.Errorf("cannot %s resource \"secrets\" in API group \"\"", verb))
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sec).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if key.Name != "creds" {
					return forbidden("get", key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object,
				patch client.Patch, opts ...client.PatchOption) error {
				return forbidden("patch", obj.GetName())
			},
		}).Build()

	err := refresh.Run(ctx, c, "default", cred)

	assert.Zero(t, hits.Load(),
		"the token endpoint must not be contacted: the provider retires the refresh token "+
			"on redemption and this caller cannot persist the replacement — the credential "+
			"would be permanently wedged")
	require.Error(t, err, "a caller that may not read the redemption material must be refused")
	assert.Contains(t, err.Error(), refresh.MaterialSecretName("creds"),
		"the error must name the Secret whose unreadability caused the refusal")

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "creds"}, &got))
	assert.Equal(t, "old-rt", string(got.Data["refresh_token"]),
		"the stored refresh token must still be the one the provider honours")
}

// oauthMaterial is the redemption-material key set — the three keys that turn a
// refresh_token into an independently usable credential.
func oauthMaterial(endpoint, clientID, clientSecret string) map[string][]byte {
	return map[string][]byte{
		"token_endpoint": []byte(endpoint),
		"client_id":      []byte(clientID),
		"client_secret":  []byte(clientSecret),
	}
}
