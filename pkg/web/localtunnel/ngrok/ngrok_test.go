package ngrok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNgrokTunnel_StartStop is a live tunnel test against ngrok's
// cloud service. Skipped unless NGROK_AUTHTOKEN is set.
func TestNgrokTunnel_StartStop(t *testing.T) {
	if os.Getenv("NGROK_AUTHTOKEN") == "" {
		t.Skip("set NGROK_AUTHTOKEN to run the live ngrok tunnel test")
	}

	// Local httptest server as the proxy target.
	const body = "hello from local"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(target.Close)

	tn := &Tunnel{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	publicURL, err := tn.Start(ctx, target.URL)
	require.NoError(t, err, "Start should succeed with NGROK_AUTHTOKEN set")
	require.NotEmpty(t, publicURL, "Start should return a public URL")
	t.Cleanup(func() { _ = tn.Stop() })

	// Verify the tunnel actually forwards: GET <publicURL> → body.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicURL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))

	// Stop returns nil; second call is a no-op.
	require.NoError(t, tn.Stop())
	require.NoError(t, tn.Stop(), "second Stop is a no-op")
}

// TestNgrokTunnel_NoAuthToken_Errors verifies the no-token error path
// without needing a real ngrok account.
func TestNgrokTunnel_NoAuthToken_Errors(t *testing.T) {
	// Clear NGROK_AUTHTOKEN for the duration of the test, restore on
	// cleanup. We can't t.Parallel() here because we mutate process env.
	saved, hadSaved := os.LookupEnv("NGROK_AUTHTOKEN")
	require.NoError(t, os.Unsetenv("NGROK_AUTHTOKEN"))
	t.Cleanup(func() {
		if hadSaved {
			_ = os.Setenv("NGROK_AUTHTOKEN", saved)
		}
	})

	tn := &Tunnel{}
	_, err := tn.Start(context.Background(), "http://127.0.0.1:9000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth", "error should mention the missing auth token")
}

// TestNgrokTunnel_StopWithoutStart verifies Stop is safe before Start.
func TestNgrokTunnel_StopWithoutStart(t *testing.T) {
	tn := &Tunnel{}
	assert.NoError(t, tn.Stop(), "Stop before Start is a no-op")
	assert.NoError(t, tn.Stop(), "second Stop is still a no-op")
}

// TestNgrokTunnel_DoubleStart_Errors verifies a second Start call on
// an already-running tunnel returns an error rather than silently
// leaking the prior forwarder.
func TestNgrokTunnel_DoubleStart_Errors(t *testing.T) {
	if os.Getenv("NGROK_AUTHTOKEN") == "" {
		t.Skip("set NGROK_AUTHTOKEN to run the double-Start ngrok test")
	}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(target.Close)

	tn := &Tunnel{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	_, err := tn.Start(ctx, target.URL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tn.Stop() })

	_, err = tn.Start(ctx, target.URL)
	require.Error(t, err, "second Start without Stop should error")
	assert.Contains(t, err.Error(), "already started")
}
