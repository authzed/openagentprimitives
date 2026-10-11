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
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"golang.org/x/sync/semaphore"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

// connectRetryDelay is the pause before the single reconnect attempt. The pod
// is Ready before the first tool call, but a lazy connect with one retry makes
// a wake race harmless.
const connectRetryDelay = 500 * time.Millisecond

// credHoldTTL bounds how long a Fetched website credential lingers in the
// in-memory hold (spec §5.3). A login flow (type username, type password,
// type OTP, submit) completes in seconds; a few minutes covers a slow page
// without keeping the plaintext around for the session's lifetime. The hold is
// ALSO dropped on Close, so this is only the upper bound for an idle session.
const credHoldTTL = 3 * time.Minute

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

	// currentHost reads the canonical host of the live page. Overridable for
	// tests (production reads chromedp.Location through the gate and
	// canonicalizes it); newSession wires the live implementation. It backs
	// CurrentHost, which the acting tools' web_host authorization check reads as
	// the per-call resource instance.
	currentHost func() string

	// mu is the per-session browser mutex, serializing all CDP access on the
	// one chromedp connection. It is a binary semaphore (weight 1) rather than
	// a plain sync.Mutex so do() can bound the acquire wait by the CALLER'S
	// ctx (Acquire(ctx, 1) returns ctx.Err() the moment ctx is done, even
	// mid-wait) instead of blocking past a deadline with nothing to show for
	// it — see do's doc. golang.org/x/sync/semaphore is already a go.mod
	// dependency, so this is a drop-in acquire/release pair rather than a
	// hand-rolled ctx-aware mutex.
	mu        *semaphore.Weighted
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

	// credMu guards the credential hold and the blanked-field set below. It is
	// SEPARATE from mu: the filler reads/writes the hold OUTSIDE do() (the Fetch
	// is network I/O that must not hold the single browser lock), so sharing mu
	// would serialize a vault round-trip against every browser op. Close acquires
	// mu then credMu; nothing acquires them in the other order, so there is no
	// deadlock.
	credMu sync.Mutex
	// hold is the short-lived, time-bounded cache of Fetched website credentials
	// (spec §5.3), keyed by canonical host. A value here is for IMMEDIATE USE
	// ONLY — never logged, never persisted, dropped on expiry and on Close.
	hold map[string]heldCred
	// blanked is the set of DOM backend node IDs a credential was typed into,
	// recorded by the type tool for Phase C Task C5's blanking pass to consume.
	blanked map[int64]bool

	// store persists captured session state (cookies + localStorage) across reap
	// and wake for a `persistent` toolbox (Task C9). nil for an `ephemeral`
	// toolbox, which disables capture and inject entirely. Written by the runner's
	// per-session state Secret, the sole writer of that Secret's content.
	store StateStore
	// logName is the LLM-facing toolbox name, used only as structured log context
	// for best-effort capture/inject failures.
	logName string
	// injectAttempted guards the once-per-boot inject on first attach. Guarded by
	// mu (set inside do(), which holds mu), so a concurrent first tool call injects
	// exactly once. An inject is ATTEMPTED once even if it fails, so a persistently
	// bad blob cannot wedge every subsequent call behind a retry.
	injectAttempted bool
}

// heldCred is one Fetched credential kept briefly in the §5.3 hold so repeated
// placeholders for a host (username, password, otp, …) resolve from a single
// Fetch. Carries an expiry so a stale entry is never reused.
type heldCred struct {
	cred      directcred.Credential
	expiresAt time.Time
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
	s := &Session{
		gateURL: gateURL,
		token:   token,
		refs:    map[string]refEntry{},
		hold:    map[string]heldCred{},
		blanked: map[int64]bool{},
		mu:      semaphore.NewWeighted(1),
	}
	s.newAllocator = func(parent context.Context) (context.Context, context.CancelFunc) {
		// The gate rewrites /json/version's webSocketDebuggerUrl back to itself
		// WITH the token, so chromedp stays on the authenticated gate rather than
		// dialing Chrome directly. chromedp's default modifyURL preserves the
		// query, so the token reaches the gate's auth check.
		return chromedp.NewRemoteAllocator(parent, s.gateURL+"/json/version?t="+s.token)
	}
	s.currentHost = s.liveCurrentHost
	return s
}

