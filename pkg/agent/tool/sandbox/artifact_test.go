package sandbox_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
)

func TestHTTPArtifactClientGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/debug/artifact", r.URL.Path, "request path")
		assert.Equal(t, "Bearer tok-1", r.Header.Get("Authorization"), "auth header")
		assert.Equal(t, "mem://default/s1/uuid/stdout", r.URL.Query().Get("ref"), "ref query param")
		_, _ = w.Write([]byte("hello stdout"))
	}))
	t.Cleanup(srv.Close)

	c := sandbox.NewHTTPArtifactClient(srv.URL, "tok-1")
	rc, err := c.Get(context.Background(), "mem://default/s1/uuid/stdout")
	require.NoError(t, err, "Get must succeed")
	t.Cleanup(func() { _ = rc.Close() })
	b, err := io.ReadAll(rc)
	require.NoError(t, err, "ReadAll body")
	assert.Equal(t, "hello stdout", string(b), "body")
}

func TestHTTPArtifactClientNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := sandbox.NewHTTPArtifactClient(srv.URL, "tok")
	_, err := c.Get(context.Background(), "mem://x")
	require.Error(t, err, "expected not-found error")
}
