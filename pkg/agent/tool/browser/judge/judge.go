package judge

// judge.go defines Phase E's pluggable ConsequenceJudge: the seam that
// classifies one browser acting call (click/type/press/select/submit) by its
// CONSEQUENCE TIER — observe, navigate, session (all auto-allowed) vs mutate
// (gated on the web_action grant SpiceDB caveat, Phase E's E3-E5 tasks). This
// task (E6) defines the interface, a same-package registry, and a built-in
// deterministic judge that resolves the obvious, cheap cases without a model.
// E7 adds a Haiku-backed judge for everything the deterministic tiers don't
// positively recognize; E8 wires a resolved judge into acting dispatch.
//
// This package deliberately does NOT import pkg/agent/tool/browser (the
// acting-call caller) or pkg/authz (the authorization layer the gate
// eventually calls into). Action and PageContext below are this package's OWN
// small structs, mirroring the shape of browser.ActionFacts/browser.PageState
// field-for-field where relevant — the caller (the browser package, wired in
// E8) derives them from its own ActionFacts/PageState. Reusing those types
// directly here would create an import cycle once the browser package calls
// into this one at dispatch time (browser -> judge -> browser).
import "context"

// Tier is one of the four consequence buckets a ConsequenceJudge classifies
// an action into. Only TierMutate is gated; the other three are auto-allowed.
type Tier string

const (
	// TierObserve is a read-only interaction: it changes what is visible (an
	// expand/disclosure, a tab switch over pre-rendered content) without
	// touching any server-side state.
	TierObserve Tier = "observe"
	// TierNavigate is a page-to-page or in-page navigation: following a link,
	// typing into a search/filter box. It moves the user around; it commits
	// nothing.
	TierNavigate Tier = "navigate"
	// TierSession is a client-local session affordance (e.g. dismissing a
	// banner, accepting a cookie-consent prompt). Reserved for a future
	// judge — the deterministic tiers in this package do not positively match
	// into it yet.
	TierSession Tier = "session"
	// TierMutate is anything that commits a change: a purchase, a delete, a
	// form submit to an unrecognized endpoint — or, critically, anything a
	// judge is not CONFIDENT is benign. This is the fail-closed default: an
	// uncertain action is always treated as consequential, never waved
	// through.
	TierMutate Tier = "mutate"
)

// Action is the classifier's input: the facts about ONE pending acting call,
// mirroring pkg/agent/tool/browser.ActionFacts field-for-field but as this
// package's own type (see the package doc for why it is not reused directly).
type Action struct {
	Op         string // click | type | press | select | submit
	Role       string // the target's AX role (e.g. "link", "button", "searchbox", "textbox", "tab")
	Text       string // the target's accessible name / visible text
	FormAction string // the enclosing form's action path, or a link's href path; "" if neither
	Container  string // a stable identifier for the target's nearest ancestor container
	Host       string // the canonical page host
	PageURL    string // the current page URL
	// Submits is true when the target is a form SUBMIT CONTROL — the structural
	// signal that this action COMMITS a form rather than merely fills one. It
	// mirrors browser.ActionFacts.Submits (derived in actionfacts.go's injected
	// JS: an <input type=submit|image>, or a <button> whose resolved type is
	// submit). A judge uses it to keep a commit (click on a submit control) out
	// of the benign fast-path even when the same form's field edits (typing,
	// toggling a radio) are positively cleared.
	Submits bool
	// FormHasSubmit is true when the target's enclosing <form> itself contains a
	// submit control (mirrors browser.ActionFacts.FormHasSubmit). The
	// deterministic judge's field-entry/selection-toggle recognizers require
	// this before clearing a type/selection as benign: it is what distinguishes
	// the fill-THEN-submit pattern from a standalone autosave-on-input field or
	// an instant-save-on-toggle control that ships its own JS
	// oninput/onchange -> POST with no sibling submit.
	FormHasSubmit bool
}

// PageContext is the minimal page-wide signal a judge sees alongside an
// Action. A judge classifies the ACTION primarily; page context is
// supporting evidence only — e.g. a Haiku-backed judge (E7) may fold
// VisibleTextSample into its prompt. Deliberately small: this is not
// pkg/agent/tool/browser's full PageState, just enough to disambiguate an
// action without dragging the page-fingerprint machinery into this package's
// dependency surface.
type PageContext struct {
	// URLPath is the current page's path (no scheme/host/query).
	URLPath string
	// VisibleTextSample is a short, caller-chosen sample of the page's (or the
	// action's container's) visible text.
	VisibleTextSample []string
}

// Verdict is a judge's classification of one Action.
type Verdict struct {
	// Tier is the judge's chosen consequence tier.
	Tier Tier
	// Probs is the judge's distribution over the four tiers (should sum to
	// ~1.0). A deterministic judge reports a one-hot distribution; a
	// probabilistic judge (E7) reports its real confidence spread, which lets
	// E8 tune an allow/gate threshold instead of trusting a bare label.
	Probs map[Tier]float64
	// Rationale is a short, human-readable reason for the chosen tier —
	// surfaced in logs/audit, never parsed by a caller.
	Rationale string
}

// ConsequenceJudge classifies one acting call's consequence tier. Backends
// self-register under a name (see registry.go); the built-in deterministic
// judge in this package (deterministic.go) is one such backend, registered
// under "deterministic".
type ConsequenceJudge interface {
	// Classify returns a Verdict for one pending action, given supporting page
	// context. An error means the judge itself failed (e.g. a backend call
	// errored) — it is NOT how a judge expresses "ambiguous"; ambiguous is
	// TierMutate with a low-confidence Probs spread, per the fail-closed
	// invariant documented on TierMutate above.
	Classify(ctx context.Context, a Action, page PageContext) (Verdict, error)
	// Name identifies this judge for registration/lookup.
	Name() string
}
