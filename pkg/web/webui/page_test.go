package webui

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pageUI struct{ routes []Route }

func (u pageUI) Name() string        { return "pageui" }
func (u pageUI) Routes(Deps) []Route { return u.routes }

func newTestServer(t *testing.T, uis []WebUI) (*Server, error) {
	t.Helper()
	return NewServer(
		func(*http.Request) (string, bool) { return "user:alice", true },
		nil,
		func() string { return "trusted.example" },
		func() string { return "sandbox.example" },
		nil,
		nil,
		uis,
	)
}

func TestMountRejectsBothHandlerAndPage(t *testing.T) {
	ui := pageUI{routes: []Route{{
		Origin: OriginTrusted, Pattern: "/p", Auth: AuthNone,
		Handler: http.NotFoundHandler(),
		Page:    &Page{App: "system", Build: func(context.Context, *http.Request) (any, PageMeta, error) { return nil, PageMeta{}, nil }},
	}}}
	_, err := newTestServer(t, []WebUI{ui})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one of Handler or Page")
}

func TestMountRejectsNeitherHandlerNorPage(t *testing.T) {
	ui := pageUI{routes: []Route{{Origin: OriginTrusted, Pattern: "/p", Auth: AuthNone}}}
	_, err := newTestServer(t, []WebUI{ui})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one of Handler or Page")
}

func TestMountRejectsPageWithEmptyApp(t *testing.T) {
	ui := pageUI{routes: []Route{{
		Origin: OriginTrusted, Pattern: "/p", Auth: AuthNone,
		Page: &Page{App: "", Build: func(context.Context, *http.Request) (any, PageMeta, error) { return nil, PageMeta{}, nil }},
	}}}
	_, err := newTestServer(t, []WebUI{ui})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Page.App")
}

func TestMountRejectsPageWithNilBuild(t *testing.T) {
	ui := pageUI{routes: []Route{{
		Origin: OriginTrusted, Pattern: "/p", Auth: AuthNone,
		Page: &Page{App: "system"}, // no Build
	}}}
	_, err := newTestServer(t, []WebUI{ui})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil Build")
}
