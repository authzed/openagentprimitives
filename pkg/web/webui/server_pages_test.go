package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type sysUI struct{}

func (sysUI) Name() string { return "sysui" }
func (sysUI) Routes(Deps) []Route {
	build := func(context.Context, *http.Request) (any, PageMeta, error) {
		return map[string]any{"ok": true}, PageMeta{Title: "OK"}, nil
	}
	return []Route{
		{Origin: OriginTrusted, Pattern: "/ok", Auth: AuthNone, Page: &Page{App: "system", Build: build}},
		{Origin: OriginTrusted, Pattern: "/needauth", Auth: AuthAuthenticated, Page: &Page{App: "system", Build: build}},
		{Origin: OriginTrusted, Pattern: "/boom", Auth: AuthNone, Page: &Page{App: "system",
			Build: func(context.Context, *http.Request) (any, PageMeta, error) {
				return nil, PageMeta{}, &PageError{Status: 410, Kind: "expired", Title: "Gone", Message: "expired"}
			}}},
	}
}

func TestPageBuildUnmarshalablePropsRender500NotEmpty200(t *testing.T) {
	noAuth := func(*http.Request) (string, bool) { return "", false }
	ui := pageUI{routes: []Route{{
		Origin: OriginTrusted, Pattern: "/bad", Auth: AuthNone,
		Page: &Page{App: "system", Build: func(context.Context, *http.Request) (any, PageMeta, error) {
			return map[string]any{"ch": make(chan int)}, PageMeta{}, nil // not JSON-serializable
		}},
	}}}
	s, err := NewServer(noAuth, nil,
		func() string { return "t.example" }, func() string { return "s.example" },
		nil, nil, []WebUI{ui})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "http://t.example/bad", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), `data-app="system"`)
}

func TestPageRoutes(t *testing.T) {
	noAuth := func(*http.Request) (string, bool) { return "", false }
	s, err := NewServer(noAuth, nil,
		func() string { return "trusted.example" }, func() string { return "sandbox.example" },
		nil, nil, []WebUI{sysUI{}})
	require.NoError(t, err)

	get := func(host, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	t.Run("AuthNone Page renders the app document (200)", func(t *testing.T) {
		rec := get("trusted.example", "/ok")
		assert.Equal(t, 200, rec.Code)
		assert.Contains(t, rec.Body.String(), `data-app="system"`)
		assert.Contains(t, rec.Body.String(), `{"ok":true}`)
	})
	t.Run("AuthAuthenticated without cookie renders styled 401 system page", func(t *testing.T) {
		rec := get("trusted.example", "/needauth")
		assert.Equal(t, 401, rec.Code)
		assert.Contains(t, rec.Body.String(), `data-app="system"`)
		assert.Contains(t, rec.Body.String(), `"kind":"unauthorized"`)
	})
	t.Run("Build PageError renders the mapped status + kind", func(t *testing.T) {
		rec := get("trusted.example", "/boom")
		assert.Equal(t, 410, rec.Code)
		assert.Contains(t, rec.Body.String(), `"kind":"expired"`)
	})
	t.Run("unknown host -> styled 404 system page", func(t *testing.T) {
		rec := get("nope.example", "/ok")
		assert.Equal(t, 404, rec.Code)
		assert.Contains(t, rec.Body.String(), `data-app="system"`)
	})
}

// TestServeRoutePageAppMissingFromManifest_LogsAndRendersErrorNotBlank proves
// the fix for a shipped hole: serveRoute's `entry := s.manifest[rt.Page.App]`
// used to be an unchecked map lookup, so a Page.App absent from the built
// manifest (an unregistered app, or a page whose bundle predates the last
// `mage web:build`) silently rendered `<div id="root" data-app="...">` with
// EMPTY Scripts/CSS and HTTP 200 — a blank page with nothing in the logs, the
// exact silent-failure shape AGENTS.md's no-silent-errors rule forbids. This
// is deliberately triggered with a synthetic, never-registered app name
// rather than "agent-ui" specifically: pkg/web/webui/webassets/dist/manifest.json
// does not have an "agent-ui" entry as of this test (a later task's
// `mage web:build` adds it), and this test must keep passing once it does.
func TestServeRoutePageAppMissingFromManifest_LogsAndRendersErrorNotBlank(t *testing.T) {
	const missingApp = "no-such-app-ever-in-the-manifest"

	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	ui := pageUI{routes: []Route{{
		Origin: OriginTrusted, Pattern: "/missing-app", Auth: AuthNone,
		Page: &Page{App: missingApp, Build: func(context.Context, *http.Request) (any, PageMeta, error) {
			return map[string]any{"ok": true}, PageMeta{Title: "whatever"}, nil
		}},
	}}}
	noAuth := func(*http.Request) (string, bool) { return "", false }
	s, err := NewServer(noAuth, nil,
		func() string { return "trusted.example" }, func() string { return "sandbox.example" },
		nil, nil, []WebUI{ui})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "http://trusted.example/missing-app", nil)
	req = req.WithContext(log.IntoContext(req.Context(), capLogger))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"a manifest miss must render a styled error, never a bare 200")
	assert.Contains(t, rec.Body.String(), `data-app="system"`,
		"must fall back to the styled system page, not the missing app's own (script-less) shell")
	assert.NotContains(t, rec.Body.String(), missingApp,
		"the missing app's own document must never be emitted — only the system fallback")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "a manifest miss must be logged (INFO+), not silent")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, missingApp, "log must name the missing app so an operator can locate the misconfigured route")
}
