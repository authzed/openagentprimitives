// Package toolguard implements per-tool circuit breakers (with exponential
// backoff as the breaker's recovery schedule) and per-tool rate limits,
// enforced as PreToolCall/PostToolCall pipeline hooks.
package toolguard

import (
	"context"
	"sync"
	"time"
)

// Action is what a guard denial does, ordered by severity for ceiling floors.
type Action int

const (
	ActionOff Action = iota
	ActionWarn
	ActionDeny
	ActionHalt
)

func (a Action) String() string {
	switch a {
	case ActionWarn:
		return "warn"
	case ActionDeny:
		return "deny"
	case ActionHalt:
		return "halt"
	default:
		return "off"
	}
}

// ParseAction maps the CRD enum string; empty or unknown returns def.
func ParseAction(s string, def Action) Action {
	switch s {
	case "off":
		return ActionOff
	case "warn":
		return ActionWarn
	case "deny":
		return ActionDeny
	case "halt":
		return ActionHalt
	default:
		return def
	}
}

// WindowLimit is one sliding-window call bound: at most MaxCalls in Window.
// Both halves are required — a limit missing either is unenforceable and reads
// as unlimited (v1.CallRate says the same about the CRD pair it comes from).
type WindowLimit struct {
	MaxCalls int32
	Window   time.Duration
}

// Enforced reports whether this bound can gate anything.
func (w WindowLimit) Enforced() bool { return w.MaxCalls > 0 && w.Window > 0 }

// ResolvedRule is the per-tool effective rule after the tier walk + ceiling
// clamp (see policy.go). Zero thresholds disable that breaker layer; zero
// rate fields disable that rate limit.
type ResolvedRule struct {
	FailureThreshold       int32
	OriginFailureThreshold int32
	InitialCoolOff         time.Duration
	MaxCoolOff             time.Duration
	Action                 Action

	// AuthHaltThreshold is how many CONSECUTIVE unbilled failures
	// (pipeline.ToolCallInfo.UnbilledFailure — a call whose provider metered
	// nothing, i.e. one it refused at the auth layer) end the session. Zero
	// disables the layer.
	//
	// Its own threshold rather than the breaker's because the two answer
	// different questions. The breaker counts failures and replies with a
	// cool-off, which is right when an upstream is down and backing off helps;
	// no cool-off helps a credential that will never work, so the answer there
	// is to stop and tell the operator. The action is always a halt for the same
	// reason, which is why no field carries it.
	AuthHaltThreshold int32

	RateMaxPerTurn int32
	RateMaxCalls   int32
	RateWindow     time.Duration
	// RateCeilings are the ADMIN ceiling's sliding-window bounds, carried
	// alongside the authored pair above rather than folded into it, and
	// enforced as well as it.
	//
	// They express different shapes and none implies another at every horizon:
	// a ceiling of {100 calls, 1m} permits 144,000/day, so it cannot stand in
	// for an authored {200, 24h}; that authored pair permits all 200 inside one
	// second, so it cannot stand in for the admin's burst bound. Folding them to
	// whichever wins a single calls/second comparison silently dropped a bound
	// its author had written.
	//
	// A slice because the ceiling itself can name several windows — pkg/platform/settings
	// folds the cluster and namespace Limits into one v1.ToolGuardCeiling and
	// keeps every distinct window both tiers wrote (ToolGuardCeiling.RateBounds).
	// Populated by clampVolume with only the ceiling bounds whose window differs
	// from the authored pair's; an equal window collapses exactly to
	// min(maxCalls), so it is never duplicated here.
	RateCeilings []WindowLimit
	RateAction   Action

	// Byte-volume budget (default-off; 0 = unlimited). MaxEgressBytes caps
	// serialized args (PreToolCall); MaxIngressBytes caps successful result
	// bytes (PostToolCall) on the model-read path. ByteAction is the shared
	// action for both.
	MaxEgressBytes  int64
	MaxIngressBytes int64
	ByteAction      Action

	// MaxUIIngressBytes caps result bytes on a UI data binding. See
	// IngressLimitFor for the resolution order and for why the model-sized
	// MaxIngressBytes is deliberately not consulted on that path.
	MaxUIIngressBytes int64

	// Provenance names the rule's source tier for logs/audit:
	// "class[i]" | "namespace[i]" | "cluster[i]" | "builtin" | "ceiling"
	// ("ceiling" = no rule matched and the Builtin fallback does not apply, so
	// only the Limits ceiling shaped this rule — see ForUngatedTools).
	Provenance string
}

