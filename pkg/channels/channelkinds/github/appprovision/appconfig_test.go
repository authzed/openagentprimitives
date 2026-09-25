package appprovision

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAppPEM generates a fresh RSA key and PEM-encodes it, once per test
// process. ReadAppConfig signs a real JWT with it (via githubapp.SignAppJWT),
// so the key must parse as valid RSA — a placeholder string would fail
// before the HTTP call this test is actually about.
func testAppPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate test RSA key")
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestReadAppConfig_ReturnsTheRegisteredWebhookURL(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"hook_attributes":{"url":"https://ap.demo.test/webhooks/github/default/demo-reviewbot-gh","active":true}}`))
	}))
	t.Cleanup(srv.Close)

	got, err := NewHTTPClient(WithBaseURL(srv.URL)).ReadAppConfig(context.Background(), ReadAppConfigParams{
		AppID: "12345", PrivateKeyPEM: testAppPEM(t),
	})
	require.NoError(t, err)
	assert.Equal(t, "https://ap.demo.test/webhooks/github/default/demo-reviewbot-gh", got.WebhookURL)

	// This is a READ: it must never be anything but a GET, and it must
	// authenticate as the App (a Bearer JWT), not omit auth entirely.
	assert.Equal(t, http.MethodGet, gotMethod, "reading App config must never be anything but a GET")
	assert.Equal(t, "/app", gotPath)
	assert.Contains(t, gotAuth, "Bearer ", "must authenticate as the App itself")
}

func TestReadAppConfig_MalformedPEMIsRejectedBeforeAnyNetworkCall(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewHTTPClient(WithBaseURL(srv.URL)).ReadAppConfig(context.Background(), ReadAppConfigParams{
		AppID: "12345", PrivateKeyPEM: []byte("not a pem"),
	})
	require.Error(t, err)
	assert.False(t, called, "an invalid key must fail before any network call")
}

func TestReadAppConfig_ErrorSurfacesStatusNotBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewHTTPClient(WithBaseURL(srv.URL)).ReadAppConfig(context.Background(), ReadAppConfigParams{
		AppID: "12345", PrivateKeyPEM: testAppPEM(t),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}
