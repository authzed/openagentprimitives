package v1alpha1

import (
	"math"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CallRate expresses a sliding-window call limit (maxCalls over window) as
// calls per second — the only quantity comparable across two different windows.
// Comparing call COUNTS instead lets a {100 calls, 1m} burst bound
// (144,000/day) displace an authored {200 calls, 24h}, relaxing the very rule
// a ceiling exists to tighten.
//
// Even so, nothing chooses one pair over another: bounds with differing windows
// are carried ALONGSIDE each other and all are enforced, because neither shape
// implies the other at every horizon — a rate comparison says which is stricter
// on average, never which is stricter over one second. CallRate therefore
// decides only PRESENTATION (which pair leads a folded ceiling), never which
// bound survives.
//
// A limit missing EITHER half is unlimited (+Inf): enforcement needs both
// positive, so a half-authored pair caps nothing and must never read as a
// bound. +Inf keeps ENFORCEMENT safe for objects stored before the XValidation
// rules on RateLimitSpec and ToolGuardCeiling existed, and keeps a zero window
// from being divided by.
//
// It is NOT a backstop for ADMISSION, and that difference wedged a cluster:
// both types are mirrored into status.effectiveSettings, so a stored
// half-authored pair enforced nothing (fine) and then made the status write
// inadmissible (fatal), failing every reconcile with an error naming a field on
// another object. Whatever is mirrored into status must be narrowed to what
// these rules accept — the ceiling fold and the tier-policy mirror both drop a
// pair that cannot enforce, and report it.
func CallRate(maxCalls int32, window time.Duration) float64 {
	if maxCalls <= 0 || window <= 0 {
		return math.Inf(1)
	}
	return float64(maxCalls) / window.Seconds()
}

// CallRateBound is one sliding-window call bound: at most MaxCalls in Window.
// Both halves are required, because half of a pair caps nothing (see CallRate)
// — expressing the pair as one struct is what makes a half-authored bound
// unrepresentable here, rather than something an XValidation rule has to catch.
type CallRateBound struct {
	// MaxCalls is the cap on calls inside Window.
	// +kubebuilder:validation:Minimum=1
	MaxCalls int32 `json:"maxCalls"`
	// Window is the sliding span MaxCalls is counted over. A non-positive
	// duration enforces nothing; the resolver reports one rather than folding
	// it in (pkg/platform/settings, ToolGuardRateBoundUnenforceable).
	Window metav1.Duration `json:"window"`
}

// Breaker / rate-limit action values (BreakerSpec.Action, RateLimitSpec.Action,
// ToolGuardCeiling.MinAction). Severity order: off < warn < deny < halt.
const (
	ToolGuardActionOff  = "off"
	ToolGuardActionWarn = "warn"
	ToolGuardActionDeny = "deny"
	ToolGuardActionHalt = "halt"
)

// ToolGuardPolicy is an ordered, first-match-wins rule list governing per-tool
// circuit breakers and rate limits. It appears on AgentClass.spec.toolGuard and
// on settings Defaults; the tier walk is class → namespace defaults → cluster
// defaults → built-in rule (pkg/authz/toolguard.Builtin).
type ToolGuardPolicy struct {
	// Rules are evaluated in order, first match wins; empty means this tier
	// contributes nothing and the walk falls through.
	// +optional
	// +listType=atomic
	Rules []ToolGuardRule `json:"rules,omitempty"`
}

// ToolGuardRule pairs a tool matcher with breaker and/or rate-limit specs. A
// matched rule fully replaces the built-in rule for that tool: a nil Breaker
// means no breaker; a nil RateLimit means no rate limit.
type ToolGuardRule struct {
	// Match selects the tools this rule governs.
	Match ToolGuardMatch `json:"match"`

	// Breaker configures the circuit breaker; nil means matched tools get none.
	// +optional
	Breaker *BreakerSpec `json:"breaker,omitempty"`
	// RateLimit caps call volume; nil means matched tools get no rate limit.
	// +optional
	RateLimit *RateLimitSpec `json:"rateLimit,omitempty"`
	// DataLimit caps per-call byte volume; nil means no byte cap.
	// +optional
	DataLimit *DataLimitSpec `json:"dataLimit,omitempty"`
}

// ToolGuardMatch selects tools. All set fields must match (AND); an empty
// field matches anything. Tool and Origin accept path.Match globs.
type ToolGuardMatch struct {
	// Kind is the runtime tool kind. Sidecar-toolbox tools report "mcp"
	// (they are synthesized through the MCP synthesizer); select them via
	// Origin "sidecartoolbox/*".
	// +kubebuilder:validation:Enum=sandbox;mcp;meta
	// +optional
	Kind string `json:"kind,omitempty"`
	// Tool is a glob over the LLM-visible tool name (e.g. "github_*").
	// +optional
	Tool string `json:"tool,omitempty"`
	// Origin is a glob over the tool's origin in "<kind>/<name>" form,
	// e.g. "mcpserver/github" or "sidecartoolbox/*".
	// Note: a bare "*" also matches origin-less tools (empty origin); use a
	// prefixed glob like "mcpserver/*" to scope to tools that have an origin.
	// +optional
	Origin string `json:"origin,omitempty"`
}

// BreakerSpec configures the circuit breaker for matched tools. Zero-valued
// fields inherit the built-in defaults (threshold 5, origin threshold 10,
// cool-off 30s doubling to a 10m cap, action deny).
type BreakerSpec struct {
	// FailureThreshold is consecutive Execute failures (per tool) that open
	// the breaker.
	// +kubebuilder:validation:Minimum=1
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
	// OriginFailureThreshold is consecutive failures across ALL tools of the
	// tool's origin that open the origin breaker (denying every sibling).
	// +kubebuilder:validation:Minimum=1
	// +optional
	OriginFailureThreshold int32 `json:"originFailureThreshold,omitempty"`
	// InitialCoolOff is the first cool-off period after the breaker opens;
	// it doubles on each successive trip up to MaxCoolOff. Default 30s.
	// +optional
	InitialCoolOff *metav1.Duration `json:"initialCoolOff,omitempty"`
	// MaxCoolOff caps the exponential-backoff cool-off. Default 10m.
	// +optional
	MaxCoolOff *metav1.Duration `json:"maxCoolOff,omitempty"`
	// Action when the breaker denies: halt ends the session; deny returns an
	// IsError tool_result; warn logs/audits but allows; off disables the
	// breaker for matched tools.
	// +kubebuilder:validation:Enum=halt;deny;warn;off
	// +optional
	Action string `json:"action,omitempty"`
}

// RateLimitSpec caps call volume for matched tools regardless of success.
//
// maxCalls and window are enforced only as a pair (see CallRate), so the
// apiserver refuses a half-authored one rather than admitting a rule that
// enforces nothing while reading as if it did.
// +kubebuilder:validation:XValidation:rule="has(self.maxCalls) == has(self.window)",message="rateLimit.maxCalls and rateLimit.window must be set together"
type RateLimitSpec struct {
	// MaxCallsPerTurn caps calls within a single agent turn; 0 is unlimited.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxCallsPerTurn int32 `json:"maxCallsPerTurn,omitempty"`
	// MaxCalls caps calls in the sliding Window; both must be set together.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxCalls int32 `json:"maxCalls,omitempty"`
	// Window is the sliding-window span for MaxCalls; both must be set together.
	// +optional
	Window *metav1.Duration `json:"window,omitempty"`
	// Action when a rate cap is hit: halt ends the session; deny errors the
	// call; warn logs and audits but allows it.
	// +kubebuilder:validation:Enum=halt;deny;warn
	// +optional
	Action string `json:"action,omitempty"`
}

// DataLimitSpec caps the byte volume a matched tool may move per call,
// independent of success or call count. Default-off: a zero dimension is
// unlimited. Egress (serialized tool args) is enforced at PreToolCall; ingress
// (tool result bytes) at PostToolCall. The ingress cap applies to ALL results,
// error included — a tool could mark an unbounded exfil payload IsError to
// dodge it. Like RateLimitSpec, there is no "off" action: a data limit is
// disabled by omitting the field or leaving a dimension zero; Action only
// selects what happens when a configured limit is exceeded.
type DataLimitSpec struct {
	// MaxEgressBytes caps the serialized tool-args size sent outbound per call.
	// Exceeding it (at PreToolCall) applies Action; on deny the tool does not run.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxEgressBytes int64 `json:"maxEgressBytes,omitempty"`
	// MaxIngressBytes caps the tool-result size returned inbound per call (any
	// result, success or error). Exceeding it (at PostToolCall) applies Action;
	// on deny the result is withheld (replaced with an IsError) so the oversized
	// payload never reaches the model.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxIngressBytes int64 `json:"maxIngressBytes,omitempty"`
	// MaxUIIngressBytes caps result bytes on an agent-UI DATA BINDING, whose
	// result is rendered by a browser and never read by the model. Unset means
	// the platform's browser-sized default applies (toolguard's
	// DefaultUIIngressBytes) — NOT unlimited, and NOT MaxIngressBytes. Set this
	// to bind the UI path tighter or looser than the platform default;
	// tightening maxIngressBytes alone does not affect it.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxUIIngressBytes int64 `json:"maxUIIngressBytes,omitempty"`
	// Action when a byte limit is exceeded: halt ends the session; deny errors
	// the call (egress: tool not run; ingress: result withheld); warn
	// logs/audits but allows.
	// +kubebuilder:validation:Enum=halt;deny;warn
	// +optional
	Action string `json:"action,omitempty"`
}

// ToolGuardCeiling is the Limits-side hard bound lower tiers cannot escape.
// Folded strictest-across-tiers (like SettingsBudgetCeiling).
//
// maxCalls and window bound call volume only as a pair (see CallRate): both the
// cross-tier fold and the per-rule clamp skip a ceiling missing either half, so
// a half-authored one is admitted, reported by nothing, and enforces nothing.
// The apiserver refuses it instead. rateBounds needs no such rule: it carries
// each pair as one struct with both halves required.
// +kubebuilder:validation:XValidation:rule="has(self.maxCalls) == has(self.window)",message="toolGuard.maxCalls and toolGuard.window must be set together"
type ToolGuardCeiling struct {
	// MaxFailureThreshold caps the effective breaker threshold (min wins).
	// Note: the per-origin threshold (BreakerSpec.OriginFailureThreshold)
	// deliberately has no ceiling yet.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxFailureThreshold *int32 `json:"maxFailureThreshold,omitempty"`
	// MinInitialCoolOff raises the effective initial cool-off (max wins).
	// Note: MaxCoolOff deliberately has no floor yet.
	// +optional
	MinInitialCoolOff *metav1.Duration `json:"minInitialCoolOff,omitempty"`
	// MinAction is a severity floor (off < warn < deny < halt). Setting
	// "deny" makes the breaker non-disableable below this tier.
	// +kubebuilder:validation:Enum=warn;deny;halt
	// +optional
	MinAction *string `json:"minAction,omitempty"`
	// MaxCallsPerTurn / MaxCalls+Window impose rate ceilings even when no
	// lower-tier rule configures a rate limit.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxCallsPerTurn *int32 `json:"maxCallsPerTurn,omitempty"`
	// MaxCalls caps calls in the sliding Window; both must be set together.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxCalls *int32 `json:"maxCalls,omitempty"`
	// Window is the sliding-window span for MaxCalls; both must be set together.
	// +optional
	Window *metav1.Duration `json:"window,omitempty"`
	// RateBounds carries sliding-window bounds BESIDE MaxCalls/Window. Every
	// bound binds: a call must fit under all of them, and a lower tier can only
	// add bounds, never trade one away.
	//
	// A ceiling is a conjunction, not a choice. Two tiers naming different
	// windows have written bounds neither of which implies the other — a
	// cluster {5 calls, 1s} permits 432,000/day, a namespace {10 calls, 24h}
	// permits all 10 inside one second — so collapsing them by calls/second
	// discards a bound its author wrote, on the very surface this type calls
	// the hard bound lower tiers cannot escape.
	//
	// The settings fold writes it: the strictest-by-rate pair leads in
	// MaxCalls/Window, so a reader that knows only the pair still sees a real
	// bound, and every other distinct window lands here, deduped
	// strictest-per-window and window-ordered so the object is stable to
	// re-stamp onto status. Authoring it directly is how one tier expresses
	// burst-plus-sustained on its own.
	//
	// It deliberately carries no MaxItems: one schema governs both the AUTHORED
	// surface and the FOLDED one stamped onto status, so a cap of N would also
	// bind a fold that unions two tiers of N+1 windows and can legitimately
	// produce 2N+1 — the status write rejected by the very bound meant to keep
	// it small, wedging the reconcile. Enforcement stays cheap instead because
	// the per-call sweep is one pass over call history per bound, and the size
	// of that history is set by maxCalls over the longest window, not by how
	// many horizons are named.
	// +listType=atomic
	// +optional
	RateBounds []CallRateBound `json:"rateBounds,omitempty"`
	// MaxEgressBytes / MaxIngressBytes impose per-call byte ceilings even when
	// no lower-tier rule configures a data limit (an unset limit is
	// "unlimited", so the ceiling wins). Folded strictest-across-tiers (min).
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxEgressBytes *int64 `json:"maxEgressBytes,omitempty"`
	// MaxIngressBytes is the inbound half of the same per-call byte ceiling.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxIngressBytes *int64 `json:"maxIngressBytes,omitempty"`
	// MaxUIIngressBytes imposes a ceiling on the UI data-binding ingress path
	// (toolguard.DefaultUIIngressBytes when neither a rule nor this ceiling
	// sets one — this path is never unlimited). Folded strictest-across-tiers
	// (min), independent of MaxIngressBytes.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxUIIngressBytes *int64 `json:"maxUIIngressBytes,omitempty"`
}

// EffectiveToolGuard preserves the per-tier policies for the runner's
// session-start rule walk (rules cannot be pre-folded — first match in the
// highest tier wins per tool), plus the pre-folded strictest ceiling.
//
// CONSTRAINT: this type lives on status yet reaches ToolGuardPolicy and
// ToolGuardCeiling, the same types the AUTHORED surfaces use. Every validation
// rule on anything reachable from here is therefore attached to a status path
// too, governing status writes derived from objects the apiserver never
// re-validates. A rule a stored object violates does not fail that object — it
// fails the operator's status write, wedging the reconcile.
//
// So before adding a validation rule (XValidation, Minimum, Required, Enum) to
// any type reachable from here, make the settings resolver guarantee the value
// it stamps satisfies it, or keep the rule off the shared type.
// TestNoUnguardedStatusCELRules enumerates the CEL rules that reach a status
// path today and fails when a new one appears.
type EffectiveToolGuard struct {
	// Cluster is the cluster-tier policy, kept un-folded for the rule walk.
	// +optional
	Cluster *ToolGuardPolicy `json:"cluster,omitempty"`
	// Namespace is the namespace-tier policy, kept un-folded for the rule walk.
	// +optional
	Namespace *ToolGuardPolicy `json:"namespace,omitempty"`
	// Ceiling is the strictest-across-tiers bound, pre-folded.
	// +optional
	Ceiling *ToolGuardCeiling `json:"ceiling,omitempty"`
}

// ToolGuardStatus is the live enforcement picture on AgentSession status.
type ToolGuardStatus struct {
	// OpenBreakers lists the circuits currently denying calls; empty means none.
	// +optional
	// +listType=map
	// +listMapKey=key
	OpenBreakers []OpenBreaker `json:"openBreakers,omitempty"`
}

// OpenBreaker is one currently-open circuit.
type OpenBreaker struct {
	// Key is the breaker key: "tool/<name>" or "origin/<kind>/<name>".
	Key string `json:"key"`
	// OpenedAt is when this circuit last opened.
	// +optional
	OpenedAt metav1.Time `json:"openedAt,omitempty"`
	// RetryAt is when the cool-off elapses and calls are admitted again.
	// +optional
	RetryAt metav1.Time `json:"retryAt,omitempty"`
	// Trips counts consecutive opens without an intervening success.
	// +optional
	Trips int32 `json:"trips,omitempty"`
}
