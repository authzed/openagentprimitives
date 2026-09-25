package setupui_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/setupui"
)

// startServer starts a Server on an OS-assigned loopback port and
// registers a cleanup to shut it down. Returns the server and its base URL.
func startServer(t *testing.T, tl *setupui.Timeline, onConfig setupui.OnConfigFunc) (*setupui.Server, string) {
	t.Helper()
	srv := setupui.New(tl, onConfig)
	require.NoError(t, srv.Start(0))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, srv.URL()
}

func TestServer_AddrIsLoopbackOnly(t *testing.T) {
	srv, _ := startServer(t, setupui.NewTimeline(nil), nil)
	assert.True(t, strings.HasPrefix(srv.Addr(), "127.0.0.1:"), "Addr() = %q, want 127.0.0.1:<port>", srv.Addr())
}

func TestServer_Index(t *testing.T) {
	_, base := startServer(t, setupui.NewTimeline(nil), nil)

	resp, err := http.Get(base + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	// The real page (not the old placeholder) must be served: assert the
	// known screen ids and the routes it's expected to talk to are present.
	assert.Contains(t, html, `id="config-form"`, "config form screen id must be present")
	assert.Contains(t, html, `id="timeline"`, "timeline screen id must be present")
	assert.Contains(t, html, `id="ready-screen"`, "ready screen id must be present")
	assert.Contains(t, html, `id="failed-screen"`, "failed screen id must be present")
	assert.Contains(t, html, "/events")
	assert.Contains(t, html, "/progress")
	assert.Contains(t, html, "/config")
	assert.Contains(t, html, "/open-dashboard")
	assert.NotContains(t, html, "placeholder page", "the placeholder page must have been replaced")
}

// TestServer_IndexOffersEveryKnownModelProvider keeps the served form's
// provider <select> and desktop.Config's fail-closed provider gate in step in
// both directions: a provider the form offers but Config rejects is a form
// that 400s on submit, and a provider Config accepts but the form omits is one
// no desktop user can ever pick.
func TestServer_IndexOffersEveryKnownModelProvider(t *testing.T) {
	_, base := startServer(t, setupui.NewTimeline(nil), nil)

	resp, err := http.Get(base + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	offered := optionValues(t, string(body))
	assert.Equal(t, desktop.ModelProviderNames(), offered, "the form's provider options must be exactly the providers desktop.Config accepts")
}

// optionValues returns the sorted `value` attributes of the provider
// <select>'s <option> elements. It reads the raw HTML rather than a DOM: the
// page is a hand-written static document, so a regexp over the one <select>
// this test cares about is enough and pulls in no parser.
func optionValues(t *testing.T, html string) []string {
	t.Helper()
	sel := regexp.MustCompile(`(?s)<select id="provider-select".*?</select>`).FindString(html)
	require.NotEmpty(t, sel, "provider <select> not found in the served page")
	var got []string
	for _, m := range regexp.MustCompile(`<option value="([^"]*)"`).FindAllStringSubmatch(sel, -1) {
		got = append(got, m[1])
	}
	slices.Sort(got)
	return got
}

func TestServer_IndexUnknownPathIs404(t *testing.T) {
	_, base := startServer(t, setupui.NewTimeline(nil), nil)

	resp, err := http.Get(base + "/does-not-exist")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestServer_Progress(t *testing.T) {
	tl := setupui.NewTimeline([]string{"one", "two"})
	tl.Begin("one", 1000)
	_, base := startServer(t, tl, nil)

	resp, err := http.Get(base + "/progress")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	var got setupui.TimelineState
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.Equal(t, tl.Snapshot(), got)
	assert.Equal(t, setupui.StatusActive, got.Steps[0].Status)
}

func TestServer_ProgressWrongMethod(t *testing.T) {
	_, base := startServer(t, setupui.NewTimeline(nil), nil)

	resp, err := http.Post(base+"/progress", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestServer_Config(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantStatus     int
		wantCalled     bool
		wantModelCfg   desktop.ModelConfig
		onConfigErr    error
		wantBodySubstr string
	}{
		{
			name:         "anthropic, no model: 200 + callback invoked, model left to the provider default",
			body:         `{"provider":"anthropic","apiKey":"sk-ant-test","password":"hunter2"}`,
			wantStatus:   http.StatusOK,
			wantCalled:   true,
			wantModelCfg: desktop.ModelConfig{Provider: "anthropic", APIKey: "sk-ant-test"},
		},
		{
			name:         "openai, no model: 200 + callback invoked",
			body:         `{"provider":"openai","apiKey":"sk-test","password":"hunter2"}`,
			wantStatus:   http.StatusOK,
			wantCalled:   true,
			wantModelCfg: desktop.ModelConfig{Provider: "openai", APIKey: "sk-test"},
		},
		{
			name:         "openrouter with a namespaced model: 200, the vendor/model id reaches the callback intact",
			body:         `{"provider":"openrouter","apiKey":"sk-or-test","model":"anthropic/claude-3.5-sonnet","password":"hunter2"}`,
			wantStatus:   http.StatusOK,
			wantCalled:   true,
			wantModelCfg: desktop.ModelConfig{Provider: "openrouter", APIKey: "sk-or-test", Name: "anthropic/claude-3.5-sonnet"},
		},
		{
			name:           "unknown provider: 400, callback not invoked",
			body:           `{"provider":"bogus","apiKey":"sk-ant-test","password":"hunter2"}`,
			wantStatus:     http.StatusBadRequest,
			wantCalled:     false,
			wantBodySubstr: "unknown model provider",
		},
		{
			name:           "model belonging to another provider: 400 naming the real owner, callback not invoked",
			body:           `{"provider":"openai","apiKey":"sk-test","model":"claude-sonnet-5","password":"hunter2"}`,
			wantStatus:     http.StatusBadRequest,
			wantCalled:     false,
			wantBodySubstr: `is served by "anthropic"`,
		},
		{
			name:           "empty apiKey: 400, callback not invoked",
			body:           `{"provider":"anthropic","apiKey":"","password":"hunter2"}`,
			wantStatus:     http.StatusBadRequest,
			wantCalled:     false,
			wantBodySubstr: "apiKey required",
		},
		{
			name:           "malformed JSON: 400, callback not invoked",
			body:           `{not json`,
			wantStatus:     http.StatusBadRequest,
			wantCalled:     false,
			wantBodySubstr: "decode request body",
		},
		{
			name:           "onConfig error (e.g. empty password): 500",
			body:           `{"provider":"anthropic","apiKey":"sk-ant-test","password":""}`,
			wantStatus:     http.StatusInternalServerError,
			wantCalled:     true,
			onConfigErr:    assert.AnError,
			wantBodySubstr: "apply config",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			var gotCfg desktop.Config
			var gotPassword string
			onConfig := func(cfg desktop.Config, password string) error {
				called = true
				gotCfg = cfg
				gotPassword = password
				return tc.onConfigErr
			}
			_, base := startServer(t, setupui.NewTimeline(nil), onConfig)

			resp, err := http.Post(base+"/config", "application/json", strings.NewReader(tc.body))
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			assert.Equal(t, tc.wantCalled, called)
			if tc.wantCalled && tc.onConfigErr == nil {
				assert.Equal(t, tc.wantModelCfg, gotCfg.Model)
				assert.Equal(t, "hunter2", gotPassword)
				assert.Empty(t, gotCfg.AdminPasswordHash, "handleConfig must never populate AdminPasswordHash itself")
			}
			if tc.wantBodySubstr != "" {
				buf := new(bytes.Buffer)
				_, _ = buf.ReadFrom(resp.Body)
				assert.Contains(t, buf.String(), tc.wantBodySubstr)
			}
		})
	}
}

func TestServer_OpenDashboard(t *testing.T) {
	cases := []struct {
		name       string
		register   bool
		cbErr      error
		wantStatus int
		wantCalled bool
	}{
		{
			name:       "no callback registered: 404, no-op",
			register:   false,
			wantStatus: http.StatusNotFound,
			wantCalled: false,
		},
		{
			name:       "callback registered and succeeds: 200",
			register:   true,
			wantStatus: http.StatusOK,
			wantCalled: true,
		},
		{
			name:       "callback registered and errors: 500",
			register:   true,
			cbErr:      assert.AnError,
			wantStatus: http.StatusInternalServerError,
			wantCalled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			srv := setupui.New(setupui.NewTimeline(nil), nil)
			if tc.register {
				srv.SetOnOpenDashboard(func() error {
					called = true
					return tc.cbErr
				})
			}
			require.NoError(t, srv.Start(0))
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			})

			resp, err := http.Post(srv.URL()+"/open-dashboard", "application/json", nil)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			assert.Equal(t, tc.wantCalled, called)
		})
	}
}

