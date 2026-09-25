package webui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAuthzFixtureServer(t *testing.T, authed bool, authorize func(ctx context.Context, subject string, r *http.Request) error) *Server {
	t.Helper()
	ui := stubUI{routes: []Route{{
		Origin: OriginTrusted, Pattern: "/gated", Methods: []string{http.MethodGet},
		Auth: AuthLoginIfNecessary, Authorize: authorize,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }),
	}}}
	s, err := NewServer(
		func(*http.Request) (string, bool) {
			if authed {
				return "user:YWRtaW4", true
			}
			return "", false
		},
		func(*http.Request) (string, bool) { return "https://login.test/oidc/login", true },
		func() string { return "trusted.test" }, func() string { return "sandbox.test" },
		nil, nil, []WebUI{ui},
	)
	require.NoError(t, err)
	return s
}

type stubUI struct{ routes []Route }

func (stubUI) Name() string          { return "stub" }
func (u stubUI) Routes(Deps) []Route { return u.routes }

func get(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://trusted.test/gated", nil)
	req.Host = "trusted.test"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func TestAuthLoginIfNecessary_RunsAuthorize(t *testing.T) {
	t.Run("unauthenticated → login redirect (unchanged)", func(t *testing.T) {
		s := newAuthzFixtureServer(t, false, func(context.Context, string, *http.Request) error { return nil })
		w := get(t, s)
		assert.Equal(t, http.StatusFound, w.Code)
	})
	t.Run("authenticated + authorized → 200", func(t *testing.T) {
		s := newAuthzFixtureServer(t, true, func(context.Context, string, *http.Request) error { return nil })
		w := get(t, s)
		assert.Equal(t, http.StatusOK, w.Code)
	})
	t.Run("authenticated + denied → 403 system page", func(t *testing.T) {
		s := newAuthzFixtureServer(t, true, func(context.Context, string, *http.Request) error { return fmt.Errorf("nope") })
		w := get(t, s)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
	t.Run("Authorize returning *PageError surfaces its status (SpiceDB error ≠ denied)", func(t *testing.T) {
		s := newAuthzFixtureServer(t, true, func(context.Context, string, *http.Request) error {
			return &PageError{Status: http.StatusInternalServerError, Kind: "error",
				Title: "Authorization unavailable", Message: "The permission check failed; try again."}
		})
		w := get(t, s)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}
