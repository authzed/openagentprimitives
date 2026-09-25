package appprovision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

func TestBuildManifest_RequestsOnlyTheFourPermissionsReviewbotNeeds(t *testing.T) {
	raw, err := BuildManifest(Params{
		Name: "demo-reviewbot", ExternalBaseURL: "https://ap.demo.test",
		Namespace: "default", ChannelName: "demo-reviewbot-gh", RedirectURL: "http://127.0.0.1:8931/callback",
	})
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &m))

	perms, ok := m["default_permissions"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{
		"contents": "read", "pull_requests": "read", "metadata": "read", "checks": "write",
	}, perms, "reviewbot must be structurally unable to modify a repo or comment on a PR")

	hook, ok := m["hook_attributes"].(map[string]any)
	require.True(t, ok, "the webhook is registered declaratively by the manifest, with no extra API call")
	// Built with the shared helper, never a literal — see the mux-match test below.
	assert.Equal(t, "https://ap.demo.test"+channelevents.WebhookPathFor("github", "default", "demo-reviewbot-gh"),
		hook["url"])
	assert.Equal(t, true, hook["active"])
	assert.Equal(t, []any{"pull_request"}, m["default_events"])
	assert.Equal(t, false, m["public"])
}

// TestWebhookPathFor_ActuallyRoutesToTheRegisteredHandler is the reason the
// helper exists. Asserting that two constants are equal would pass with both
// sides drifted in step — the exact failure this guards. Register the real
// route on a real mux and prove a helper-built URL reaches it, so the test
// fails the moment either side moves alone.
func TestWebhookPathFor_ActuallyRoutesToTheRegisteredHandler(t *testing.T) {
	var hit bool
	mux := http.NewServeMux()
	mux.HandleFunc(channelevents.WebhookRoutePattern, func(w http.ResponseWriter, r *http.Request) {
		hit = true
		assert.Equal(t, "github", r.PathValue("kind"))
		assert.Equal(t, "default", r.PathValue("ns"))
		assert.Equal(t, "demo-reviewbot-gh", r.PathValue("channel"))
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		channelevents.WebhookPathFor("github", "default", "demo-reviewbot-gh"), nil))

	assert.True(t, hit, "a helper-built path must reach the registered pattern")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestConvert_ReturnsTheAppIdentityAndSecrets(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":12345,"slug":"demo-reviewbot","pem":"-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----\n","webhook_secret":"whsec_demo"}`))
	}))
	t.Cleanup(srv.Close)

	got, err := NewHTTPClient(WithBaseURL(srv.URL)).Convert(context.Background(), "tempcode")
	require.NoError(t, err)
	assert.Equal(t, "/app-manifests/tempcode/conversions", gotPath)
	assert.Equal(t, "12345", got.AppID)
	assert.Equal(t, "demo-reviewbot", got.Slug)
	// PEM and WebhookSecret are sensitive.SensitiveValue — reach the bytes via
	// UnderlyingValue(), the one sanctioned exit, rather than comparing the
	// wrapper directly.
	assert.Equal(t, "whsec_demo", string(got.WebhookSecret.UnderlyingValue()))
	assert.Contains(t, string(got.PEM.UnderlyingValue()), "BEGIN RSA PRIVATE KEY")
}

// TestConversion_FormattingNeverLeaksSecrets is the reason PEM and
// WebhookSecret are sensitive.SensitiveValue rather than plain strings: the
// PEM mints installation tokens indefinitely, so an accidental %v, a
// structured-log line built from this struct, or a json.Marshal of it must
// redact rather than dump the private key in cleartext. Assert against every
// rendering path a caller could reach for by accident.
func TestConversion_FormattingNeverLeaksSecrets(t *testing.T) {
	const pemBody = "-----BEGIN RSA PRIVATE KEY-----\nMIIsupersecretkeymaterial\n-----END RSA PRIVATE KEY-----\n"
	const webhookSecret = "whsec_super_secret_do_not_leak"

	c := Conversion{
		AppID:         "12345",
		Slug:          "demo-reviewbot",
		PEM:           sensitive.NewSensitiveValue([]byte(pemBody)),
		WebhookSecret: sensitive.NewSensitiveValue([]byte(webhookSecret)),
	}

	jsonBytes, err := json.Marshal(c)
	require.NoError(t, err)

	renderings := map[string]string{
		"%v":           fmt.Sprintf("%v", c),
		"%+v":          fmt.Sprintf("%+v", c),
		"%#v":          fmt.Sprintf("%#v", c),
		"json.Marshal": string(jsonBytes),
	}
	for verb, out := range renderings {
		assert.NotContains(t, out, pemBody, "%s must not contain the PEM body", verb)
		assert.NotContains(t, out, "supersecretkeymaterial", "%s must not contain PEM key material", verb)
		assert.NotContains(t, out, webhookSecret, "%s must not contain the webhook secret", verb)
		assert.Contains(t, out, "REDACTED", "%s must show the redaction happened, not just an absence of the secret", verb)
	}
}

func TestConvert_ErrorDoesNotEchoSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"code expired"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewHTTPClient(WithBaseURL(srv.URL)).Convert(context.Background(), "tempcode")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "tempcode", "never echo the exchange code into an error")
}
