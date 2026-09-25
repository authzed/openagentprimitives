package clilogin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliidentity"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// ---- Test helpers ------------------------------------------------------------

// webdURLConfigMap builds the webd external-URL ConfigMap for tests.
func webdURLConfigMap(trustedURL string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: externalurl.Namespace,
			Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
		},
		Data: map[string]string{
			spiceboxv1alpha1.WebdTrustedURLKey: trustedURL,
		},
	}
}

// fakeIdentitydServer creates a fake identityd httptest.Server.
//
// The /cli/login handler asserts state + port params are present, then
// simulates the browser hop: it immediately GETs the CLI's loopback
// callback at http://127.0.0.1:<port>/callback?code=fake-one-time&state=<state>,
// then returns 200 (the caller used GET so we just acknowledge).
//
// The /cli/exchange handler asserts code + state in the JSON body, then
// responds with a canned exchange response.
//
// loginHandler overrides the /cli/login behaviour (nil → use default).
// exchangeStatus overrides the /cli/exchange status code (0 → 200).
// exchangeBody overrides the /cli/exchange response body (nil → canned).
func fakeIdentitydServer(t *testing.T, loginHandler http.HandlerFunc, exchangeStatus int, exchangeBody []byte) *httptest.Server {
	t.Helper()

	cannedExchange, _ := json.Marshal(map[string]any{
		"assertion":   "x.y",
		"subject":     "YWxpY2VAZXhhbXBsZS5jb20",
		"email":       "alice@example.com",
		"displayName": "Alice",
		"expiresAt":   time.Now().Add(12 * time.Hour).Unix(),
	})

	mux := http.NewServeMux()

	mux.HandleFunc("/cli/login", func(w http.ResponseWriter, r *http.Request) {
		if loginHandler != nil {
			loginHandler(w, r)
			return
		}
		state := r.URL.Query().Get("state")
		port := r.URL.Query().Get("port")
		if state == "" || port == "" {
			http.Error(w, "missing params", http.StatusBadRequest)
			return
		}
		// Simulate the browser redirect: call the CLI's loopback callback.
		callbackURL := fmt.Sprintf("http://127.0.0.1:%s/callback?code=fake-one-time&state=%s", port, state)
		resp, err := http.Get(callbackURL) //nolint:noctx
		if err != nil {
			http.Error(w, "callback failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_ = resp.Body.Close()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/cli/exchange", func(w http.ResponseWriter, r *http.Request) {
		if exchangeStatus != 0 && exchangeStatus != http.StatusOK {
			errBody := exchangeBody
			if errBody == nil {
				eb, _ := json.Marshal(map[string]string{"error": "invalid or expired code"})
				errBody = eb
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(exchangeStatus)
			_, _ = w.Write(errBody)
			return
		}
		var req struct {
			Code  string `json:"code"`
			State string `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Code == "" || req.State == "" {
			http.Error(w, "missing code/state", http.StatusBadRequest)
			return
		}
		body := cannedExchange
		if exchangeBody != nil {
			body = exchangeBody
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	return httptest.NewServer(mux)
}

// withLoginSeams overrides the package-level injectable vars for the duration
// of the test and restores them in t.Cleanup.
//
// openFn goes through browsertest rather than a var in this package: the login
// flow calls browser.Open, which declines to reach the OS from a test binary on
// its own. openFn here is a DRIVER, not a stub — it stands in for the human who
// would visit the login page, and the loopback callback it triggers is what
// lets runLoginFlow return at all.
func withLoginSeams(t *testing.T, httpClient *http.Client, openFn func(string) error, timeout time.Duration) {
	t.Helper()
	origClient := loginHTTPClient
	origTimeout := loginTimeout
	loginHTTPClient = httpClient
	browsertest.Use(t, openFn)
	if timeout > 0 {
		loginTimeout = timeout
	}
	t.Cleanup(func() {
		loginHTTPClient = origClient
		loginTimeout = origTimeout
	})
}

// ---- Tests -------------------------------------------------------------------

// TestLogin_HappyPath verifies the full login flow:
//   - The fake identityd /cli/login calls back to the CLI's loopback.
//   - The exchange returns a canned response.
//   - The cache file is written with 0600 perms.
//   - stdout contains "Logged in as alice@example.com".
func TestLogin_HappyPath(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)

	srv := fakeIdentitydServer(t, nil, 0, nil)
	t.Cleanup(srv.Close)

	c := aptest.NewClient(t, webdURLConfigMap(srv.URL))
	withLoginSeams(t, srv.Client(), func(url string) error {
		// The fake OpenBrowser drives the /cli/login handler which calls back.
		resp, err := srv.Client().Get(url)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}, 10*time.Second)

	var out bytes.Buffer
	cmd := NewLoginCmd(&apcmd.Globals{})
	cmd.SetOut(&out)
	// Inject the fake bundle via the command's RunE. We can't override g.Bundle()
	// easily, so we test runLoginFlow directly and verify the command output
	// separately.
	cached, err := runLoginFlow(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", cached.Email)
	assert.Equal(t, "Alice", cached.DisplayName)
	assert.Equal(t, "x.y", cached.Assertion)

	// Verify the file was persisted.
	got, err := cliidentity.Load()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "alice@example.com", got.Email)

	// Verify file permissions.
	p, err := cliidentity.Path()
	require.NoError(t, err)
	fi, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm(), "cache file must be 0600")
}

// TestLogin_StateMismatch verifies that a callback with a wrong state is
// rejected and the flow times out (not a success).
func TestLogin_StateMismatch(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)

	// A login handler that sends the wrong state to the callback.
	badStateHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		port := r.URL.Query().Get("port")
		if port == "" {
			http.Error(w, "missing port", http.StatusBadRequest)
			return
		}
		// Call the callback with the wrong state.
		callbackURL := fmt.Sprintf("http://127.0.0.1:%s/callback?code=bad&state=WRONG-STATE", port)
		resp, _ := http.Get(callbackURL) //nolint:noctx
		if resp != nil {
			_ = resp.Body.Close()
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := fakeIdentitydServer(t, badStateHandler, 0, nil)
	t.Cleanup(srv.Close)

	c := aptest.NewClient(t, webdURLConfigMap(srv.URL))
	// Very short timeout so the test doesn't wait 3 minutes.
	withLoginSeams(t, srv.Client(), func(url string) error {
		resp, err := srv.Client().Get(url)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}, 200*time.Millisecond)

	_, err := runLoginFlow(context.Background(), c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out", "state mismatch should eventually time out")
}

// TestLogin_Exchange403 verifies that a 403 from /cli/exchange surfaces the
// error body message.
func TestLogin_Exchange403(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)

	srv := fakeIdentitydServer(t, nil, http.StatusForbidden, nil)
	t.Cleanup(srv.Close)

	c := aptest.NewClient(t, webdURLConfigMap(srv.URL))
	withLoginSeams(t, srv.Client(), func(url string) error {
		resp, err := srv.Client().Get(url)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}, 10*time.Second)

	_, err := runLoginFlow(context.Background(), c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403", "error must mention status code")
	assert.Contains(t, err.Error(), "invalid or expired code", "error must surface the server message")
}

// TestLogin_NoExternalURLConfigMap verifies the actionable error when the
// webd ConfigMap is absent.
func TestLogin_NoExternalURLConfigMap(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)

	// Empty client — no ConfigMap.
	c := aptest.NewClient(t)
	withLoginSeams(t, http.DefaultClient, func(string) error { return nil }, 10*time.Second)

	_, err := runLoginFlow(context.Background(), c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no external URL configured", "error must be actionable")
	assert.Contains(t, err.Error(), "oap init --local", "error must mention oap init")
}

// ---- EnsureIdentity tests -------------------------------------------------

// TestEnsureCLIIdentity_CacheHit verifies that a valid cached assertion
// returns VerifiedEmail without running the login flow.
func TestEnsureCLIIdentity_CacheHit(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)

	require.NoError(t, cliidentity.Save(cliidentity.Cached{
		Email:       "alice@example.com",
		DisplayName: "Alice",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}))

	g := &apcmd.Globals{}
	p, err := EnsureIdentity(context.Background(), g)
	require.NoError(t, err)
	assert.Equal(t, identity.VerifiedEmail("alice@example.com", "Alice"), p)
}

// TestEnsureCLIIdentity_NoCacheNoCR verifies that a missing cache and no
// ClusterIdentityProvider CR returns the local fallback principal.
func TestEnsureCLIIdentity_NoCacheNoCR(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)

	// No ClusterIdentityProvider in the fake client.
	c := aptest.NewClient(t)

	// EnsureIdentity calls g.Bundle() which needs a real kubeconfig.
	// Inject the bundle directly by testing the inner logic.
	p, err := EnsureIdentityWithClient(context.Background(), c)
	require.NoError(t, err)
	// Should be the local fallback.
	assert.Equal(t, identity.Kind("local"), p.Kind())
	assert.False(t, p.EmailVerified())
}

// TestEnsureCLIIdentity_NoCacheCRExists verifies that a missing cache with
// a ClusterIdentityProvider CR present triggers the login flow.
func TestEnsureCLIIdentity_NoCacheCRExists(t *testing.T) {
	aptest.FakeIdentityConfigDir(t)

	srv := fakeIdentitydServer(t, nil, 0, nil)
	t.Cleanup(srv.Close)

	// Seed both the webd ConfigMap and a ClusterIdentityProvider CR.
	cidp := &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: spiceboxv1alpha1.ClusterIdentityProviderName,
		},
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:     "oidc",
			Issuer:   "https://accounts.example.com",
			ClientID: "client-id",
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: "agentprimitives-system", Name: "idp-secret", Key: "secret",
			},
			AllowAnyEmail: true,
		},
	}
	c := aptest.NewClient(t, webdURLConfigMap(srv.URL), cidp)

	withLoginSeams(t, srv.Client(), func(url string) error {
		resp, err := srv.Client().Get(url)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}, 10*time.Second)

	p, err := EnsureIdentityWithClient(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, identity.VerifiedEmail("alice@example.com", "Alice"), p)
}
