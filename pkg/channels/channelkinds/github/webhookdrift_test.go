package github

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func testAppPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate test RSA key")
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func demoChannel() *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-reviewbot-gh"},
	}
}

func TestCheckWebhookURLDrift_ReportsMatchWhenURLsAgree(t *testing.T) {
	const url = "https://ap.demo.test/webhooks/github/default/demo-reviewbot-gh"
	var sawPatch bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			sawPatch = true
		}
		_, _ = w.Write([]byte(`{"hook_attributes":{"url":"` + url + `"}}`))
	}))
	t.Cleanup(srv.Close)

	secrets := channelkinds.WebhookSecrets{Data: map[string][]byte{
		"app-id": []byte("12345"), "private-key": testAppPEM(t),
	}}
	registered, drifted, err := Kind{}.CheckWebhookURLDrift(context.Background(), demoChannel(), secrets, url, srv.URL)
	require.NoError(t, err)
	assert.Equal(t, url, registered)
	assert.False(t, drifted, "identical URLs must not be reported as drift")
	assert.False(t, sawPatch, "a drift CHECK must never write back to the provider")
}

func TestCheckWebhookURLDrift_ReportsDriftWhenURLsDiffer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hook_attributes":{"url":"https://old.demo.test/webhooks/github/default/demo-reviewbot-gh"}}`))
	}))
	t.Cleanup(srv.Close)

	secrets := channelkinds.WebhookSecrets{Data: map[string][]byte{
		"app-id": []byte("12345"), "private-key": testAppPEM(t),
	}}
	registered, drifted, err := Kind{}.CheckWebhookURLDrift(context.Background(), demoChannel(), secrets,
		"https://new.demo.test/webhooks/github/default/demo-reviewbot-gh", srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "https://old.demo.test/webhooks/github/default/demo-reviewbot-gh", registered)
	assert.True(t, drifted)
}

// TestCheckWebhookURLDrift_MissingCredsErrorsBeforeAnyNetworkCall is the
// negative control for the "unreachable App is NOT the same as no drift"
// rule: with no app-id/private-key to authenticate with, this must return an
// error (drifted=false is not a valid stand-in), and it must fail before
// touching the network at all.
func TestCheckWebhookURLDrift_MissingCredsErrorsBeforeAnyNetworkCall(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	_, drifted, err := Kind{}.CheckWebhookURLDrift(context.Background(), demoChannel(),
		channelkinds.WebhookSecrets{Data: map[string][]byte{}}, "https://ap.demo.test/webhooks/github/default/demo-reviewbot-gh", srv.URL)
	require.Error(t, err)
	assert.False(t, drifted, "an error result must not also claim drift")
	assert.False(t, called, "must fail before any network call with no credentials")
}
