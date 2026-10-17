//go:build integration || e2e

// Package testbrowser spins up ONE chromedp/headless-shell container per test
// BINARY (shared, lazily, refcounted) and runs the browser gate
// (pkg/tools/browsergate) IN-PROCESS in front of it, so an e2e test can drive a
// real headless Chrome through exactly the authenticated-DevTools +
// policy-enforced-egress boundary production uses.
//
// Architecture lifted verbatim from test/testspicedb: dockertest pool, a pinned
// NUMERIC image tag (never :latest — a moving tag makes a green run
// unreproducible), a binary-scoped refcounted container (SharedChrome), reaping
// labels (labels.go, reap.go, AP_TEST_RUN_ID), res.Expire(1200) as the
// SIGKILL backstop, a 3-attempt start retry, and the EXACT skip-vs-fail policy:
// t.Skipf ONLY when the Docker daemon is unreachable; a container that will not
// start after retries is t.Fatalf — skipping that case is how a suite reports
// green having run nothing.
//
// # Topology (and why it is what it is)
//
// The e2e harness runs every FIRST-PARTY component in-process (the runner, and
// here the gate), so only Chrome is containerized — the newest numeric upstream
// headless-shell. The first-party images/browser image's flag set is covered by
// Task 8a's pod-builder unit tests; building it here would couple `mage
// test:e2e` to `docker build`. (Recorded deviation from the spec's §7
// "testbrowser starts the first-party image"; revisit in Phase C when real
// Chrome bot-check fidelity starts to matter.)
//
//	chromedp client ──(gate DevTools listener, 127.0.0.1:<gatePort>, token)──▶ gate ──▶ Chrome CDP (container 9222)
//	Chrome egress   ──(host.docker.internal:<proxyPort>)──▶ gate proxy (127.0.0.1:<proxyPort>) ──▶ ServeSite
//
// Two cross-boundary facts the fixture leans on, both verified on Docker
// Desktop for Mac and provided on Linux by the host-gateway ExtraHost:
//
//   - A container reaches a HOST service bound to 127.0.0.1 via
//     host.docker.internal, so the in-process gate's loopback proxy is Chrome's
//     reachable egress. Chrome's --proxy-server is baked at container start to
//     host.docker.internal:<proxyPort>; SharedChrome picks that port and
//     StartGate binds the gate's proxy to it (one shared port per binary — the
//     container is shared, so its proxy target is fixed, and gates in a binary
//     run sequentially).
//   - Containerized Chrome ECHOES the request Host in /json/version's
//     webSocketDebuggerUrl, so the gate's existing host-rewrite (devtools.go)
//     keeps chromedp on the authenticated gate rather than dialing Chrome
//     directly.
//
// The gate's production dialer refuses loopback/private by design
// (controlRejectPrivate), so it cannot reach a host-side httptest server on its
// own; StartGate installs a TEST-ONLY dialer (the browsergate.SetDialContextForTest
// seam the field's own comment anticipates) that maps host.docker.internal to
// the loopback ServeSite and DELEGATES every other host to the guarded dialer,
// so off-site denials still fire.
package testbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/browsergate"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

const (
	// chromeImage / chromeTag pin the upstream headless Chrome. NUMERIC tag,
	// never :latest/:stable — a moving tag makes a green run unreproducible.
	// This was the newest numeric chromedp/headless-shell tag when the fixture
	// landed (== :latest/:stable on 2026-08-11); bump it deliberately.
	chromeImage = "chromedp/headless-shell"
	chromeTag   = "151.0.7922.109"

	// hostDockerInternal is the name a container resolves to reach the test
	// host. ServeSite returns urlForChrome / hostForPolicy in terms of it, and
	// StartGate's test dialer maps it back to the loopback ServeSite.
	hostDockerInternal = "host.docker.internal"

	// chromeDebugContainerPort is the port the headless-shell image publishes
	// its DevTools on (its entrypoint socat-forwards it to Chrome's internal
	// debug port).
	chromeDebugContainerPort = "9222/tcp"

	// idleGrace is how long the shared container outlives its last user before
	// it is purged — see test/testspicedb/shared.go for the full rationale
	// (bridges the sub-millisecond gaps between serial tests without restarting
	// a container per test).
	idleGrace = 5 * time.Second

	// containerStartAttempts bounds retries of the run+probe cycle. A genuinely
	// broken image must still fail rather than spin.
	containerStartAttempts = 3

	// expireSeconds is the dockertest Expire backstop: the daemon force-stops
	// the container this long after creation even if the process is SIGKILLed
	// without running cleanups. A ceiling on a shared container's life, not a
	// grace period (see testspicedb).
	expireSeconds = 1200
)

