package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent/tools"
)

// TestLocalCallbackHappy uses the 2-phase API: call Start to get the URL
// before blocking, fire an HTTP GET with query params, then await returns
// the params.
func TestLocalCallbackHappy(t *testing.T) {
	ctx := context.Background()
	url, await, err := tools.LocalCallbackStart(ctx, 5*time.Second)
	require.NoError(t, err, "LocalCallbackStart")
	require.True(t, strings.HasPrefix(url, "http://127.0.0.1:"), "unexpected url %q", url)

	// Fire a request concurrently — await blocks until the request lands.
	getErr := make(chan error, 1)
	go func() {
		target := fmt.Sprintf("%s?code=abc123&state=xyz", url)
		resp, err := http.Get(target) //nolint:noctx
		if err != nil {
			getErr <- err
			return
		}
		resp.Body.Close()
		getErr <- nil
	}()

	params, err := await()
	require.NoError(t, err, "await")
	require.NoError(t, <-getErr, "GET callback")
	assert.Equal(t, "abc123", params["code"], "params[code]")
	assert.Equal(t, "xyz", params["state"], "params[state]")
}

// TestLocalCallbackTimeout verifies that awaiter returns an error when no
// request arrives within the timeout window.
func TestLocalCallbackTimeout(t *testing.T) {
	ctx := context.Background()
	_, await, err := tools.LocalCallbackStart(ctx, 200*time.Millisecond)
	require.NoError(t, err, "LocalCallbackStart")

	_, err = await()
	require.Error(t, err, "expected timeout error")
	assert.Contains(t, err.Error(), "timed out", "error should mention 'timed out'")
}

// urlCapturingWriter is an io.Writer that scrapes the listener URL out of
// the stderr line LocalCallbackRun emits before blocking on the awaiter,
// letting tests fire the callback request against a Run that is already
// in flight.
//
// This helper depends on the exact "listening on http://127.0.0.1:..." line
// that LocalCallbackRun writes to stderr before calling the awaiter. There
// is no cleaner coordination seam because Run owns the listener internally
// and blocks until the first request arrives — the stderr write is the
// only signal that the listener is ready.
type urlCapturingWriter struct {
	ch chan string
}

func (w *urlCapturingWriter) Write(p []byte) (int, error) {
	s := string(p)
	if idx := strings.Index(s, "http://127.0.0.1:"); idx >= 0 {
		select {
		case w.ch <- strings.TrimSpace(s[idx:]):
		default:
		}
	}
	return len(p), nil
}

// runLocalCallback drives LocalCallbackRun in a goroutine, discovers the
// listener URL from the stderr writer, fires a GET carrying query, and
// returns the tool's output/error.
func runLocalCallback(t *testing.T, args, query string,
	store func(context.Context, builtins.StoreValue) error) (string, error) {
	t.Helper()

	w := &urlCapturingWriter{ch: make(chan string, 1)}
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := tools.LocalCallbackRun(context.Background(), json.RawMessage(args), w, store)
		done <- result{out, err}
	}()

	select {
	case listenerURL := <-w.ch:
		resp, err := http.Get(listenerURL + "?" + query) //nolint:noctx
		require.NoError(t, err, "GET callback")
		resp.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("listener URL never appeared on stderr")
	}

	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("LocalCallbackRun did not return after callback fired")
		return "", nil // unreachable
	}
}

// TestLocalCallbackRunJSON exercises LocalCallbackRun end to end: the URL
// is discovered from the stderr line, a GET fires, and the JSON output
// carries the (non-credential) params and the listener URL.
func TestLocalCallbackRunJSON(t *testing.T) {
	storeCalled := false
	out, err := runLocalCallback(t, `{}`, "token=tok-999",
		func(context.Context, builtins.StoreValue) error {
			storeCalled = true
			return nil
		})
	require.NoError(t, err, "LocalCallbackRun")
	assert.Contains(t, out, `"token":"tok-999"`, "output missing token param")
	assert.Contains(t, out, `"url":"http://127.0.0.1:`, "output missing url field")
	assert.False(t, storeCalled, "store must not fire without oauth_exchange + code")
}

// TestLocalCallbackRedactsCodeWithoutExchangeConfig verifies that a captured
// authorization code never reaches the tool output, even when no
// oauth_exchange config was given, and that a guidance note is returned.
func TestLocalCallbackRedactsCodeWithoutExchangeConfig(t *testing.T) {
	storeCalled := false
	out, err := runLocalCallback(t, `{}`, "code=supersecretcode&state=xyz",
		func(context.Context, builtins.StoreValue) error {
			storeCalled = true
			return nil
		})
	require.NoError(t, err, "LocalCallbackRun")
	assert.NotContains(t, out, "supersecretcode", "raw code leaked into tool output")

	var res tools.LocalCallbackResult
	require.NoError(t, json.Unmarshal([]byte(out), &res), "unmarshal result")
	assert.Equal(t, "(redacted)", res.Params["code"], "code must be redacted")
	assert.Equal(t, "xyz", res.Params["state"], "non-credential params stay visible")
	assert.False(t, res.Exchanged, "no exchange config — Exchanged must be false")
	assert.False(t, res.Stored, "no exchange config — Stored must be false")
	assert.NotEmpty(t, res.Note, "a note must explain the discarded code")
	assert.False(t, storeCalled, "store must not fire without oauth_exchange")
}

