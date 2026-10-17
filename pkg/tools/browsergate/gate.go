// Package browsergate is the security boundary for the browser toolbox's
// per-session Chrome pod. It runs as the pod's second container and enforces
// two seams:
//
//   - A policy-enforcing forward proxy (CONNECT + plain HTTP) bound to loopback,
//     which is Chrome's ONLY egress (--proxy-server). Every destination host is
//     checked against the per-session browserpolicy before a byte leaves the
//     pod, and the dialer additionally refuses any host that RESOLVES to a
//     private/metadata/loopback address — closing the DNS-rebinding and
//     "localhost" SSRF gaps the hostname check alone cannot.
//   - An authenticated reverse proxy for Chrome's DevTools endpoint, bound to
//     the pod's gate port, which the session runner is the sole client of.
//     Every request but /healthz must present the per-session token before
//     anything reaches Chrome.
//
// The package is stdlib-only (plus the pure browserpolicy and safehttp helpers)
// so the gate stays a small, auditable boundary.
package browsergate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

const (
	// DefaultProxyAddr is Chrome's --proxy-server target. It is loopback-only,
	// never the pod IP: nothing but Chrome in the same pod may reach it.
	DefaultProxyAddr = "127.0.0.1:3128"
	// DefaultGateAddr is the runner-facing listener (DevTools proxy + /healthz).
	DefaultGateAddr = ":9223"
	// DefaultGatePort is the port half of DefaultGateAddr.
	DefaultGatePort = 9223
	// DefaultDevtoolsURL is Chrome's remote-debugging endpoint inside the pod.
	DefaultDevtoolsURL = "http://127.0.0.1:9222"
)

const (
	// dialTimeout bounds a single upstream dial (CONNECT and plain-HTTP alike).
	dialTimeout = 10 * time.Second
	// shutdownTimeout bounds each listener's graceful-shutdown window.
	shutdownTimeout = 5 * time.Second
	// devtoolsTimeout bounds the gate's own fetch of /json/* from Chrome.
	devtoolsTimeout = 15 * time.Second
)

// Config configures a Gate. Zero values for the addresses and DevtoolsURL fall
// back to the Default* constants; Token and a readable PolicyPath are required
// (New errors otherwise).
type Config struct {
	ProxyAddr   string
	GateAddr    string
	DevtoolsURL string // e.g. "http://127.0.0.1:9222"
	Token       string // per-session browser token; required
	PolicyPath  string // JSON PolicyDoc; reloaded when its mtime changes
	ExtraPorts  []int  // TEST-ONLY additional CONNECT/HTTP ports; empty in production
}

// Gate is the running browser gate: a forward proxy (ProxyHandler) and an
// authenticated DevTools reverse proxy (GateHandler).
type Gate struct {
	cfg      Config
	policy   *policyStore
	devtools *url.URL
	ports    map[int]bool
	logger   *slog.Logger

	// dialContext dials an upstream for both the CONNECT path and the plain-HTTP
	// transport. It defaults to a guarded net.Dialer whose Control hook refuses
	// any resolved private/metadata/loopback address. It is a field (not a
	// method) so in-process tests can substitute a dialer that reaches an
	// httptest backend on loopback without tripping that guard.
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	transport     *http.Transport        // plain-HTTP forward transport (uses dialContext)
	forwardProxy  *httputil.ReverseProxy // plain-HTTP forward proxy
	devtoolsProxy *httputil.ReverseProxy // DevTools reverse proxy (loopback; not guarded)
	devtoolsCli   *http.Client           // fetches /json/* for rewriting
}