var (
	sharedMu        sync.Mutex
	sharedCDP       string // "http://127.0.0.1:<hostPort>"
	sharedProxyPort int    // host port Chrome's --proxy-server targets; the gate binds it
	sharedRes       *dockertest.Resource
	sharedPool      *dockertest.Pool
	sharedUsers     int
	sharedIdle      *time.Timer
	sharedFail      error
	sharedTried     bool
)

// SharedChrome starts ONE chromedp/headless-shell container for the whole test
// BINARY (lazily, on first call) and returns its CDP base URL
// ("http://127.0.0.1:<port>"). Chrome's only egress is baked to the in-process
// gate's proxy (host.docker.internal:<proxyPort>); StartGate binds that port.
//
// Lifetime is reference-counted: every caller's t.Cleanup releases, and the
// container is purged idleGrace after the LAST release, so the fixture is
// self-cleaning and needs no TestMain. A package that DOES define TestMain
// should call StopShared after m.Run() to purge immediately.
//
// If Docker is unreachable the calling test is SKIPPED. If Docker is reachable
// and the container still fails, the test FAILS — skipping that is how a suite
// reports green having run nothing.
func SharedChrome(t *testing.T) string {
	t.Helper()

	sharedMu.Lock()
	defer sharedMu.Unlock()

	if sharedIdle != nil {
		sharedIdle.Stop()
		sharedIdle = nil
	}

	if !sharedTried {
		sharedTried = true
		cdp, proxyPort, res, pool, err := startContainer()
		sharedCDP, sharedProxyPort, sharedRes, sharedPool, sharedFail = cdp, proxyPort, res, pool, err
		if err != nil {
			t.Logf("testbrowser: could not start shared headless-shell container: %v", err)
		}
	}
	if errors.Is(sharedFail, ErrDockerUnavailable) {
		t.Skipf("skipping: %v", sharedFail)
	}
	if sharedFail != nil || sharedCDP == "" {
		t.Fatalf("testbrowser: docker is reachable but the shared headless-shell container would not start: %v", sharedFail)
	}

	sharedUsers++
	t.Cleanup(releaseShared)
	return sharedCDP
}

// sharedProxyAddr returns the host:port Chrome's --proxy-server was baked to,
// which is the address StartGate must bind the gate's forward proxy to. Fatals
// if SharedChrome has not run — StartGate is always paired with it.
func sharedProxyAddr(t *testing.T) string {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedProxyPort == 0 {
		t.Fatal("testbrowser: StartGate called before SharedChrome (no proxy port reserved)")
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(sharedProxyPort))
}

func releaseShared() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	sharedUsers--
	if sharedUsers > 0 {
		return
	}
	sharedIdle = time.AfterFunc(idleGrace, purgeIfIdle)
}

func purgeIfIdle() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedUsers > 0 {
		return
	}
	purgeSharedLocked()
}

// StopShared purges the package's shared container immediately. Safe when none
// was started. Call from TestMain after m.Run().
func StopShared() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedIdle != nil {
		sharedIdle.Stop()
		sharedIdle = nil
	}
	purgeSharedLocked()
}

func purgeSharedLocked() {
	if sharedRes == nil || sharedPool == nil {
		return
	}
	if err := sharedPool.Purge(sharedRes); err != nil {
		// Reachable from TestMain and from a timer goroutine, both outside any
		// *testing.T — stderr is the only surface. Never silent (AGENTS.md).
		fmt.Fprintf(os.Stderr, "testbrowser: purge shared container: %v\n", err)
	}
	sharedRes, sharedPool, sharedCDP, sharedProxyPort = nil, nil, "", 0
	sharedTried, sharedFail = false, nil
}

// startContainer launches the headless-shell container, retrying transient
// startup failures. No Docker is terminal (retrying cannot conjure a daemon).
func startContainer() (cdp string, proxyPort int, res *dockertest.Resource, pool *dockertest.Pool, err error) {
	var lastErr error
	for attempt := 1; attempt <= containerStartAttempts; attempt++ {
		cdp, proxyPort, res, pool, err = startContainerOnce()
		if err == nil {
			return cdp, proxyPort, res, pool, nil
		}
		if errors.Is(err, ErrDockerUnavailable) {
			return "", 0, nil, nil, err
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "testbrowser: container start attempt %d/%d failed: %v\n",
			attempt, containerStartAttempts, err)
	}
	return "", 0, nil, nil, fmt.Errorf("after %d attempts: %w", containerStartAttempts, lastErr)
}

