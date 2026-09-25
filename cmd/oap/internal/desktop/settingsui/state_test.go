package settingsui

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestState_KubectlCurrentOverlay proves GET /api/state overlays
// KubectlCurrent from Deps.KubectlCurrent (see Server.currentState) —
// Deps.State itself never sets it, so this is the only place the field can
// come from.
func TestState_KubectlCurrentOverlay(t *testing.T) {
	cases := []struct {
		name string
		seam func() bool
		want bool
	}{
		{name: "nil seam overlays false", seam: nil, want: false},
		{name: "seam reports true", seam: func() bool { return true }, want: true},
		{name: "seam reports false", seam: func() bool { return false }, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := startTestServer(t, Deps{KubectlCurrent: tc.seam})
			c := authedClient(t, s)
			resp, err := c.Get("http://" + s.Addr() + "/api/state")
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			var got State
			require.NoError(t, json.Unmarshal(body, &got))
			assert.Equal(t, tc.want, got.KubectlCurrent)
		})
	}
}

// TestEvents_KubectlCurrentOverlay proves the SSE frames carry the same
// overlay as GET /api/state, not a raw Deps.State() snapshot.
func TestEvents_KubectlCurrentOverlay(t *testing.T) {
	s := startTestServer(t, Deps{KubectlCurrent: func() bool { return true }})
	c := authedClient(t, s)
	req, err := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/api/events", nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	first, err := readSSEEvent(t, reader)
	require.NoError(t, err)
	assert.Contains(t, first, `"kubectlCurrent":true`)
}
