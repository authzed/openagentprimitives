//go:build e2e

package testbrowser

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

// TestSmokeChromeThroughGateLoadsPage is the fixture's own end-to-end test: a
// chromedp client drives the shared headless-Chrome container THROUGH the
// in-process gate — the DevTools control plane over the gate's authenticated
// reverse proxy (token in the query), and Chrome's page fetch through the gate's
// forward proxy — and reads the loaded page's title. It proves SharedChrome,
// StartGate and ServeSite compose into the exact boundary production uses.
//
// Per the fixture's skip-vs-fail policy, this is SKIPPED only when the Docker
// daemon is unreachable (SharedChrome's t.Skipf); a container that will not
// start is a hard failure.
func TestSmokeChromeThroughGateLoadsPage(t *testing.T) {
	const token = "testbrowser-smoke-secret-token"

	cdp := SharedChrome(t)

	urlForChrome, hostForPolicy, port := ServeSite(t, map[string]string{
		"/": `<!doctype html><html><head><title>Browser Toolbox Smoke</title></head>` +
			`<body><h1>hello from the smoke site</h1></body></html>`,
	})

	gateURL := StartGate(t, browserpolicy.PolicyDoc{
		Mode:  browserpolicy.ModeAllowlist,
		Sites: []browserpolicy.Site{{Host: hostForPolicy}},
	}, token, cdp, port)

	// Connect THROUGH the gate's DevTools listener, presenting the token in the
	// query (the gate's /json rewrite carries it onto the ws URL so chromedp
	// stays on the authenticated gate). Mirrors pkg/agent/tool/browser's
	// newSession allocator.
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), gateURL+"/json/version?t="+token)
	t.Cleanup(cancelAlloc)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelBrowser)

	runCtx, cancelRun := context.WithTimeout(browserCtx, 40*time.Second)
	t.Cleanup(cancelRun)

	var title string
	require.NoError(t, chromedp.Run(runCtx,
		chromedp.Navigate(urlForChrome), // egress forced through the gate's forward proxy
		chromedp.Title(&title),
	), "drive Chrome through the gate")

	require.Contains(t, title, "Browser Toolbox Smoke",
		"the gate-proxied page load must return the served title")
}