// HasRateLimit reports whether any rate dimension is configured.
func (r ResolvedRule) HasRateLimit() bool {
	if r.RateMaxPerTurn > 0 {
		return true
	}
	any := false
	r.eachWindowLimit(func(WindowLimit) bool { any = true; return false })
	return any
}

// eachWindowLimit calls fn for every ENFORCED sliding-window bound this rule
// carries, in the order Admit sweeps them: the authored pair first, then each
// admin ceiling bound naming a different window. A call must fit under EVERY
// one of them; fn returning false stops the walk early.
//
// An iterator rather than a returned slice because the number of bounds is no
// longer fixed (see RateCeilings) and the sweep runs on every tool call — fn
// does not escape, so the walk still allocates nothing however many windows a
// ceiling folds to.
func (r ResolvedRule) eachWindowLimit(fn func(WindowLimit) bool) {
	if w := (WindowLimit{MaxCalls: r.RateMaxCalls, Window: r.RateWindow}); w.Enforced() && !fn(w) {
		return
	}
	for _, w := range r.RateCeilings {
		if w.Enforced() && !fn(w) {
			return
		}
	}
}

// longestWindow is the span of call history the sweep above still needs, so
// trimming keeps exactly what every bound counts against and no more.
func (r ResolvedRule) longestWindow() time.Duration {
	var longest time.Duration
	r.eachWindowLimit(func(w WindowLimit) bool {
		if w.Window > longest {
			longest = w.Window
		}
		return true
	})
	return longest
}

// HasDataLimit reports whether any byte dimension is configured.
func (r ResolvedRule) HasDataLimit() bool {
	return r.MaxEgressBytes > 0 || r.MaxIngressBytes > 0 || r.MaxUIIngressBytes > 0
}

// DefaultUIIngressBytes is the ingress ceiling a UI data binding takes when no
// rule configures one. It is sized for what a browser can render rather than
// for what a model can read — a table, a chart series, a document — which is
// the whole reason the UI path does not inherit MaxIngressBytes.
const DefaultUIIngressBytes int64 = 4 << 20 // 4 MiB

// IngressLimitFor returns the per-call ingress ceiling for one surface; 0
// means unlimited.
//
// On a UI data binding the model-sized MaxIngressBytes is deliberately NOT
// consulted. Of the three reasons a data-volume budget exists, only two
// survive on that path: context pollution dissolves (the result never reaches
// the model), while the exfiltration bound and the cost/upstream-DoS bound
// both survive. So the UI path takes its own ceiling, sized for what a browser
// can render rather than for what a model can read — and, unlike the model
// path, it is never unlimited: an unconfigured UI binding falls to
// DefaultUIIngressBytes.
//
// The consequence an operator must know: tightening maxIngressBytes for a
// leaky tool does not tighten its UI bindings. Set maxUIIngressBytes on the
// rule, or toolGuard.maxUIIngressBytes on the settings ceiling (which folds
// strictest-across-tiers and clamps the rule), to bound this path.
func (r ResolvedRule) IngressLimitFor(uiDataBinding bool) int64 {
	if !uiDataBinding {
		return r.MaxIngressBytes
	}
	if r.MaxUIIngressBytes > 0 {
		return r.MaxUIIngressBytes
	}
	return DefaultUIIngressBytes
}

// Disabled reports whether this rule does nothing at all.
func (r ResolvedRule) Disabled() bool {
	return r.Action == ActionOff && !r.HasRateLimit() && !r.HasDataLimit()
}

// breakerState is the classic three-state circuit.
type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// Admission is the result of an Admit call.
type Admission struct {
	Allowed bool
	// Probe marks the single half-open probe call.
	Probe bool
	// DeniedBy: "breaker" | "probe" | "rate_turn" | "rate_window"; "" when allowed.
	DeniedBy string
	// Key is the denying key ("tool/<name>" or "origin/<kind>/<name>").
	Key string
	// RateLimit is the sliding-window bound that denied, set only when
	// DeniedBy is "rate_window". A rule can carry several (the authored pair
	// and the admin ceiling's), so the denial names the one that actually fired
	// rather than leaving the caller to guess which budget it quotes.
	RateLimit WindowLimit
	// RetryAfter is the remaining cool-off for breaker denials.
	RetryAfter time.Duration
}