// CurrentHost returns the canonical host (browserpolicy.CanonicalHost) of the
// current page, or "" when there is no live page (about:blank), the location
// cannot be read, or the host fails canonicalization.
//
// Empty is the fail-closed answer by design: an acting tool exposes this as the
// web_host resource instance its External check resolves against, and an empty
// id resolves to an UnresolvedResource denial — routed to a human, never a
// silent allow.
func (s *Session) CurrentHost() string { return s.currentHost() }

// liveCurrentHost reads chromedp.Location through the gate and canonicalizes its
// host. Every failure collapses to "" (fail-closed); nothing is surfaced to the
// caller, because an acting call with no resolvable host must deny, not error.
func (s *Session) liveCurrentHost() string {
	var loc string
	if err := s.do(context.Background(), shortTimeout, func(c context.Context) error {
		return runAction(c, func(ac context.Context) error { return chromedp.Location(&loc).Do(ac) })
	}); err != nil {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host, err := browserpolicy.CanonicalHost(u.Hostname())
	if err != nil {
		return ""
	}
	return host
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

// doTiming carries do()'s internal stage durations: how long the call waited
// to acquire mu, how long ensureConnected took, and how long fn itself ran.
// do() discards them; doTimed returns them for the one caller that needs to
// explain an intermittent timeout — CaptureRedacted logs them as INFO
// diagnostics because the browser pod is EPHEMERAL (Task #53): once the pod
// is gone, these numbers are the only record of which stage was slow.
type doTiming struct {
	LockWait        time.Duration
	EnsureConnected time.Duration
	Run             time.Duration
}

// do runs fn against the browser tab, honoring the caller's ctx for
// cancellation and applying a per-call timeout. It serializes all browser
// operations and connects lazily. fn receives a context already bound to the
// chromedp browser context, so it can invoke chromedp.Run / cdproto actions.
func (s *Session) do(ctx context.Context, timeout time.Duration, fn func(ctx context.Context) error) error {
	_, err := s.doTimed(ctx, timeout, fn)
	return err
}

// doTimed is do's real implementation, additionally returning the per-stage
// timing described on doTiming. Every do() call site but CaptureRedacted
// goes through the do() wrapper above and ignores the timing; doTimed itself
// is unchanged behavior for them.
//
// The mu acquire uses Acquire(ctx, 1) rather than a plain sync.Mutex Lock():
// a plain Lock() cannot be bounded by ctx, so a call stuck behind another
// in-flight do() (the gated acting call's Execute, or the classify path's own
// capturePageState round-trip) blocks past ctx's deadline with no error and
// no log — the wait is invisible, and whatever time it ate is gone from the
// budget fn then tries to do its CDP work in. Acquire(ctx, 1) instead returns
// ctx.Err() the instant ctx is done, even mid-wait, so a contended capture
// now fails fast and attributably instead of silently eating the deadline.
func (s *Session) doTimed(ctx context.Context, timeout time.Duration, fn func(ctx context.Context) error) (doTiming, error) {
	var timing doTiming
	lockStart := time.Now()
	if err := s.mu.Acquire(ctx, 1); err != nil {
		timing.LockWait = time.Since(lockStart)
		return timing, err
	}
	defer s.mu.Release(1)
	timing.LockWait = time.Since(lockStart)

	connStart := time.Now()
	connErr := s.ensureConnected()
	timing.EnsureConnected = time.Since(connStart)
	if connErr != nil {
		return timing, connErr
	}
	// Inject persisted state on the FIRST attach after a pod boot, before fn runs —
	// so a wake's restored cookies are in place before the first browser action.
	// Once per boot, best-effort, under mu (the connection is live here). A nil
	// store (ephemeral) or an empty Secret (first boot) makes this a no-op.
	s.maybeInject(ctx)
	runCtx, cancel := context.WithTimeout(s.browseCtx, timeout)
	defer cancel()
	// Propagate the caller's cancellation (an interrupt or the LLM call ending)
	// into the chromedp run, which is parented on the long-lived browser ctx.
	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	runStart := time.Now()
	err := fn(runCtx)
	timing.Run = time.Since(runStart)
	return timing, err
}

// maybeInject performs the once-per-boot state injection on first attach. It is
// called with s.mu held (from do, right after ensureConnected) and runs directly
// on s.browseCtx — NOT through s.do, which would re-lock mu. A nil store disables
// it; an empty blob (first boot) injects nothing. Failures are LOGGED with origin
// context and swallowed (repo rule: never crash the turn on a best-effort restore
// — the login simply isn't present and the model re-logs-in, spec §5.5). The
// attempt flag is set unconditionally so a bad blob cannot wedge every later call.
func (s *Session) maybeInject(ctx context.Context) {
	if s.store == nil || s.injectAttempted {
		return
	}
	s.injectAttempted = true
	data, err := s.store.Load(ctx)
	if err != nil {
		slog.Warn("browser: load persisted state failed; starting with a clean session",
			"toolbox", s.logName, "gate", s.gateURL, "err", err.Error())
		return
	}
	blob, err := unmarshalBlob(data)
	if err != nil {
		slog.Warn("browser: persisted state is unreadable; starting with a clean session",
			"toolbox", s.logName, "gate", s.gateURL, "err", err.Error())
		return
	}
	if len(blob.Cookies) == 0 && len(blob.Origins) == 0 {
		return // first boot: nothing to inject.
	}
	ictx, cancel := context.WithTimeout(s.browseCtx, shortTimeout)
	defer cancel()
	if err := injectFrom(ictx, blob); err != nil {
		slog.Warn("browser: inject persisted state failed; the model may need to log in again",
			"toolbox", s.logName, "gate", s.gateURL, "err", err.Error())
	}
}

// captureAndPersist captures the live cookie jar + current-origin localStorage,
// merges it with the prior saved blob, and writes it back through the StateStore.
// A no-op for an ephemeral toolbox (nil store). Every failure is RETURNED with
// context so the caller can log and continue (capture must never fail a turn —
// the next acting call or the SIGTERM capture retries). Not called with mu held:
// the capture goes through s.do (which takes mu) and the store I/O runs outside it.
func (s *Session) captureAndPersist(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	var captured stateBlob
	if err := s.do(ctx, shortTimeout, func(c context.Context) error {
		return captureInto(c, &captured)
	}); err != nil {
		return fmt.Errorf("capture browser state: %w", err)
	}
	prior, err := s.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load prior browser state: %w", err)
	}
	priorBlob, err := unmarshalBlob(prior)
	if err != nil {
		// A corrupt prior blob must not block a fresh capture: start from the
		// capture alone rather than refusing to persist anything.
		slog.Warn("browser: prior persisted state is unreadable; overwriting with a fresh capture",
			"toolbox", s.logName, "err", err.Error())
		priorBlob = stateBlob{}
	}
	data, err := marshalBlob(mergeBlob(priorBlob, captured))
	if err != nil {
		return fmt.Errorf("marshal browser state: %w", err)
	}
	if err := s.store.Save(ctx, data); err != nil {
		return fmt.Errorf("persist browser state: %w", err)
	}
	return nil
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

// ErrStaleRef is the sentinel a fact-reading helper (actionFacts,
// capturePageState) wraps into its returned error when lookupRef reports a
// miss — i.e. the ref named a page element that is simply GONE (the page
// re-rendered since the agent last read it). It is distinct from every other
// fact-read fault (a CDP transport error, a JS exception mid-walk): those
// faults mean "we don't know what this call would do," which must stay
// fail-closed-to-mutate, while a stale ref means "this call cannot execute at
// all," because every acting op's Execute path independently re-resolves the
// same ref and refuses with a clean tool error before touching the page. A
// caller distinguishes the two with errors.Is(err, ErrStaleRef); never match
// on the error's string text, which is presentation, not contract.
var ErrStaleRef = errors.New("ref is stale")

// heldCredential returns the hold's entry for host, and whether it is present
// and unexpired. An expired entry is dropped and reported absent. Safe for
// concurrent use; takes credMu, never mu.
func (s *Session) heldCredential(host string) (directcred.Credential, bool) {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	hc, ok := s.hold[host]
	if !ok {
		return directcred.Credential{}, false
	}
	if time.Now().After(hc.expiresAt) {
		delete(s.hold, host)
		return directcred.Credential{}, false
	}
	return hc.cred, true
}

// putHeldCredential places cred in the §5.3 hold under host, time-bounded by
// credHoldTTL. The value is for immediate use only and is dropped on expiry and
// on Close — it is never logged or persisted.
func (s *Session) putHeldCredential(host string, cred directcred.Credential) {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	if s.hold == nil {
		s.hold = map[string]heldCred{}
	}
	s.hold[host] = heldCred{cred: cred, expiresAt: time.Now().Add(credHoldTTL)}
}

// markBlanked records that a credential was typed into backendNodeID, so Phase
// C Task C5's blanking pass can clear the field's value afterward.
func (s *Session) markBlanked(backendNodeID int64) {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	if s.blanked == nil {
		s.blanked = map[int64]bool{}
	}
	s.blanked[backendNodeID] = true
}

// blankedNodes returns a snapshot of the backend node IDs a credential was
// typed into (for C5). The returned slice is a copy; the caller may mutate it.
func (s *Session) blankedNodes() []int64 {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	out := make([]int64, 0, len(s.blanked))
	for id := range s.blanked {
		out = append(out, id)
	}
	return out
}

// blankedSet returns a copy of the blanked backend-node-ID set, for
// buildSnapshot to redact by backendNodeID (spec §3.6). A copy, not the live
// map, so the snapshot build never races a concurrent markBlanked/clearBlanked.
func (s *Session) blankedSet() map[int64]bool {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	out := make(map[int64]bool, len(s.blanked))
	for id := range s.blanked {
		out[id] = true
	}
	return out
}

// anyBlanked reports whether at least one field is currently blanked — a
// credential placeholder was typed into it and the page has not navigated
// since. screenshot refuses while this is true (spec §3.6).
func (s *Session) anyBlanked() bool {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	return len(s.blanked) > 0
}

// clearBlanked drops the whole blanked set: every element it named was on a
// document that is now gone. Called alongside clearRefs ONLY on a genuine
// document replacement — navigate / back / a successful follow (which is a
// navigate) — because that is the one event that reliably detaches every prior
// backend node. It is deliberately the ONLY thing that un-blanks mid-session:
//
//   - submit does NOT call it: a submit may be intercepted by the page (an SPA
//     onsubmit that pushState's a new route while keeping the password input
//     mounted), so it is not a reliable "the fields are gone" signal.
//   - observation (read/find) does NOT prune by AX-tree presence either:
//     Chromium omits a display:none subtree from the accessibility tree, so a
//     field hidden by a tab/accordion/"show password" toggle — still in the DOM
//     with its typed value — would look gone and be re-exposed verbatim on
//     re-show. Over-blanking a field that really was removed mid-document (it
//     stays blanked until the next nav) is a harmless usability wart; this is
//     strictly fail-closed.
func (s *Session) clearBlanked() {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	s.blanked = map[int64]bool{}
}

// Close tears down the browser connection and its goroutines, and clears the
// credential hold and blanked-field set. Safe to call more than once. After
// Close a subsequent tool call reconnects lazily.
func (s *Session) Close() {
	// Background: Close must always eventually tear the connection down, so
	// it never bounds the wait by a caller ctx the way do()'s Acquire does.
	_ = s.mu.Acquire(context.Background(), 1)
	defer s.mu.Release(1)
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

	// Drop every held credential and the blanked set: a closed session must
	// leave no plaintext secret in memory. credMu is taken after mu here; no
	// other path takes them in the opposite order.
	s.credMu.Lock()
	s.hold = map[string]heldCred{}
	s.blanked = map[int64]bool{}
	s.credMu.Unlock()
}