// New validates cfg and builds a Gate. It fails closed: an empty Token, an
// unparseable DevtoolsURL, or an unreadable/invalid initial policy are errors,
// so a gate never starts in a state that could admit traffic it should refuse.
func New(cfg Config) (*Gate, error) {
	if cfg.Token == "" {
		return nil, errors.New("browsergate: Token is required")
	}
	if cfg.ProxyAddr == "" {
		cfg.ProxyAddr = DefaultProxyAddr
	}
	if cfg.GateAddr == "" {
		cfg.GateAddr = DefaultGateAddr
	}
	if cfg.DevtoolsURL == "" {
		cfg.DevtoolsURL = DefaultDevtoolsURL
	}
	du, err := url.Parse(cfg.DevtoolsURL)
	if err != nil || du.Scheme == "" || du.Host == "" {
		return nil, fmt.Errorf("browsergate: invalid DevtoolsURL %q", cfg.DevtoolsURL)
	}

	logger := slog.Default()
	ps, err := newPolicyStore(cfg.PolicyPath, logger)
	if err != nil {
		return nil, err
	}

	g := &Gate{
		cfg:      cfg,
		policy:   ps,
		devtools: du,
		ports:    portSet(cfg.ExtraPorts),
		logger:   logger,
	}

	// The guarded dialer: resolved private/metadata/loopback addresses are
	// refused at dial time, before connect. Shared by CONNECT and the transport.
	baseDialer := &net.Dialer{Timeout: dialTimeout, Control: g.controlRejectPrivate}
	g.dialContext = baseDialer.DialContext

	// The transport indirects through g.dialContext so a test override of that
	// field takes effect for plain-HTTP forwarding too.
	g.transport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return g.dialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     false, // a forward proxy speaks HTTP/1.1 to arbitrary origins
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// forwardProxy forwards an already-absolute request URL (the Director is a
	// no-op) through the guarded transport. Its ErrorHandler turns a dial-time
	// private-address refusal into the policy 403, and every other failure into
	// a logged 502.
	g.forwardProxy = &httputil.ReverseProxy{
		Director:  func(*http.Request) {},
		Transport: g.transport,
		ErrorLog:  log.New(io.Discard, "", 0), // real logging happens in ErrorHandler
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			var bae *blockedAddrError
			if errors.As(err, &bae) {
				writeDeny(w, req.URL.Hostname(), rulePrivateOrMetadata)
				return
			}
			g.logger.Info("browsergate: plain-HTTP forward error", "host", req.URL.Hostname(), "err", err.Error())
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
	}

	// devtoolsProxy targets Chrome's loopback DevTools endpoint. It deliberately
	// does NOT use the guarded transport: its target is loopback, which that
	// guard exists to refuse. Chrome's DevTools rejects a non-IP/localhost Host
	// header (a DNS-rebinding defense), so the Director rewrites Host too.
	g.devtoolsProxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = g.devtools.Scheme
			req.URL.Host = g.devtools.Host
			req.Host = g.devtools.Host
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			g.logger.Info("browsergate: devtools proxy error", "err", err.Error())
			http.Error(w, "devtools upstream error", http.StatusBadGateway)
		},
	}

	g.devtoolsCli = &http.Client{Timeout: devtoolsTimeout}

	return g, nil
}

// SetDialContextForTest replaces the gate's upstream dialer and returns the
// one it displaced. TEST-ONLY seam, in the same spirit as Config.ExtraPorts:
// an in-process test substitutes a dialer that can reach a loopback/httptest
// backend the production guard (controlRejectPrivate) refuses by design — see
// the dialContext field's own comment, which anticipates exactly this.
//
// Returning the previous dialer lets an override DELEGATE every host but the
// one(s) it redirects, so the private/metadata-address guard still fires for
// off-site hosts and the security semantics a test is not deliberately
// bypassing stay intact. Production never calls this.
//
// It panics outside a test binary, mirroring pkg/x/browser.SetOpenerForTest:
// a panic is checkable where a doc comment is not, and that is what keeps
// this seam from becoming a way for production code to bypass the SSRF
// guard. Call it before Run; it is not safe for concurrent use with a
// running gate (g.dialContext is read without a lock by in-flight CONNECT/
// proxy handling, so swapping it while the gate is serving traffic races).
func (g *Gate) SetDialContextForTest(d func(ctx context.Context, network, addr string) (net.Conn, error)) (prev func(ctx context.Context, network, addr string) (net.Conn, error)) {
	if !testing.Testing() {
		panic("browsergate.SetDialContextForTest called outside a test binary")
	}
	prev = g.dialContext
	g.dialContext = d
	return prev
}

// portSet builds the set of CONNECT/HTTP ports the gate admits: 80 and 443,
// plus any TEST-ONLY extras.
func portSet(extra []int) map[int]bool {
	ports := map[int]bool{80: true, 443: true}
	for _, p := range extra {
		ports[p] = true
	}
	return ports
}

// Run serves both listeners until ctx is cancelled, then shuts them down
// gracefully. A listener that fails to bind returns its error (and the other
// listener is shut down).
func (g *Gate) Run(ctx context.Context) error {
	proxySrv := &http.Server{Addr: g.cfg.ProxyAddr, Handler: g.ProxyHandler()}
	gateSrv := &http.Server{Addr: g.cfg.GateAddr, Handler: g.GateHandler()}
	safehttp.HardenServer(proxySrv)
	safehttp.HardenServer(gateSrv)

	errCh := make(chan error, 2)
	serve := func(name string, s *http.Server) {
		err := s.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("browsergate: %s listener: %w", name, err)
			return
		}
		errCh <- nil
	}
	go serve("proxy", proxySrv)
	go serve("gate", gateSrv)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}

	shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := proxySrv.Shutdown(shCtx); err != nil {
		g.logger.Info("browsergate: proxy listener shutdown", "err", err.Error())
	}
	if err := gateSrv.Shutdown(shCtx); err != nil {
		g.logger.Info("browsergate: gate listener shutdown", "err", err.Error())
	}
	return runErr
}