// Transition is one observable breaker state change.
type Transition struct {
	Event   string // "breaker_opened" | "breaker_half_open" | "breaker_closed"
	Key     string
	Trips   int32
	CoolOff time.Duration
	RetryAt time.Time
}

// OpenBreakerInfo is a snapshot row for status surfacing.
type OpenBreakerInfo struct {
	Key      string
	OpenedAt time.Time
	RetryAt  time.Time
	Trips    int32
}

type keyState struct {
	state       breakerState
	consecFails int32
	trips       int32
	openedAt    time.Time
	coolOff     time.Duration
	probing     bool
	// probeID identifies the live probe claim, so a stale release cannot hand
	// back a slot a later call legitimately re-took. See probe.go.
	probeID uint64

	// consecUnbilled counts CONSECUTIVE unbilled failures for the credential
	// halt. Separate from consecFails because the two count different things
	// at different thresholds: an outcome the provider BILLED for — success or
	// failure — breaks this streak while leaving the breaker's own count
	// untouched.
	consecUnbilled int32

	turnIndex     int
	callsThisTurn int32
	windowTimes   []time.Time
}

// Registry holds per-key breaker + rate state for one session. Safe for
// concurrent use; dispatch goroutines call Admit/Record in parallel.
type Registry struct {
	mu  sync.Mutex
	now func() time.Time
	// probeSeq mints probe claim ids; monotonic across every key.
	probeSeq uint64
	keys     map[string]*keyState
}

// NewRegistry builds a session-scoped registry. now==nil uses time.Now.
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{now: now, keys: map[string]*keyState{}}
}

func (r *Registry) key(k string) *keyState {
	ks, ok := r.keys[k]
	if !ok {
		ks = &keyState{}
		r.keys[k] = ks
	}
	return ks
}

// breakerCheck is the read-only phase: would this key admit, and does
// admitting require claiming the half-open probe?
type breakerCheck struct {
	allow      bool
	needsProbe bool
	deniedBy   string // "breaker" | "probe"
	retryAfter time.Duration
}

func (r *Registry) checkBreaker(k string) breakerCheck {
	ks := r.key(k)
	switch ks.state {
	case stateClosed:
		return breakerCheck{allow: true}
	case stateOpen:
		remaining := ks.coolOff - r.now().Sub(ks.openedAt)
		if remaining > 0 {
			return breakerCheck{deniedBy: "breaker", retryAfter: remaining}
		}
		// Cool-off elapsed: admitting transitions to half-open + claims probe.
		return breakerCheck{allow: true, needsProbe: true}
	default: // stateHalfOpen
		if ks.probing {
			return breakerCheck{deniedBy: "probe"}
		}
		// Probe slot free (previous probe recorded): claim it.
		return breakerCheck{allow: true, needsProbe: true}
	}
}

