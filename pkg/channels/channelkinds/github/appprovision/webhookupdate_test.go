package appprovision

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateWebhookURL_PATCHesTheAppsHookConfig(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotAuth, gotBody = r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"url":"https://ap.demo.test/webhooks/x"}`))
	}))
	t.Cleanup(srv.Close)

	err := NewHTTPClient(WithBaseURL(srv.URL)).UpdateWebhookURL(context.Background(), UpdateWebhookURLParams{
		AppID: "12345", PrivateKeyPEM: testAppPEM(t), URL: "https://ap.demo.test/webhooks/x",
	})
	require.NoError(t, err)

	assert.Equal(t, http.MethodPatch, gotMethod)
	assert.Equal(t, "/app/hook/config", gotPath)
	assert.Contains(t, gotAuth, "Bearer ", "must authenticate as the App itself")
	assert.Contains(t, gotBody, "https://ap.demo.test/webhooks/x")
}

func TestUpdateWebhookURL_EmptyURLIsRefusedBeforeAnyNetworkCall(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	err := NewHTTPClient(WithBaseURL(srv.URL)).UpdateWebhookURL(context.Background(), UpdateWebhookURLParams{
		AppID: "12345", PrivateKeyPEM: testAppPEM(t), URL: "",
	})
	require.Error(t, err)
	assert.False(t, called, "an empty url would clear the App's webhook; it must never be sent")
}

func TestUpdateWebhookURL_ErrorCarriesNoCredential(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(srv.Close)

	keyPEM := testAppPEM(t)
	err := NewHTTPClient(WithBaseURL(srv.URL)).UpdateWebhookURL(context.Background(), UpdateWebhookURLParams{
		AppID: "12345", PrivateKeyPEM: keyPEM, URL: "https://ap.demo.test/x",
	})
	require.Error(t, err)

	// Without this the assertions below are inert: if the request carried no
	// auth at all, there is no token to find in the error and both pass while
	// proving nothing.
	require.NotEmpty(t, gotAuth, "the request must have carried the App JWT")
	appJWT := strings.TrimPrefix(gotAuth, "Bearer ")
	require.NotEmpty(t, appJWT)

	assert.NotContains(t, err.Error(), appJWT, "the App JWT must never reach an error string — this one is logged")
	assert.NotContains(t, err.Error(), string(keyPEM), "nor may the private key")
}
