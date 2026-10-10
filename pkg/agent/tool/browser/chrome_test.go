package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/browsergate"
)

const chromeToken = "chrome-test-token"

// requireLocalChrome skips the test unless a Chrome/Chromium binary can be
// found, mirroring chromedp's own findExecPath search (PATH names on every OS
// plus the macOS .app bundle paths). Spec §7 permits this skip, with a reason.
func requireLocalChrome(t *testing.T) {
	t.Helper()
	var names []string
	switch runtime.GOOS {
	case "darwin":
		for _, p := range []string{
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		} {
			if _, err := os.Stat(p); err == nil {
				return
			}
		}
		names = []string{"chromium", "google-chrome", "chrome"}
	case "windows":
		names = []string{"chrome", "chrome.exe"}
	default:
		names = []string{"headless-shell", "chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"}
	}
	for _, n := range names {
		if _, err := exec.LookPath(n); err == nil {
			return
		}
	}
	t.Skipf("no chrome binary found: looked for %v (and macOS .app bundles)", names)
}

// freePort grabs a free localhost TCP port and releases it. There is a small
// race before it is reclaimed; acceptable for a test harness.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// writePolicyFile writes a browserpolicy PolicyDoc JSON file for the gate.
func writePolicyFile(t *testing.T, mode string, hosts ...string) string {
	t.Helper()
	sites := make([]map[string]any, 0, len(hosts))
	for _, h := range hosts {
		sites = append(sites, map[string]any{"host": h})
	}
	doc, err := json.Marshal(map[string]any{"mode": mode, "sites": sites})
	require.NoError(t, err)
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	require.NoError(t, os.WriteFile(path, doc, 0o600))
	return path
}

// fixture is a running full-stack browser test environment: a local Chrome
// whose ONLY egress is the in-process gate's proxy, reached by session code
// THROUGH the in-process gate's authenticated DevTools endpoint.
type fixture struct {
	gateURL  string // gate DevTools listener; session connects here
	siteBase string // "http://site.test:<port>" — the allowlisted test site
	rt       spiceboxv1alpha1.ResolvedBrowserToolbox
}

// startFixture stands up: the httptest site, the in-process gate (proxy +
// devtools), and a local headless Chrome launched via chromedp.ExecAllocator
// with --proxy-server pointed at the gate proxy. The allowlisted site is
// reached by Chrome directly (host-resolver-rules + proxy-bypass), because the
// gate's guarded dialer refuses loopback by design and its dialer hook is
// package-private; a redirect to an off-policy host still traverses the gate
// proxy and is denied there, which is what the redirect test exercises.
func startFixture(t *testing.T, handler http.Handler) *fixture {
	t.Helper()
	requireLocalChrome(t)

	site := httptest.NewServer(handler)
	t.Cleanup(site.Close)
	sitePort := site.Listener.Addr().(*net.TCPAddr).Port
	siteBase := fmt.Sprintf("http://site.test:%d", sitePort)

	debugPort := freePort(t)
	policyPath := writePolicyFile(t, "allowlist", "site.test")

	g, err := browsergate.New(browsergate.Config{
		ProxyAddr:   "127.0.0.1:0",
		GateAddr:    "127.0.0.1:0",
		DevtoolsURL: fmt.Sprintf("http://127.0.0.1:%d", debugPort),
		Token:       chromeToken,
		PolicyPath:  policyPath,
		ExtraPorts:  []int{sitePort},
	})
	require.NoError(t, err)

	proxy := httptest.NewServer(g.ProxyHandler())
	t.Cleanup(proxy.Close)
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")

	gate := httptest.NewServer(g.GateHandler())
	t.Cleanup(gate.Close)

	// Launch Chrome: egress only through the gate proxy; the allowlisted test
	// site is mapped to loopback and bypassed so Chrome reaches it directly.
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.ProxyServer(proxyAddr),
		chromedp.Flag("remote-debugging-port", strconv.Itoa(debugPort)),
		// Chrome 111+ rejects remote-debugging connections whose Origin is not
		// allow-listed; the gate reaches Chrome over a rewritten host, so allow all.
		chromedp.Flag("remote-allow-origins", "*"),
		chromedp.Flag("host-resolver-rules", "MAP site.test 127.0.0.1"),
		chromedp.Flag("proxy-bypass-list", "site.test"),
		chromedp.NoSandbox,
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	t.Cleanup(allocCancel)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	t.Cleanup(browserCancel)
	// Boot on the browser context itself (not a timeout-scoped child): cancelling
	// a derived boot context can tear down the freshly created target.
	require.NoError(t, chromedp.Run(browserCtx), "boot local Chrome through the gate proxy")

	// Wait until Chrome's DevTools endpoint answers (the gate proxies it).
	waitForDevtools(t, debugPort)

	return &fixture{
		gateURL:  gate.URL,
		siteBase: siteBase,
		rt: spiceboxv1alpha1.ResolvedBrowserToolbox{
			Name: "demo",
			Ref:  "demo-browser",
			Spec: spiceboxv1alpha1.BrowserToolboxSpec{
				Access:      spiceboxv1alpha1.BrowserAccess{Mode: "allowlist"},
				Interaction: "readonly",
				State:       "ephemeral",
			},
			EffectiveSites: []spiceboxv1alpha1.BrowserEffectiveSite{{Host: "site.test", Source: "explicit"}},
		},
	}
}

