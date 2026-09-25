package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a goroutine-safe io.Writer used in tests where Run streams
// status output from a background goroutine while the test inspects the
// output from the main goroutine. A plain strings.Builder races under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeAuthServer builds an httptest server that:
//   - GET /authorize → immediately 302s to redirect_uri with fake code + state
//   - POST /token    → returns a fake access_token
func fakeAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirectURI := q.Get("redirect_uri")
		state := q.Get("state")
		if redirectURI == "" {
			http.Error(w, "missing redirect_uri", http.StatusBadRequest)
			return
		}
		target, err := url.Parse(redirectURI)
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		rq := target.Query()
		rq.Set("code", "test-auth-code-123")
		rq.Set("state", state)
		target.RawQuery = rq.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "parse form", http.StatusBadRequest)
			return
		}
		if r.FormValue("grant_type") != "authorization_code" {
			http.Error(w, "wrong grant_type", http.StatusBadRequest)
			return
		}
		if r.FormValue("code") != "test-auth-code-123" {
			http.Error(w, "wrong code", http.StatusBadRequest)
			return
		}
		if r.FormValue("code_verifier") == "" {
			http.Error(w, "missing code_verifier", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Token{
			AccessToken: "test-access-token-abc",
			TokenType:   "bearer",
			ExpiresIn:   3600,
			Scope:       "read write",
		})
	})

	return httptest.NewServer(mux)
}

// TestRun_HappyPath exercises the full flow end-to-end with a stubbed
// auth server and browser-open hook.
func TestRun_HappyPath(t *testing.T) {
	authSrv := fakeAuthServer(t)
	t.Cleanup(authSrv.Close)

	meta := &Metadata{
		AuthorizationEndpoint: authSrv.URL + "/authorize",
		TokenEndpoint:         authSrv.URL + "/token",
	}

	var capturedURL string
	openBrowser := func(authURL string) error {
		capturedURL = authURL
		// Simulate browser: follow the auth URL and let the fake auth server
		// 302 to our callback. The simplest approach: follow with
		// http.DefaultClient.
		resp, err := http.Get(authURL) //nolint:noctx // test only
		if err != nil {
			return fmt.Errorf("stub browser: %w", err)
		}
		_ = resp.Body.Close()
		return nil
	}

	var out strings.Builder
	tok, err := Run(context.Background(), Opts{
		Metadata:    meta,
		ClientID:    "test-client",
		Scope:       "read write",
		Timeout:     10 * time.Second,
		Out:         &out,
		OpenBrowser: openBrowser,
		HTTPClient:  http.DefaultClient,
	})
	require.NoError(t, err, "Run; status output: %s", out.String())

	assert.NotEmpty(t, tok.AccessToken, "AccessToken")
	assert.Equal(t, "bearer", tok.TokenType)
	assert.Equal(t, 3600, tok.ExpiresIn)
	assert.Contains(t, capturedURL, "/authorize", "capturedURL should be an authorization URL")
	assert.Contains(t, capturedURL, "code_challenge", "capturedURL must carry PKCE code_challenge")
	assert.Contains(t, capturedURL, "S256", "capturedURL must carry code_challenge_method=S256")

	outStr := out.String()
	assert.Contains(t, outStr, "Open this URL", "status output should prompt the user to open the URL")
	assert.Contains(t, outStr, "Waiting for callback", "status output should mention waiting for callback")
}

// TestRun_NoBrowser verifies NoBrowser=true doesn't call OpenBrowser.
func TestRun_NoBrowser(t *testing.T) {
	authSrv := fakeAuthServer(t)
	t.Cleanup(authSrv.Close)

	meta := &Metadata{
		AuthorizationEndpoint: authSrv.URL + "/authorize",
		TokenEndpoint:         authSrv.URL + "/token",
	}

	// captureOpen lets us assert OpenBrowser was NOT called. With
	// NoBrowser=true, Run never invokes it, so urlCh stays empty.
	urlCh := make(chan string, 1)
	captureOpen := func(authURL string) error {
		urlCh <- authURL
		return nil
	}

	var out syncBuffer
	type runResult struct {
		tok *Token
		err error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		tok, err := Run(context.Background(), Opts{
			Metadata:    meta,
			ClientID:    "test-client",
			NoBrowser:   true,
			Timeout:     5 * time.Second,
			Out:         &out,
			OpenBrowser: captureOpen,
			HTTPClient:  http.DefaultClient,
		})
		resultCh <- runResult{tok, err}
	}()

	// Give Run a moment to write the URL to out, then parse it.
	time.Sleep(200 * time.Millisecond)
	outStr := out.String()
	authURL := ""
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, authSrv.URL) {
			authURL = line
			break
		}
	}
	require.NotEmpty(t, authURL, "could not parse auth URL from output: %q", outStr)

	// Drive the callback.
	resp, err := http.Get(authURL) //nolint:noctx
	require.NoError(t, err, "drive callback")
	_ = resp.Body.Close()

	result := <-resultCh
	require.NoError(t, result.err, "Run with NoBrowser")
	// captureOpen must NOT have been called.
	select {
	case <-urlCh:
		t.Error("OpenBrowser was called despite NoBrowser=true")
	default:
	}
	assert.NotEmpty(t, result.tok.AccessToken, "AccessToken")
}

// TestRun_StateMismatch verifies that a mismatched state triggers an error.
func TestRun_StateMismatch(t *testing.T) {
	// Build a fake auth server that sends the wrong state back.
	badAuthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authorize" {
			redirectURI := r.URL.Query().Get("redirect_uri")
			target, _ := url.Parse(redirectURI)
			rq := target.Query()
			rq.Set("code", "some-code")
			rq.Set("state", "WRONG-STATE")
			target.RawQuery = rq.Encode()
			http.Redirect(w, r, target.String(), http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(badAuthSrv.Close)

	meta := &Metadata{
		AuthorizationEndpoint: badAuthSrv.URL + "/authorize",
		TokenEndpoint:         badAuthSrv.URL + "/token",
	}

	openBrowser := func(authURL string) error {
		resp, err := http.Get(authURL) //nolint:noctx
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}

	_, err := Run(context.Background(), Opts{
		Metadata:    meta,
		ClientID:    "client",
		Timeout:     5 * time.Second,
		Out:         &strings.Builder{},
		OpenBrowser: openBrowser,
		HTTPClient:  http.DefaultClient,
	})
	require.Error(t, err, "expected error for state mismatch")
	assert.Contains(t, err.Error(), "state mismatch")
}