// Admit decides whether a call to toolKey (with optional originKey) may
// dispatch under rule, reserving rate slots and the half-open probe claim on
// success. Two-phase under one lock: all checks first, then all mutations —
// a denial never leaks a reservation. turn is the current turn index (for
// per-turn rate counters). Returned transitions are open→half-open moves.
// Use ToolKey(name) and OriginKey(origin) to build the key arguments; the key
// format is part of the audit contract (it appears verbatim in Admission.Key
// and Transition.Key).
//
// Every probe claim granted is filed into the probe ledger carried by ctx
// (see WithProbeLedger), so a call that never reaches Record still hands its
// claim back. Admission.Probe reports whether one was granted; the caller
// surfaces a claim taken with no ledger in ctx.
func (r *Registry) Admit(ctx context.Context, toolKey, originKey string, rule ResolvedRule, turn int) (Admission, []Transition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()

	// ---- Phase 1: checks (no mutation) ----
	var originChk, toolChk breakerCheck
	originChk.allow, toolChk.allow = true, true
	if originKey != "" && rule.OriginFailureThreshold > 0 {
		originChk = r.checkBreaker(originKey)
		if !originChk.allow {
			return Admission{DeniedBy: originChk.deniedBy, Key: originKey, RetryAfter: originChk.retryAfter}, nil
		}
	}
	if rule.FailureThreshold > 0 {
		toolChk = r.checkBreaker(toolKey)
		if !toolChk.allow {
			return Admission{DeniedBy: toolChk.deniedBy, Key: toolKey, RetryAfter: toolChk.retryAfter}, nil
		}
	}
	ks := r.key(toolKey)
	if rule.RateMaxPerTurn > 0 {
		calls := ks.callsThisTurn
		if ks.turnIndex != turn {
			calls = 0
		}
		if calls >= rule.RateMaxPerTurn {
			return Admission{DeniedBy: "rate_turn", Key: toolKey}, nil
		}
	}
	// Every enforced sliding-window bound must have room: the authored pair and
	// each admin ceiling bound naming a different window (see
	// ResolvedRule.RateCeilings). One pass over history per bound, and the fold
	// emits one bound per distinct window, so the pass count is the number of
	// horizons an admin named — the length of the history each pass walks is the
	// term that actually grows.
	var windowDenial Admission
	rule.eachWindowLimit(func(w WindowLimit) bool {
		cut := now.Add(-w.Window)
		live := int32(0)
		// len(ks.windowTimes) is bounded by the MaxCalls of the longest-window
		// bound — that bound denies once its own budget is spent, and the
		// mutation phase trims to that window on every admitted call, so stale
		// entries only persist during all-denied periods.
		for _, t := range ks.windowTimes {
			if t.After(cut) {
				live++
			}
		}
		if live >= w.MaxCalls {
			windowDenial = Admission{DeniedBy: "rate_window", Key: toolKey, RateLimit: w}
			return false
		}
		return true
	})
	if windowDenial.DeniedBy != "" {
		return windowDenial, nil
	}

	// ---- Phase 2: mutations (call is admitted) ----
	var trans []Transition
	var claimed []probeClaim
	adm := Admission{Allowed: true}
	claim := func(k string, chk breakerCheck) {
		if !chk.needsProbe {
			return
		}
		s := r.key(k)
		if s.state == stateOpen {
			trans = append(trans, Transition{Event: "breaker_half_open", Key: k, Trips: s.trips})
		}
		r.probeSeq++
		s.state, s.probing, s.probeID = stateHalfOpen, true, r.probeSeq
		adm.Probe = true
		claimed = append(claimed, probeClaim{reg: r, key: k, id: r.probeSeq})
	}
	if originKey != "" && rule.OriginFailureThreshold > 0 {
		claim(originKey, originChk)
	}
	if rule.FailureThreshold > 0 {
		claim(toolKey, toolChk)
	}
	if rule.RateMaxPerTurn > 0 {
		if ks.turnIndex != turn {
			ks.turnIndex, ks.callsThisTurn = turn, 0
		}
		ks.callsThisTurn++
	}
	// Keep the history the longest enforced window still counts against; a
	// shorter bound reads the same slice through its own cutoff.
	if longest := rule.longestWindow(); longest > 0 {
		cut := now.Add(-longest)
		kept := ks.windowTimes[:0]
		for _, t := range ks.windowTimes {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		ks.windowTimes = append(kept, now)
	}
	// Almost every admitted call claims nothing, so skip the ctx walk entirely
	// unless there is something to file.
	if len(claimed) > 0 {
		if led := probeLedgerFrom(ctx); led != nil {
			for _, c := range claimed {
				led.file(c)
			}
		}
	}
	return adm, trans
}

// releaseProbe hands back an unresolved half-open probe claim: the call that
// held it exited without an outcome (denied, halted, or interrupted before
// PostToolCall), so nothing was learned about the key's health. The key stays
// stateHalfOpen with the slot free, letting the next call probe immediately.
//
// A stale release is a no-op — id must still be the live claim — so it cannot
// steal a slot a later call re-took after Record resolved this one. Only the
// probe ledger calls this; see probe.go for why release is not an exported
// per-key operation.
func (r *Registry) releaseProbe(key string, id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ks, ok := r.keys[key]
	if !ok || !ks.probing || ks.probeID != id {
		return
	}
	ks.probing = false
}

// Record feeds one Execute outcome into the breaker layers. failed is
// Go-error ∨ Result.IsError. Returned transitions drive audit/status/logs.
func (r *Registry) Record(toolKey, originKey string, rule ResolvedRule, failed bool) []Transition {
	r.mu.Lock()
	defer r.mu.Unlock()
	var trans []Transition
	if rule.FailureThreshold > 0 {
		trans = append(trans, r.record(toolKey, rule.FailureThreshold, rule, failed)...)
	}
	if originKey != "" && rule.OriginFailureThreshold > 0 {
		trans = append(trans, r.record(originKey, rule.OriginFailureThreshold, rule, failed)...)
	}
	return trans
}

func (r *Registry) record(key string, threshold int32, rule ResolvedRule, failed bool) []Transition {
	ks := r.key(key)
	now := r.now()
	if !failed {
		wasNonClosed := ks.state != stateClosed
		ks.state, ks.consecFails, ks.trips, ks.probing = stateClosed, 0, 0, false
		if wasNonClosed {
			return []Transition{{Event: "breaker_closed", Key: key}}
		}
		return nil
	}
	ks.consecFails++
	probeFailed := ks.state == stateHalfOpen && ks.probing
	if probeFailed || (ks.state == stateClosed && ks.consecFails >= threshold) {
		ks.trips++
		ks.coolOff = backoff(rule.InitialCoolOff, rule.MaxCoolOff, ks.trips)
		ks.state, ks.openedAt, ks.probing = stateOpen, now, false
		return []Transition{{
			Event: "breaker_opened", Key: key, Trips: ks.trips,
			CoolOff: ks.coolOff, RetryAt: now.Add(ks.coolOff),
		}}
	}
	return nil
}

// RecordAuth feeds one Execute outcome into the credential-halt layer and
// reports whether originKey's consecutive unbilled-failure streak just REACHED
// threshold.
//
// unbilledFailure is the platform's own structured observation that the call
// failed and its provider metered nothing for it (see
// pipeline.ToolCallInfo.UnbilledFailure). Anything else — a success, or a
// failure the provider CHARGED for — clears the streak, because "consecutive"
// is meant literally: a billed call between two refusals is proof the
// credential was still being accepted, so the two are not a run.
//
// Counted per ORIGIN and never per tool. One credential backs every tool a
// toolkit exposes, which cuts both ways and both ways are what a per-tool
// count would get wrong: two refusals across two of its tools ARE two refusals
// of the same credential and must add up, and a billed call on any of its
// tools proves the credential works and must clear the others. An origin-less
// tool (in-process meta, a toolkit-less sandbox) has no provider, so there is
// no credential for one to have refused and nothing a halt could tell an
// operator to go fix.
//
// threshold <= 0 disables the layer WITHOUT touching the streak, so a rule
// carrying no credential halt can never leave stale counts behind for one that
// does.
func (r *Registry) RecordAuth(originKey string, threshold int32, unbilledFailure bool) bool {
	if threshold <= 0 || originKey == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ks := r.key(originKey)
	if !unbilledFailure {
		ks.consecUnbilled = 0
		return false
	}
	ks.consecUnbilled++
	return ks.consecUnbilled >= threshold
}

// OpenBreakers snapshots every currently-open circuit for status surfacing.
func (r *Registry) OpenBreakers() []OpenBreakerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []OpenBreakerInfo
	for k, ks := range r.keys {
		if ks.state == stateOpen {
			out = append(out, OpenBreakerInfo{
				Key: k, OpenedAt: ks.openedAt,
				RetryAt: ks.openedAt.Add(ks.coolOff), Trips: ks.trips,
			})
		}
	}
	return out
}

// backoff is initial × 2^(trips−1), capped at max (0 max = uncapped).
func backoff(initial, max time.Duration, trips int32) time.Duration {
	d := initial
	for i := int32(1); i < trips; i++ {
		d *= 2
		if max > 0 && d >= max {
			return max
		}
	}
	if max > 0 && d > max {
		return max
	}
	return d
}

// ToolKey / OriginKey build registry keys.
func ToolKey(name string) string { return "tool/" + name }

// OriginKey returns "" when origin is empty (callers pass "" for origin-less
// tools such as sandbox and meta).
func OriginKey(origin string) string {
	if origin == "" {
		return ""
	}
	return "origin/" + origin
}
