package webui

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderStandaloneApp_KnownEntry(t *testing.T) {
	// "admin" is a committed webassets entry; the settings entry lands in a
	// later task, so pin the contract on an entry that exists today.
	rec := httptest.NewRecorder()
	err := RenderStandaloneApp(rec, "admin", "Test Title", map[string]string{"apiBase": "/api"})
	require.NoError(t, err)
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="admin"`)
	assert.Contains(t, body, "<title>Test Title</title>")
	assert.Contains(t, body, `type="module"`, "must reference the manifest entry script")
	csp := rec.Header().Get("Content-Security-Policy")
	require.NotEmpty(t, csp)
	assert.Contains(t, csp, "script-src 'nonce-", "strict nonce CSP required")
	assert.NotContains(t, csp, "unsafe-eval", "no dev-mode widening in standalone rendering")
}

func TestRenderStandaloneApp_SettingsEntry(t *testing.T) {
	// The settings app entry lands in the webassets dist as of this task;
	// pin the contract now that it is present in the embedded manifest.
	rec := httptest.NewRecorder()
	err := RenderStandaloneApp(rec, "settings", "Settings", map[string]string{"apiBase": "/api"})
	require.NoError(t, err)
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="settings"`)
	assert.Contains(t, body, "<title>Settings</title>")
	assert.Contains(t, body, `type="module"`, "must reference the manifest entry script")
}

func TestRenderStandaloneApp_UnknownEntry_Errors(t *testing.T) {
	rec := httptest.NewRecorder()
	err := RenderStandaloneApp(rec, "no-such-app", "x", nil)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no-such-app"))
}