// ProxyHandler is Chrome's egress proxy: CONNECT tunnels and plain-HTTP
// forwarding, both policy-enforced.
func (g *Gate) ProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			g.handleConnect(w, r)
			return
		}
		g.handlePlainHTTP(w, r)
	})
}

// handleConnect enforces the port and host policy, then (on success) hijacks
// the client connection and bidirectionally copies to the upstream. Every deny
// is written before the hijack, as a plain 403.
func (g *Gate) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "bad CONNECT target", http.StatusBadRequest)
		return
	}
	if !g.portAllowed(port) {
		writePortDeny(w, port)
		return
	}
	if dec := g.decide(host); !dec.Allowed {
		writeDeny(w, host, dec.Rule)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	upstream, err := g.dialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		var bae *blockedAddrError
		if errors.As(err, &bae) {
			// The resolved address was private/metadata/loopback (e.g. a host
			// that is policy-allowed by name but resolves to loopback).
			writeDeny(w, host, rulePrivateOrMetadata)
			return
		}
		g.logger.Info("browsergate: CONNECT dial failed", "host", host, "port", port, "err", err.Error())
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		g.logger.Info("browsergate: CONNECT hijack failed", "host", host, "err", err.Error())
		return
	}
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		_ = upstream.Close()
		_ = clientConn.Close()
		return
	}
	tunnel(clientConn, upstream)
}

// handlePlainHTTP enforces the same port and host policy for a non-CONNECT
// forward-proxy request, then forwards it through the guarded transport.
func (g *Gate) handlePlainHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	port := r.URL.Port()
	if port == "" {
		port = "80"
	}
	if !g.portAllowed(port) {
		writePortDeny(w, port)
		return
	}
	if dec := g.decide(host); !dec.Allowed {
		writeDeny(w, host, dec.Rule)
		return
	}
	g.forwardProxy.ServeHTTP(w, r)
}

// decide evaluates host against the current (possibly just-reloaded) policy.
func (g *Gate) decide(host string) browserpolicy.Decision {
	doc := g.policy.current()
	return browserpolicy.Decide(doc.Mode, doc.Sites, host)
}

// portAllowed reports whether port (a decimal string) is in the admitted set.
func (g *Gate) portAllowed(port string) bool {
	n, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	return g.ports[n]
}

// controlRejectPrivate is the net.Dialer.Control hook: address is the RESOLVED
// remote address, so refusing private/metadata/loopback/unspecified here closes
// the gap where a policy-allowed hostname resolves to an internal address.
func (g *Gate) controlRejectPrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if browserpolicy.IsPrivateOrMetadataHost(host) {
		return &blockedAddrError{addr: host}
	}
	return nil
}

// blockedAddrError is returned by the dial Control hook when a resolved address
// is private/metadata/loopback. The handlers detect it with errors.As (it is
// wrapped in a *net.OpError by the dialer) to render the policy 403 rather than
// a generic dial failure.
type blockedAddrError struct{ addr string }

func (e *blockedAddrError) Error() string {
	return "browsergate: refusing to dial private/metadata address " + e.addr
}

// rulePrivateOrMetadata is the deny-rule string browserpolicy uses for a
// private/metadata/loopback address; reused so the dial-time refusal renders the
// same body as the pre-dial decision.
const rulePrivateOrMetadata = "private or metadata address"

// writeDeny writes the verbatim policy-deny body as a 403. The body matches the
// contract exactly (no trailing newline) so downstream exact-match assertions
// hold.
func writeDeny(w http.ResponseWriter, host, rule string) {
	msg := fmt.Sprintf("host %s is not allowed by browser toolbox policy (rule: %s)", host, rule)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(msg))
}

// writePortDeny writes a 403 for a CONNECT/HTTP port outside the admitted set.
func writePortDeny(w http.ResponseWriter, port string) {
	msg := fmt.Sprintf("port %s is not allowed by browser toolbox policy", port)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(msg))
}

// tunnel copies bytes in both directions between the hijacked client connection
// and the upstream, closing both when either side ends so neither copy goroutine
// can leak.
func tunnel(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		_ = src.Close()
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
