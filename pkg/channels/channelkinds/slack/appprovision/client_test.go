package appprovision

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testManifestYAML = "display_information:\n  name: demo-agent-bot\n"

// newTestClient points an HTTPClient at a stand-in Slack.
func newTestClient(t *testing.T, h http.HandlerFunc) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewHTTPClient(WithBaseURL(srv.URL))
}

func TestManifestJSON_ConvertsYAMLToJSON(t *testing.T) {
	got, err := ManifestJSON(testManifestYAML)
	require.NoError(t, err, "ManifestJSON")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(got), &decoded), "result must be valid JSON")
	info, ok := decoded["display_information"].(map[string]any)
	require.True(t, ok, "display_information must survive the conversion")
	assert.Equal(t, "demo-agent-bot", info["name"])
}

func TestCreate_SendsTheManifestAndBearerToken(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]json.RawMessage
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"ok":true,"app_id":"A0DEMO","credentials":{"client_id":"cid","signing_secret":"sig"},"oauth_authorize_url":"https://example.test/auth"}`))
	})

	manifestJSON, err := ManifestJSON(testManifestYAML)
	require.NoError(t, err, "ManifestJSON")

	res, err := c.Create(context.Background(), "xoxe.xoxp-demo", manifestJSON)
	require.NoError(t, err, "Create")

	assert.Equal(t, "Bearer xoxe.xoxp-demo", gotAuth, "config token goes in the Authorization header")
	assert.Equal(t, "/api/apps.manifest.create", gotPath)
	assert.Contains(t, string(gotBody["manifest"]), "demo-agent-bot", "the manifest is sent as a JSON object")
	assert.Equal(t, "A0DEMO", res.AppID)
	assert.Equal(t, "sig", res.SigningSecret)
	assert.Equal(t, "https://example.test/auth", res.OAuthAuthorizeURL)
}

func TestInstall_ReturnsBothTokens(t *testing.T) {
	var gotBody struct {
		AppID     string   `json:"app_id"`
		BotScopes []string `json:"bot_scopes"`
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"ok":true,"app_id":"A0DEMO","api_access_tokens":{"bot":"xoxb-demo","app_level":"xapp-demo"}}`))
	})

	res, err := c.Install(context.Background(), "xoxe.xoxp-demo", "A0DEMO", []string{"chat:write", "commands"})
	require.NoError(t, err, "Install")

	assert.Equal(t, "A0DEMO", gotBody.AppID)
	assert.Equal(t, []string{"chat:write", "commands"}, gotBody.BotScopes, "scopes are sent verbatim")
	assert.Equal(t, "xoxb-demo", res.BotToken)
	assert.Equal(t, "xapp-demo", res.AppLevelToken)
}

// TestAPIErrors covers what Slack refuses with, including the approval states
// that decide whether the wizard falls back rather than failing.
func TestAPIErrors(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		status       int
		wantCode     string
		wantApproval bool
	}{
		{name: "invalid_manifest: reported with its code, not approval", body: `{"ok":false,"error":"invalid_manifest"}`, status: 200, wantCode: "invalid_manifest"},
		{name: "app_approval_request_eligible: reported as approval-required", body: `{"ok":false,"error":"app_approval_request_eligible"}`, status: 200, wantCode: "app_approval_request_eligible", wantApproval: true},
		{name: "app_approval_request_pending: reported as approval-required", body: `{"ok":false,"error":"app_approval_request_pending"}`, status: 200, wantCode: "app_approval_request_pending", wantApproval: true},
		{name: "app_approval_request_denied: reported as approval-required", body: `{"ok":false,"error":"app_approval_request_denied"}`, status: 200, wantCode: "app_approval_request_denied", wantApproval: true},
		{name: "ratelimited: reported with its code", body: `{"ok":false,"error":"ratelimited"}`, status: 200, wantCode: "ratelimited"},
		{name: "malformed body: reported, never read as success", body: `not json`, status: 200, wantCode: ""},
		{name: "HTTP 500: reported, never read as success", body: `{"ok":true}`, status: 500, wantCode: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			_, err := c.Install(context.Background(), "tok", "A0DEMO", []string{"chat:write"})
			require.Error(t, err, "a refusal must never be read as success")

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				assert.Empty(t, tc.wantCode, "a coded refusal must surface as *APIError")
				return
			}
			assert.Equal(t, tc.wantCode, apiErr.Code)
			assert.Equal(t, tc.wantApproval, apiErr.IsApprovalRequired())
		})
	}
}