func TestServer_OpenDashboardWrongMethod(t *testing.T) {
	_, base := startServer(t, setupui.NewTimeline(nil), nil)

	resp, err := http.Get(base + "/open-dashboard")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestServer_ConfigWrongMethod(t *testing.T) {
	_, base := startServer(t, setupui.NewTimeline(nil), nil)

	resp, err := http.Get(base + "/config")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// TestServer_LoopbackGuard exercises the defense-in-depth Host/Origin
// check: a forged, non-loopback Host or Origin header must be rejected
// even though the listener only ever binds 127.0.0.1.
func TestServer_LoopbackGuard(t *testing.T) {
	_, base := startServer(t, setupui.NewTimeline(nil), nil)

	cases := []struct {
		name   string
		host   string
		origin string
	}{
		{name: "forged Host header", host: "evil.example.com"},
		{name: "forged Origin header", origin: "http://evil.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, base+"/progress", nil)
			require.NoError(t, err)
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		})
	}
}

func TestServer_LoopbackGuardAllowsLocalOrigin(t *testing.T) {
	_, base := startServer(t, setupui.NewTimeline(nil), nil)

	req, err := http.NewRequest(http.MethodGet, base+"/progress", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", base) // http://127.0.0.1:<port>, matches the server's own origin
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestServer_EventsInitialSnapshot connects to /events and reads the first
// SSE frame, confirming it carries the current snapshot.
func TestServer_EventsInitialSnapshot(t *testing.T) {
	tl := setupui.NewTimeline([]string{"one", "two"})
	tl.Begin("one", 777)
	_, base := startServer(t, tl, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	frame, err := readOneSSEFrame(t, resp.Body)
	require.NoError(t, err)

	var got setupui.TimelineState
	require.NoError(t, json.Unmarshal(frame, &got))
	assert.Equal(t, setupui.PhaseRunning, got.Phase)
	assert.Equal(t, setupui.StatusActive, got.Steps[0].Status)
}

// TestServer_EventsPushesUpdate confirms a Timeline mutation after connect
// is pushed as a second SSE frame.
func TestServer_EventsPushesUpdate(t *testing.T) {
	tl := setupui.NewTimeline([]string{"one", "two"})
	_, base := startServer(t, tl, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)

	// First frame: initial snapshot (still configuring/pending).
	first, err := scanNextData(scanner)
	require.NoError(t, err)
	var firstState setupui.TimelineState
	require.NoError(t, json.Unmarshal(first, &firstState))
	assert.Equal(t, setupui.PhaseConfiguring, firstState.Phase)

	// Mutate; the second frame must reflect it.
	tl.Begin("one", 42)

	second, err := scanNextData(scanner)
	require.NoError(t, err)
	var secondState setupui.TimelineState
	require.NoError(t, json.Unmarshal(second, &secondState))
	assert.Equal(t, setupui.PhaseRunning, secondState.Phase)
	assert.Equal(t, setupui.StatusActive, secondState.Steps[0].Status)
}

// readOneSSEFrame reads exactly one "data: ...\n\n" frame's payload.
func readOneSSEFrame(t *testing.T, body io.Reader) ([]byte, error) {
	t.Helper()
	scanner := bufio.NewScanner(body)
	return scanNextData(scanner)
}

// scanNextData advances scanner line-by-line until it finds a "data: "
// line, and returns its payload (the blank terminator line is left
// unconsumed in the buffered reader's next read, which is fine since each
// frame is read independently here).
func scanNextData(scanner *bufio.Scanner) ([]byte, error) {
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			return []byte(strings.TrimPrefix(line, "data: ")), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, context.DeadlineExceeded
}
