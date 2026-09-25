package settingsui

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readSSEEvent reads lines from an SSE stream until a blank line (the frame
// terminator) and returns the concatenated "data:" payload. Blank/comment
// (":") lines that precede any data (e.g. a stray keepalive) are skipped.
func readSSEEvent(t *testing.T, r *bufio.Reader) (string, error) {
	t.Helper()
	var data strings.Builder
	sawData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if sawData {
				return data.String(), nil
			}
			// blank line with no data seen yet (shouldn't normally happen) — keep reading
			continue
		case strings.HasPrefix(line, ":"):
			// comment/keepalive line — ignore and keep reading this frame
			continue
		case strings.HasPrefix(line, "data:"):
			if sawData {
				data.WriteString("\n")
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			sawData = true
		}
	}
}

func TestIndex_ServesSettingsShell(t *testing.T) {
	// Exercise the REAL "settings" webassets entry (New's default appKey) — the
	// override seam stays for tests that need a different entry.
	s := startTestServer(t, Deps{})
	c := authedClient(t, s)
	resp, err := c.Get("http://" + s.Addr() + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), `data-app="settings"`)
}

func TestEvents_SSEEmitsStateChange(t *testing.T) {
	var st atomic.Value
	st.Store(State{Running: false, Phase: "stopped"})
	s := startTestServer(t, Deps{State: func() State { return st.Load().(State) }})
	s.pollInterval = 10 * time.Millisecond
	c := authedClient(t, s)
	req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/api/events", nil)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	first, err := readSSEEvent(t, reader)
	require.NoError(t, err)
	assert.Contains(t, first, `"phase":"stopped"`)
	st.Store(State{Running: true, Phase: "running"})
	next, err := readSSEEvent(t, reader)
	require.NoError(t, err)
	assert.Contains(t, next, `"phase":"running"`)
}
