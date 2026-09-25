package settingsui

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKubectlUse(t *testing.T) {
	t.Run("nil seam: 501, message explains why", func(t *testing.T) {
		s := startTestServer(t, Deps{})
		c := authedClient(t, s)
		resp, err := c.Post("http://"+s.Addr()+"/api/kubectl/use", "", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("seam error: 500, error surfaced verbatim", func(t *testing.T) {
		s := startTestServer(t, Deps{KubectlUse: func() error { return assertionError("kubectl switch failed") }})
		c := authedClient(t, s)
		resp, err := c.Post("http://"+s.Addr()+"/api/kubectl/use", "", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(body), "kubectl switch failed")
	})

	t.Run("success: 204, seam invoked exactly once", func(t *testing.T) {
		calls := 0
		s := startTestServer(t, Deps{KubectlUse: func() error { calls++; return nil }})
		c := authedClient(t, s)
		resp, err := c.Post("http://"+s.Addr()+"/api/kubectl/use", "", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		assert.Equal(t, 1, calls)
	})
}

func TestRevealConfig(t *testing.T) {
	t.Run("nil seam: 501", func(t *testing.T) {
		s := startTestServer(t, Deps{})
		c := authedClient(t, s)
		resp, err := c.Post("http://"+s.Addr()+"/api/reveal/config", "", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("success: 204, seam invoked exactly once", func(t *testing.T) {
		calls := 0
		s := startTestServer(t, Deps{RevealConfig: func() { calls++ }})
		c := authedClient(t, s)
		resp, err := c.Post("http://"+s.Addr()+"/api/reveal/config", "", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		assert.Equal(t, 1, calls)
	})
}

func TestRevealLogs(t *testing.T) {
	t.Run("nil seam: 501", func(t *testing.T) {
		s := startTestServer(t, Deps{})
		c := authedClient(t, s)
		resp, err := c.Post("http://"+s.Addr()+"/api/reveal/logs", "", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("success: 204, seam invoked exactly once", func(t *testing.T) {
		calls := 0
		s := startTestServer(t, Deps{RevealLogs: func() { calls++ }})
		c := authedClient(t, s)
		resp, err := c.Post("http://"+s.Addr()+"/api/reveal/logs", "", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		assert.Equal(t, 1, calls)
	})
}