func startContainerOnce() (cdp string, proxyPort int, res *dockertest.Resource, pool *dockertest.Pool, err error) {
	pool, err = dockertest.NewPool("")
	if err != nil {
		// The ONLY skippable outcome: no reachable Docker daemon.
		return "", 0, nil, nil, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	pool.MaxWait = 60 * time.Second

	// Reserve the host port Chrome's egress proxy (the in-process gate, started
	// later by StartGate) will listen on, and bake it into Chrome's
	// --proxy-server now — the container is shared, so its proxy target is fixed
	// for the binary.
	proxyPort, err = freeLoopbackPort()
	if err != nil {
		return "", 0, nil, nil, fmt.Errorf("reserve proxy port: %w", err)
	}

	res, err = pool.RunWithOptions(&dockertest.RunOptions{
		Repository:   chromeImage,
		Tag:          chromeTag,
		ExposedPorts: []string{chromeDebugContainerPort},
		// host-gateway makes host.docker.internal resolve from inside the
		// container on Linux; harmless on macOS where Docker Desktop already
		// provides it. Chrome reaches the host gate proxy, and ServeSite,
		// through this name.
		ExtraHosts: []string{hostDockerInternal + ":host-gateway"},
		// Appended to the image entrypoint's own headless-shell invocation:
		//   - remote-allow-origins=*: Chrome 111+ rejects a DevTools ws whose
		//     Origin is not allow-listed, and the gate reaches Chrome over a
		//     rewritten host (mirrors pkg/agent/tool/browser's chrome_test.go).
		//   - proxy-server: force every page fetch through the gate's proxy.
		Cmd: []string{
			"--remote-allow-origins=*",
			"--proxy-server=" + net.JoinHostPort(hostDockerInternal, strconv.Itoa(proxyPort)),
		},
		Labels: map[string]string{
			LabelFixture:    LabelFixtureValue,
			LabelOwnerPID:   strconv.Itoa(os.Getpid()),
			LabelOwnerStart: SelfStartToken(),
			LabelRunID:      os.Getenv(EnvRunID),
		},
	})
	if err != nil {
		return "", 0, nil, nil, fmt.Errorf("run headless-shell container: %w", err)
	}

	// Having paid for a docker round trip, sweep up any container this fixture
	// started for a process that is now gone. See reapAbandoned.
	reapAbandoned(pool)

	// Hard backstop for a test process that dies without running cleanups.
	_ = res.Expire(expireSeconds)

	hostPort := res.GetPort(chromeDebugContainerPort)
	cdp = "http://127.0.0.1:" + hostPort

	// Wait for DevTools to answer. Only a REAL 200 from /json/version proves the
	// service is serving; a connection error means the port is not bound yet.
	if retryErr := pool.Retry(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, cdp+"/json/version", nil)
		if reqErr != nil {
			return reqErr
		}
		resp, getErr := http.DefaultClient.Do(req)
		if getErr != nil {
			return getErr
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("devtools /json/version: status %d", resp.StatusCode)
		}
		return nil
	}); retryErr != nil {
		_ = pool.Purge(res) // failed start — purge immediately
		return "", 0, nil, nil, fmt.Errorf("headless-shell container did not become ready: %w", retryErr)
	}

	return cdp, proxyPort, res, pool, nil
}