// TestLocalCallbackExchangesAndStoresInTool verifies the in-tool code→token
// exchange: the token endpoint receives the RFC 6749 form, the store callback
// receives the full OAuth bundle, and neither the code nor the tokens appear
// in the tool output.
func TestLocalCallbackExchangesAndStoresInTool(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm(), "ParseForm")
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-1","token_type":"Bearer","refresh_token":"rt-1","expires_in":3600,"scope":"read"}`)
	}))
	t.Cleanup(srv.Close)

	// LocalCallbackHTTPClient is the SSRF-guarded production client, which
	// refuses the loopback httptest server. Swap it for a plain client for
	// the duration of this test (LocalCallbackHTTPClient is the documented seam).
	old := tools.LocalCallbackHTTPClient
	tools.LocalCallbackHTTPClient = http.DefaultClient
	t.Cleanup(func() { tools.LocalCallbackHTTPClient = old })

	var stored *builtins.OAuthValue
	args := fmt.Sprintf(`{"oauth_exchange":{"token_endpoint":%q,"client_id":"cid","code_verifier":"ver"}}`, srv.URL)
	out, err := runLocalCallback(t, args, "code=supersecretcode&state=xyz",
		func(_ context.Context, v builtins.StoreValue) error {
			stored = v.OAuth
			return nil
		})
	require.NoError(t, err, "LocalCallbackRun")

	require.NotNil(t, gotForm, "token endpoint was never called")
	assert.Equal(t, "authorization_code", gotForm.Get("grant_type"), "form grant_type")
	assert.Equal(t, "supersecretcode", gotForm.Get("code"), "form code")
	assert.Equal(t, "ver", gotForm.Get("code_verifier"), "form code_verifier")
	assert.Equal(t, "cid", gotForm.Get("client_id"), "form client_id")
	assert.Contains(t, gotForm.Get("redirect_uri"), "127.0.0.1", "form redirect_uri")

	require.NotNil(t, stored, "store callback was never invoked")
	assert.Equal(t, "at-1", stored.AccessToken, "stored access token")
	assert.Equal(t, "rt-1", stored.RefreshToken, "stored refresh token")
	assert.Equal(t, 3600, stored.ExpiresIn, "stored expires_in")
	assert.Equal(t, srv.URL, stored.TokenEndpoint, "stored token endpoint")
	assert.Equal(t, "cid", stored.ClientID, "stored client_id")
	assert.Equal(t, "read", stored.Scope, "stored scope")

	assert.NotContains(t, out, "supersecretcode", "raw code leaked into tool output")
	assert.NotContains(t, out, "at-1", "access token leaked into tool output")
	assert.NotContains(t, out, "rt-1", "refresh token leaked into tool output")

	var res tools.LocalCallbackResult
	require.NoError(t, json.Unmarshal([]byte(out), &res), "unmarshal result")
	assert.Equal(t, "(redacted)", res.Params["code"], "code must be redacted")
	assert.True(t, res.Exchanged, "Exchanged must be true after a successful exchange")
	assert.True(t, res.Stored, "Stored must be true after a successful store")
}

// TestLocalCallbackExchangeFailure_ErrorWithoutCode verifies that a failed
// exchange surfaces as an error whose text does not embed the raw code, and
// that nothing is stored.
func TestLocalCallbackExchangeFailure_ErrorWithoutCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	t.Cleanup(srv.Close)

	// Override the SSRF-guarded client so the exchange can reach the loopback
	// httptest server (LocalCallbackHTTPClient is the documented seam).
	old := tools.LocalCallbackHTTPClient
	tools.LocalCallbackHTTPClient = http.DefaultClient
	t.Cleanup(func() { tools.LocalCallbackHTTPClient = old })

	storeCalled := false
	args := fmt.Sprintf(`{"oauth_exchange":{"token_endpoint":%q,"client_id":"cid"}}`, srv.URL)
	_, err := runLocalCallback(t, args, "code=supersecretcode",
		func(context.Context, builtins.StoreValue) error {
			storeCalled = true
			return nil
		})
	require.Error(t, err, "expected exchange failure to surface as an error")
	assert.NotContains(t, err.Error(), "supersecretcode", "raw code leaked into error text")
	assert.False(t, storeCalled, "store must not fire after a failed exchange")
}

// TestLocalCallbackExchangeMissingFields verifies that an oauth_exchange
// block missing token_endpoint/client_id errors immediately, before any
// listener is started.
func TestLocalCallbackExchangeMissingFields(t *testing.T) {
	_, err := tools.LocalCallbackRun(context.Background(),
		json.RawMessage(`{"oauth_exchange":{"token_endpoint":"","client_id":""}}`),
		nil,
		func(context.Context, builtins.StoreValue) error { return nil })
	require.Error(t, err, "expected immediate arg error")
	assert.Contains(t, err.Error(), "token_endpoint", "error should name the missing fields")
}
