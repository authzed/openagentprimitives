// Package browser synthesizes the runner-side observation tools for one
// resolved BrowserToolbox. The tools drive a per-session Chrome over chromedp,
// reaching it ONLY through the browser gate (pkg/tools/browsergate): the
// DevTools control plane is reached over the gate's authenticated reverse
// proxy, and Chrome's own egress is policy-enforced by the gate's forward
// proxy. The tools additionally pre-check every navigation host against the
// toolbox's browserpolicy BEFORE handing a URL to Chrome, so an out-of-policy
// host is refused without a round-trip.
//
// All tools of one toolbox share a single Session (one Chrome connection) and a
// single Origin, so toolguard's origin-level circuit breaker treats them as one
// upstream — mirroring pkg/agent/tool/sidecartoolbox's origin wrapper.
package browser

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// connectRetryDelay is the pause before the single reconnect attempt. The pod
// is Ready before the first tool call, but a lazy connect with one retry makes
// a wake race harmless.
const connectRetryDelay = 500 * time.Millisecond

// Session owns the lazily-established chromedp connection to one per-session
// browser, reached through the gate. It is shared by every tool of a toolbox
// and is safe for concurrent use: all browser operations serialize under mu, so
// a single Chrome tab is never driven by two calls at once and the per-snapshot
// ref map is always rebuilt atomically.
type Session struct {
	gateURL string
	token   string

	// newAllocator creates the chromedp allocator for parent. Overridable only
	// for tests; production always dials the gate's /json/version with the token.
	newAllocator func(parent context.Context) (context.Context, context.CancelFunc)

	mu        sync.Mutex
	rootCtx   context.Context
	rootStop  context.CancelFunc
	allocStop context.CancelFunc
	browseCtx context.Context
	browseCl  context.CancelFunc
	connected bool

	// refs is the latest snapshot's ref map, rebuilt atomically by read/find.
	// A ref not present here is stale (the page changed since it was minted).
	refs map[string]refEntry

	// netMu guards the network-event capture below. It is SEPARATE from mu
	// because the chromedp event listener fires on its own goroutine while mu is
	// held for the in-flight do(); sharing mu would deadlock.
	netMu      sync.Mutex
	reqURLs    map[network.RequestID]string // per-navigation request URLs
	blockedURL string                       // the last URL a gate/proxy denial failed on
}

// refEntry is what a per-snapshot "e<N>" ref resolves to: the DOM backend node
// and the rendered role, so follow can both locate the element and refuse a
// non-link without a second round-trip.
type refEntry struct {
	backendNodeID int64
	role          string
}

// newSession builds a Session for a gate URL + per-session token. gateURL is
// "http://<BrowserPodIP>:<GatePort>"; the connection is not made until the
// first tool call.
func newSession(gateURL, token string) *Session {
	s := &Session{gateURL: gateURL, token: token, refs: map[string]refEntry{}}
	s.newAllocator = func(parent context.Context) (context.Context, context.CancelFunc) {
		// The gate rewrites /json/version's webSocketDebuggerUrl back to itself
		// WITH the token, so chromedp stays on the authenticated gate rather than
		// dialing Chrome directly. chromedp's default modifyURL preserves the
		// query, so the token reaches the gate's auth check.
		return chromedp.NewRemoteAllocator(parent, s.gateURL+"/json/version?t="+s.token)
	}
	return s
}