// reapAbandoned removes containers THIS fixture started whose creating process
// no longer exists. Best-effort: failures are reported, never fatal.
func reapAbandoned(pool *dockertest.Pool) {
	res, err := ReapWith(pool.Client, os.Getpid(), "", LiveOwnerChecker())
	if err != nil {
		fmt.Fprintf(os.Stderr, "testbrowser: sweep abandoned fixture containers: %v\n", err)
		return
	}
	for _, d := range res.Kept {
		if d.Err != nil {
			fmt.Fprintf(os.Stderr, "testbrowser: reap abandoned container %s (%s): %v\n",
				shortID(d.ID), d.Reason, d.Err)
		}
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// StartGate runs the browser gate IN-PROCESS on 127.0.0.1 listeners in front of
// cdpURL and returns the gate DevTools URL the runner-side tools dial
// ("http://127.0.0.1:<gatePort>").
//
// policy is written to a temp file (the gate reloads it on mtime change, so a
// later rewrite would take effect). token authenticates the DevTools reverse
// proxy. extraPorts admits CONNECT/HTTP to non-80/443 ports — pass ServeSite's
// port so the gate proxy will forward to it.
//
// The gate's forward proxy binds the host port Chrome's --proxy-server was
// baked to (sharedProxyAddr), so Chrome's egress reaches it. A TEST-ONLY dialer
// maps host.docker.internal to the loopback ServeSite and delegates every other
// host to the guarded dialer — see the package doc.
func StartGate(t *testing.T, policy browserpolicy.PolicyDoc, token, cdpURL string, extraPorts ...int) (gateURL string) {
	t.Helper()

	policyPath := writePolicy(t, policy)
	proxyAddr := sharedProxyAddr(t)

	g, err := browsergate.New(browsergate.Config{
		ProxyAddr:   proxyAddr,
		GateAddr:    "127.0.0.1:0",
		DevtoolsURL: cdpURL,
		Token:       token,
		PolicyPath:  policyPath,
		ExtraPorts:  extraPorts,
	})
	require.NoError(t, err, "browsergate.New")

	// The production guard refuses loopback, so the gate cannot reach a
	// host-side httptest server on its own. Map host.docker.internal (what
	// Chrome navigates to, and what the gate therefore receives) to the loopback
	// ServeSite; delegate everything else to the guarded dialer so off-site
	// private-address denials still fire.
	var prev func(ctx context.Context, network, addr string) (net.Conn, error)
	prev = g.SetDialContextForTest(func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, splitErr := net.SplitHostPort(addr)
		if splitErr == nil && host == hostDockerInternal {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		}
		return prev(ctx, network, addr)
	})

	// Bind our own listeners so we can read the ephemeral gate port and control
	// lifecycle; mirrors what browsergate.Gate.Run does, minus its blocking
	// select (the test owns teardown via t.Cleanup).
	proxyLn, err := net.Listen("tcp", proxyAddr)
	require.NoError(t, err, "bind gate proxy listener on %s", proxyAddr)
	gateLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = proxyLn.Close()
		require.NoError(t, err, "bind gate devtools listener")
	}

	proxySrv := &http.Server{Handler: g.ProxyHandler()}
	gateSrv := &http.Server{Handler: g.GateHandler()}
	safehttp.HardenServer(proxySrv)
	safehttp.HardenServer(gateSrv)
	go func() { _ = proxySrv.Serve(proxyLn) }()
	go func() { _ = gateSrv.Serve(gateLn) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxySrv.Shutdown(ctx)
		_ = gateSrv.Shutdown(ctx)
	})

	gatePort := gateLn.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("http://127.0.0.1:%d", gatePort)
}

// writePolicy marshals policy to a fresh temp file and returns its path.
func writePolicy(t *testing.T, policy browserpolicy.PolicyDoc) string {
	t.Helper()
	raw, err := json.Marshal(policy)
	require.NoError(t, err, "marshal browser policy")
	path := filepath.Join(t.TempDir(), "policy.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600), "write browser policy")
	return path
}

// ServeSite starts an httptest server serving the given path→HTML map and
// returns (urlForChrome, hostForPolicy, port). urlForChrome uses
// host.docker.internal so the containerized Chrome can reach the test host;
// hostForPolicy is the host a StartGate allowlist must admit; port is what to
// pass as StartGate's extraPorts entry.
//
// Chrome reaches the site THROUGH the gate proxy (its baked --proxy-server), and
// the gate's test dialer maps host.docker.internal back to this loopback
// server — so a page load exercises the gate's egress path end to end.
func ServeSite(t *testing.T, pages map[string]string) (urlForChrome, hostForPolicy string, port int) {
	t.Helper()
	mux := http.NewServeMux()
	for path, html := range pages {
		body := html
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(body))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	addr := srv.Listener.Addr().(*net.TCPAddr)
	return fmt.Sprintf("http://%s:%d/", hostDockerInternal, addr.Port), hostDockerInternal, addr.Port
}

// freeLoopbackPort grabs a free 127.0.0.1 TCP port and releases it. There is a
// small race before it is reclaimed; acceptable for a test harness (same shape
// as pkg/agent/tool/browser's chrome_test.go freePort).
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}
