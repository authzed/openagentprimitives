package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderAppMountsAppWithProps(t *testing.T) {
	mf, err := loadManifest()
	require.NoError(t, err)
	s := &Server{manifest: mf, sandboxHost: func() string { return "sandbox.example" }}
	rec := httptest.NewRecorder()
	require.NoError(t, s.RenderApp(rec, "system", map[string]any{"k": "v"}, PageMeta{Title: "Hi"}, false))
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="system"`)
	assert.Contains(t, body, `{"k":"v"}`)
	assert.Contains(t, body, "<title>Hi</title>")
}

func TestRendererFromContextRoundTrips(t *testing.T) {
	s := &Server{}
	ctx := withRenderer(context.Background(), s)
	got := RendererFromContext(ctx)
	assert.Equal(t, AppRenderer(s), got)
}

func TestRendererFromContextNilWhenAbsent(t *testing.T) {
	assert.Nil(t, RendererFromContext(context.Background()))
}

// A raw Handler can render a React page via the context-injected renderer.
func TestHandlerRendersViaContextRenderer(t *testing.T) {
	mf, err := loadManifest()
	require.NoError(t, err)
	rraw := func(w http.ResponseWriter, r *http.Request) {
		_ = RendererFromContext(r.Context()).RenderApp(w, "system", map[string]any{"ok": true}, PageMeta{Title: "T"}, false)
	}
	ui := pageUI{routes: []Route{{Origin: OriginTrusted, Pattern: "/raw", Auth: AuthNone, Handler: http.HandlerFunc(rraw)}}}
	s, err := NewServer(func(*http.Request) (string, bool) { return "", false }, nil,
		func() string { return "t.example" }, func() string { return "s.example" }, nil, nil, []WebUI{ui})
	require.NoError(t, err)
	s.manifest = mf
	req := httptest.NewRequest(http.MethodGet, "http://t.example/raw", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `data-app="system"`)
	assert.Contains(t, rec.Body.String(), `{"ok":true}`)
}
