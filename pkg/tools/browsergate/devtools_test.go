package browsergate

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

// fakeDevtools stands in for Chrome's DevTools endpoint. Its /json/* handlers
// embed its own host in the debugger URLs (as Chrome does), so the gate's
// rewrite has a concrete upstream host to replace. recordedQuery captures the
// query string of the last non-/json request, to prove the token is stripped
// before forwarding.
type fakeDevtools struct {
	server        *httptest.Server
	host          string
	recordedQuery string
	recordedPath  string
}

func newFakeDevtools(t *testing.T) *fakeDevtools {
	t.Helper()
	fd := &fakeDevtools{}
	fd.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Browser":"Chrome","webSocketDebuggerUrl":"ws://`+fd.host+`/devtools/browser/abcd"}`)
		case "/json/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"webSocketDebuggerUrl":"ws://`+fd.host+`/devtools/page/EFGH","devtoolsFrontendUrl":"/devtools/inspector.html?ws=`+fd.host+`/devtools/page/EFGH"}]`)
		default:
			fd.recordedPath = r.URL.Path
			fd.recordedQuery = r.URL.RawQuery
			_, _ = io.WriteString(w, "upstream-ok")
		}
	}))
	fd.host = strings.TrimPrefix(fd.server.URL, "http://")
	t.Cleanup(fd.server.Close)
	return fd
}

// newDevtoolsTestGate builds a gate whose DevTools proxy targets fd, with a
// trivial allowlist (unused by the gate listener) and a quiet logger.
func newDevtoolsTestGate(t *testing.T, fd *fakeDevtools) *Gate {
	t.Helper()
	g, err := New(Config{
		ProxyAddr:   "127.0.0.1:0",
		GateAddr:    "127.0.0.1:0",
		DevtoolsURL: fd.server.URL,
		Token:       testToken,
		PolicyPath:  writePolicy(t, browserpolicy.ModeAllowlist, browserpolicy.Site{Host: "a.test"}),
	})
	require.NoError(t, err)
	g.logger = quietLogger()
	g.policy.logger = quietLogger()
	return g
}

func TestGateAuth(t *testing.T) {
	fd := newFakeDevtools(t)
	g := newDevtoolsTestGate(t, fd)
	gate := httptest.NewServer(g.GateHandler())
	t.Cleanup(gate.Close)
	client := &http.Client{Timeout: 3 * time.Second}

	cases := []struct {
		name       string
		path       string
		header     string // Authorization header value, "" for none
		query      string // raw query, "" for none
		wantStatus int
	}{
		{name: "healthz is unauthenticated: 200", path: "/healthz", wantStatus: 200},
		{name: "no token: 401", path: "/json/version", wantStatus: 401},
		{name: "wrong bearer token: 401", path: "/json/version", header: "Bearer wrong", wantStatus: 401},
		{name: "wrong query token: 401", path: "/json/version", query: "t=wrong", wantStatus: 401},
		{name: "correct bearer token: proxied 200", path: "/json/version", header: "Bearer " + testToken, wantStatus: 200},
		{name: "correct query token: proxied 200", path: "/json/version", query: "t=" + testToken, wantStatus: 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := gate.URL + tc.path
			if tc.query != "" {
				u += "?" + tc.query
			}
			req, err := http.NewRequest(http.MethodGet, u, nil)
			require.NoError(t, err)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode)
		})
	}
}

func TestGateJSONVersionRewrite(t *testing.T) {
	fd := newFakeDevtools(t)
	g := newDevtoolsTestGate(t, fd)
	gate := httptest.NewServer(g.GateHandler())
	t.Cleanup(gate.Close)
	gateHost := strings.TrimPrefix(gate.URL, "http://")

	req, err := http.NewRequest(http.MethodGet, gate.URL+"/json/version", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	s := string(body)

	assert.Contains(t, s, gateHost, "debugger URL host must be rewritten to the gate's own host")
	assert.Contains(t, s, "t="+testToken, "token must be appended so chromedp stays authenticated on the gate")
	assert.NotContains(t, s, "ws://"+fd.host, "the upstream devtools host must not survive the rewrite")
}

func TestGateJSONListRewrite(t *testing.T) {
	fd := newFakeDevtools(t)
	g := newDevtoolsTestGate(t, fd)
	gate := httptest.NewServer(g.GateHandler())
	t.Cleanup(gate.Close)
	gateHost := strings.TrimPrefix(gate.URL, "http://")

	req, err := http.NewRequest(http.MethodGet, gate.URL+"/json/list", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	s := string(body)

	assert.Contains(t, s, gateHost)
	assert.Contains(t, s, "t="+testToken)
	assert.NotContains(t, s, fd.host, "no upstream devtools host should survive in webSocketDebuggerUrl or devtoolsFrontendUrl")
}

func TestGateStripsTokenQueryBeforeForwarding(t *testing.T) {
	fd := newFakeDevtools(t)
	g := newDevtoolsTestGate(t, fd)
	gate := httptest.NewServer(g.GateHandler())
	t.Cleanup(gate.Close)

	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(gate.URL + "/devtools/page/EFGH?t=" + testToken + "&foo=bar")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "upstream-ok", string(body))

	assert.Equal(t, "/devtools/page/EFGH", fd.recordedPath)
	assert.NotContains(t, fd.recordedQuery, "t=", "the token query param must be stripped before forwarding to Chrome")
	assert.Contains(t, fd.recordedQuery, "foo=bar", "non-token query params must survive")
}
