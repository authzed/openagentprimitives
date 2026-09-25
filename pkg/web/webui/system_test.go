package webui

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSystemPageSetsStatusAndMountsSystemApp(t *testing.T) {
	mf, err := loadManifest()
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	renderSystemPage(rec, mf, "NONCE", devConfig{}, &PageError{
		Status: 404, Kind: "notFound", Title: "Not found", Message: "No such page.",
	})
	assert.Equal(t, 404, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="system"`)
	assert.Contains(t, body, `"kind":"notFound"`)
	assert.Contains(t, body, "Not found") // server-rendered styled fallback text
}