// ensureConnected establishes the chromedp connection on first use, with one
// retry. It must be called with s.mu held. Every failure is returned with
// context; nothing is swallowed.
func (s *Session) ensureConnected() error {
	if s.connected {
		return nil
	}
	if s.rootCtx == nil {
		// Parent on a session-scoped context, NOT a single call's context: the
		// browser connection outlives any one Execute. Close tears it down.
		s.rootCtx, s.rootStop = context.WithCancel(context.Background())
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		allocCtx, allocStop := s.newAllocator(s.rootCtx)
		browseCtx, browseCl := chromedp.NewContext(allocCtx)
		// Boot on browseCtx itself: a derived, immediately-cancelled context
		// would tear down the target chromedp just created. chromedp's own dial
		// and modifyURL timeouts bound this; the pod is Ready before first use.
		// Enabling Network lets the listener name the host a redirect was denied
		// at, rather than the chrome-error pseudo-host Location reports.
		if err := chromedp.Run(browseCtx, network.Enable()); err == nil {
			s.allocStop = allocStop
			s.browseCtx = browseCtx
			s.browseCl = browseCl
			s.connected = true
			s.reqURLs = map[network.RequestID]string{}
			chromedp.ListenTarget(browseCtx, s.onNetworkEvent)
			return nil
		} else {
			browseCl()
			allocStop()
			lastErr = err
		}
		if attempt == 0 {
			time.Sleep(connectRetryDelay)
		}
	}
	return fmt.Errorf("connecting to browser gate %s: %w", s.gateURL, lastErr)
}

// do runs fn against the browser tab, honoring the caller's ctx for
// cancellation and applying a per-call timeout. It serializes all browser
// operations and connects lazily. fn receives a context already bound to the
// chromedp browser context, so it can invoke chromedp.Run / cdproto actions.
func (s *Session) do(ctx context.Context, timeout time.Duration, fn func(ctx context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnected(); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(s.browseCtx, timeout)
	defer cancel()
	// Propagate the caller's cancellation (an interrupt or the LLM call ending)
	// into the chromedp run, which is parented on the long-lived browser ctx.
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	return fn(runCtx)
}

// onNetworkEvent records request URLs and, on a gate/proxy-denied load failure,
// the URL it failed at — so navigate can name the real blocked host (e.g. a
// redirect target) instead of Chrome's chrome-error pseudo-host. Runs on
// chromedp's event goroutine; guarded by netMu, never mu.
func (s *Session) onNetworkEvent(ev any) {
	switch e := ev.(type) {
	case *network.EventRequestWillBeSent:
		if e.Request == nil {
			return
		}
		s.netMu.Lock()
		if len(s.reqURLs) < 1024 { // bounded; reset per navigation keeps it small
			s.reqURLs[e.RequestID] = e.Request.URL
		}
		s.netMu.Unlock()
	case *network.EventLoadingFailed:
		if !isProxyDenial(e.ErrorText) && e.BlockedReason == "" {
			return
		}
		s.netMu.Lock()
		if u, ok := s.reqURLs[e.RequestID]; ok {
			s.blockedURL = u
		}
		s.netMu.Unlock()
	}
}

// resetBlocked clears the per-navigation capture before a navigation starts.
func (s *Session) resetBlocked() {
	s.netMu.Lock()
	s.blockedURL = ""
	s.reqURLs = map[network.RequestID]string{}
	s.netMu.Unlock()
}

// blockedHost returns the host of the last gate/proxy-denied URL, waiting
// briefly for the async LoadingFailed event to arrive, or "" if none.
func (s *Session) blockedHost() string {
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		s.netMu.Lock()
		u := s.blockedURL
		s.netMu.Unlock()
		if u != "" {
			return hostOf(u)
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// setRefs atomically replaces the latest snapshot's ref map.
func (s *Session) setRefs(refs map[string]refEntry) {
	s.refs = refs
}

// clearRefs drops the ref map; any ref presented afterward is stale. Called on
// every navigation (navigate / successful follow / back), because the page the
// refs described is gone.
func (s *Session) clearRefs() {
	s.refs = map[string]refEntry{}
}

// lookupRef returns the entry for ref, and whether it is present (i.e. not
// stale). Must be called with s.mu held (callers already hold it inside do).
func (s *Session) lookupRef(ref string) (refEntry, bool) {
	e, ok := s.refs[ref]
	return e, ok
}

// Close tears down the browser connection and its goroutines. Safe to call more
// than once. After Close a subsequent tool call reconnects lazily.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.browseCl != nil {
		s.browseCl()
		s.browseCl = nil
	}
	if s.allocStop != nil {
		s.allocStop()
		s.allocStop = nil
	}
	if s.rootStop != nil {
		s.rootStop()
		s.rootStop = nil
	}
	s.rootCtx = nil
	s.connected = false
}