func waitForDevtools(t *testing.T, debugPort int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", debugPort)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Chrome DevTools on :%d never became ready", debugPort)
}

// synthTools builds the toolbox's tools against the fixture and registers the
// shared session for teardown.
func (fx *fixture) synthTools(t *testing.T, acceptsImages bool) []agenttool.Tool {
	t.Helper()
	tools, err := Synthesize(fx.rt, fx.gateURL, chromeToken, acceptsImages, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, tools)
	if bt, ok := tools[0].(*browserTool); ok {
		t.Cleanup(func() { bt.sess.Close() })
	}
	return tools
}

func toolBy(t *testing.T, tools []agenttool.Tool, suffix string) agenttool.Tool {
	t.Helper()
	return toolBySuffix(t, tools, suffix)
}

// run executes a tool with flat args, requiring no Go-level error.
func run(t *testing.T, tl agenttool.Tool, args map[string]any) agenttool.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	res, err := tl.Execute(ctx, raw, &agenttool.SessionContext{})
	require.NoError(t, err)
	return res
}

func TestReadRendersOutlineWithRefs(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Fares</h1><a href="/x">Book a flight</a></body></html>`))
	}))
	tools := fx.synthTools(t, false)

	nav := run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"})
	require.False(t, nav.IsError, "navigate: %s", nav.Content)

	res := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, res.IsError, "read: %s", res.Content)
	assert.Contains(t, res.Content, `heading "Fares"`)
	assert.Contains(t, res.Content, `link "Book a flight"`)
	assert.Contains(t, res.Content, "[s", "links must carry a generation-scoped [s<gen>e<N>] ref")
}

func TestFollowOnlyLinks(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x" {
			_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Booked</h1></body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Fares</h1><a href="/x">Book a flight</a></body></html>`))
	}))
	tools := fx.synthTools(t, false)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)
	headingRef := refForRole(t, read.Content, "heading")
	linkRef := refForRole(t, read.Content, "link")

	follow := toolBy(t, tools, "follow")
	notLink := run(t, follow, map[string]any{"ref": headingRef})
	assert.True(t, notLink.IsError)
	assert.Contains(t, notLink.Content, "is not a link")

	ok := run(t, follow, map[string]any{"ref": linkRef})
	assert.False(t, ok.IsError, "follow link: %s", ok.Content)
	assert.Contains(t, ok.Content, "loaded")
	assert.Contains(t, ok.Content, "/x")
}

func TestStaleRefRefused(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Other</h1></body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Fares</h1><a href="/x">Book a flight</a></body></html>`))
	}))
	tools := fx.synthTools(t, false)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError)
	linkRef := refForRole(t, read.Content, "link")

	// Navigate elsewhere: the old snapshot's refs are now stale.
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/other"}).IsError)

	stale := run(t, toolBy(t, tools, "follow"), map[string]any{"ref": linkRef})
	assert.True(t, stale.IsError)
	assert.Contains(t, stale.Content, "is stale")
}

func TestRedirectToBlockedHostSurfacesDenial(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/r" {
			http.Redirect(w, r, "https://blocked.test/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Home</h1></body></html>`))
	}))
	tools := fx.synthTools(t, false)

	res := run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/r"})
	assert.True(t, res.IsError, "a redirect to an off-policy host must surface as an error, not a blank success")
	assert.Contains(t, res.Content, "is not allowed by browser toolbox")
	assert.Contains(t, res.Content, "blocked.test", "the real blocked redirect target should be named")
	assert.NotContains(t, res.Content, "loaded https://blocked.test")
}

func TestReadPaginates(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString(`<!doctype html><html><body><ul>`)
		for i := 0; i < 900; i++ {
			fmt.Fprintf(&b, `<li>list entry number %d of the very long enumeration</li>`, i)
		}
		b.WriteString(`</ul></body></html>`)
		_, _ = w.Write([]byte(b.String()))
	}))
	tools := fx.synthTools(t, false)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	readTool := toolBy(t, tools, "read")
	p1 := run(t, readTool, map[string]any{"page": 1})
	require.False(t, p1.IsError, "read p1: %s", p1.Content)
	assert.Contains(t, p1.Content, "page 1 of ")

	p2 := run(t, readTool, map[string]any{"page": 2})
	require.False(t, p2.IsError, "read p2: %s", p2.Content)
	assert.NotEqual(t, p1.Content, p2.Content, "page 2 must differ from page 1")

	oob := run(t, readTool, map[string]any{"page": 99})
	assert.Contains(t, oob.Content, "page 99 is out of range")
}

// refForRole extracts the first ref id (e.g. "s1e3") from an outline line whose
// rendered role matches role. Ref names are generation-scoped ("s<gen>e<N>"), so
// this matches the opening bracket, not an "e" prefix.
func refForRole(t *testing.T, outline, role string) string {
	t.Helper()
	for _, line := range strings.Split(outline, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		close := strings.Index(trimmed, "]")
		if close < 0 {
			continue
		}
		ref := trimmed[1:close] // "s<gen>e<N>"
		rest := strings.TrimSpace(trimmed[close+1:])
		if strings.HasPrefix(rest, role+" ") {
			return ref
		}
	}
	t.Fatalf("no [eN] %s line found in outline:\n%s", role, outline)
	return ""
}
