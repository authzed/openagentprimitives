package judge

// deterministic.go is the built-in ConsequenceJudge that resolves the
// obvious, cheap cases WITHOUT a model. It positively clears two FORM-FILLING
// families and two NAVIGATION families:
//
//   - FILLING a form — typing into a field (any `type` op) and toggling a
//     radio/checkbox/option (a `click` on one of those roles) — but ONLY when
//     the acted element's enclosing <form> itself contains a submit control
//     (Action.FormHasSubmit): the fill-THEN-submit pattern. The field edit
//     itself changes only LOCAL page state; the commit is a separate, later
//     action, so a field edit is benign EVEN INSIDE a form whose action path
//     is a mutating one (httpbin's /post, a /checkout, an /order). This is the
//     fix for the over-gating that prompted a human on every keystroke and
//     radio of a pizza-order form. A field/selection with no enclosing
//     submit-bearing form (a standalone autosave-on-input control, an
//     instant-save toggle, a contenteditable editor) is NOT cleared here — it
//     falls through to the fail-closed mutate default / the model. See
//     isFieldEntry/isSelectionToggle and the known-residual note below for why
//     this guard exists and what it still cannot catch.
//   - LABEL/CAPTION TEXT — a `click` whose target folded to the generic "text"
//     AX role (a field's <label>, a fieldset legend, a heading) rather than the
//     control it describes, under the SAME FormHasSubmit guard as the fill
//     family above. The read/find tools mint a ref for this role too
//     (snapshot.go), so a model can click a label instead of the input it
//     labels; that click is exactly as benign as the field edit it was aiming
//     for. See isLabelClick.
//   - NAVIGATING — a plain link-follow and a tab switch over pre-rendered
//     content.
//
// Everything it is not POSITIVELY confident is benign returns TierMutate — the
// fail-closed default and the package's central security invariant. The
// deterministic judge must never return observe/navigate/session unless the
// action positively matches one of the benign patterns below; anything
// ambiguous, empty, or merely "probably fine" falls through to mutate.
//
// The COMMIT is never cleared. A click/press on a form submit control
// (Action.Submits), or a link/element whose href path or accessible text
// carries a mutatingVerb, falls through to the fail-closed mutate default. A
// generic `<button>`/role=button is NOT a submit control but is NOT cleared
// either — it may still mutate via an onclick JS/fetch, so it falls through to
// the model (which leans mutate when unsure). "Not type=submit" is never a
// reason to clear something.
import (
	"context"
	"fmt"
	"strings"
)

// mutatingVerbs are words whose presence in an action's accessible text
// signal a commit even when the structural shape looks like navigation — the
// common case being a <a> element styled/used as a button (href="#" or a real
// path) that actually triggers a destructive or commit-y action ("Delete
// account", "Unsubscribe", "Log out"). A navigational link whose text
// contains one of these is NOT classified navigate; it falls through to the
// fail-closed mutate default.
var mutatingVerbs = []string{
	"delete", "remove", "buy", "purchase", "pay", "checkout", "order",
	"subscribe", "unsubscribe", "cancel", "confirm", "submit", "approve",
	"deny", "decline", "accept", "send", "invite", "share", "publish",
	"post", "upload", "sign out", "log out", "logout", "unfollow", "block",
	"report", "transfer", "withdraw", "deposit", "renew", "upgrade",
	"downgrade", "close account",
}

// selectionRoles are the AX roles whose `click` is a pure SELECTION toggle —
// choosing a radio, ticking a checkbox, picking a listbox/menu option. Like
// typing, a selection changes only local form state and never commits, so a
// click on one of these is positively benign (see isSelectionToggle). Note
// "menuitem" is deliberately absent: a plain menuitem is an arbitrary command
// (e.g. "Log out") that can commit, so only the *radio/*checkbox menuitem
// variants — which are selection toggles by ARIA contract — are listed.
var selectionRoles = map[string]bool{
	"radio":            true,
	"checkbox":         true,
	"option":           true,
	"menuitemradio":    true,
	"menuitemcheckbox": true,
}

// IsSelectionRole reports whether role is one of selectionRoles — exported
// so a caller OUTSIDE this package's classification path (the browser
// package's human-readable approval descriptor; see
// pkg/agent/tool/browser/action_descriptor.go) can phrase "Select '<x>'"
// for the same role set this judge treats as a selection toggle, without
// duplicating the list. Purely a DISPLAY helper: it carries no fail-closed
// obligation of its own, Classify above is still the only thing that gates.
func IsSelectionRole(role string) bool { return selectionRoles[role] }

// deterministicJudge is the built-in, model-free ConsequenceJudge.
type deterministicJudge struct{}

// NewDeterministic returns the built-in deterministic ConsequenceJudge. It is
// also registered under "deterministic" at package init time (see init()
// below); callers that only need the deterministic tiers directly (as the
// tests in this package do) can use NewDeterministic without touching the
// registry.
func NewDeterministic() ConsequenceJudge { return deterministicJudge{} }

// Name implements ConsequenceJudge.
func (deterministicJudge) Name() string { return "deterministic" }

// Classify implements ConsequenceJudge. It never errors: every input,
// however ambiguous or empty, produces a Verdict — the fail-closed mutate
// default IS the answer for anything it does not positively recognize, not a
// signal to propagate an error.
func (deterministicJudge) Classify(_ context.Context, a Action, _ PageContext) (Verdict, error) {
	switch {
	case isFieldEntry(a):
		return Verdict{
			Tier:      TierNavigate,
			Probs:     map[Tier]float64{TierNavigate: 1.0},
			Rationale: "typing into a form field (local edit; commits nothing until a separate submit)",
		}, nil
	case isSelectionToggle(a):
		return Verdict{
			Tier:      TierNavigate,
			Probs:     map[Tier]float64{TierNavigate: 1.0},
			Rationale: fmt.Sprintf("selecting a %s (local form state; commits nothing until a separate submit)", a.Role),
		}, nil
	case isLabelClick(a):
		return Verdict{
			Tier:      TierNavigate,
			Probs:     map[Tier]float64{TierNavigate: 1.0},
			Rationale: "clicking inert label/caption text inside a submit-bearing form (focuses an associated field; commits nothing)",
		}, nil
	case isNavigationalLink(a):
		return Verdict{
			Tier:      TierNavigate,
			Probs:     map[Tier]float64{TierNavigate: 1.0},
			Rationale: fmt.Sprintf("click on a navigational link (href %q, no mutating verb in its href path or text)", a.FormAction),
		}, nil
	case isTabSwitch(a):
		return Verdict{
			Tier:      TierObserve,
			Probs:     map[Tier]float64{TierObserve: 1.0},
			Rationale: "switching a pre-rendered tab (read-only view state)",
		}, nil
	default:
		return Verdict{
			Tier:      TierMutate,
			Probs:     map[Tier]float64{TierMutate: 1.0},
			Rationale: "no positive benign match; failing closed to mutate",
		}, nil
	}
}

// isNavigationalLink matches a plain link-follow: a click on an AX "link" role
// that carries a real, non-empty href path AND a non-empty accessible name,
// where NEITHER the href path nor the text contains a mutatingVerb. This is a
// POSITIVE-benign test, not a negative/absence-of-red-flag one:
//
//   - Empty Text is not a benign signal — it is the absence of one — so an
//     unlabeled link falls through to the fail-closed mutate default rather
//     than vacuously passing a "no mutating verb in empty text" check.
//   - The href PATH is inspected, not just the visible text: a link whose
//     text reads innocuous ("Yes, I'm sure") but whose href targets a
//     destructive endpoint (/account/delete) is a destructive action wearing
//     a navigational disguise, and must still classify mutate.
//
// Note there used to be a dead "href == \"#\"" guard here: actionfacts.go
// always resolves FormAction to a URL PATH (urlPath(raw.FormAction)), and a
// DOM `.href="#"` resolves to the current page's absolute URL — so FormAction
// is never literally "#" for a real click target. That branch never fired and
// has been removed; a same-page anchor is simply a link whose path/text are
// both free of mutating signals, which already classifies navigate.
func isNavigationalLink(a Action) bool {
	if a.Op != "click" || a.Role != "link" {
		return false
	}
	href := strings.TrimSpace(a.FormAction)
	if href == "" {
		return false
	}
	text := strings.TrimSpace(a.Text)
	if text == "" {
		return false
	}
	if containsAny(href, mutatingVerbs) {
		return false
	}
	return !containsAny(text, mutatingVerbs)
}

// isFieldEntry matches typing into a form field — any `type` op — but ONLY
// when the acted element's enclosing <form> itself contains a submit control
// (Action.FormHasSubmit): the fill-THEN-submit pattern. Typing sets an
// element's local value; ON ITS OWN it commits nothing, REGARDLESS of the
// enclosing form's action path. This is the deliberate change from the old
// search-only recognizer, which bailed whenever the enclosing form's action
// carried a mutatingVerb (httpbin posts to /post, "post"/"order" are
// mutatingVerbs) and so over-gated every keystroke of a committing form. The
// commit is the SEPARATE submit action, which is never a `type` op and is
// gated on its own.
//
// The FormHasSubmit guard is NOT redundant with "typing commits nothing on its
// own": a field can carry its OWN commit via a JS event handler
// (oninput/onchange) that fetch(POST)s on every keystroke — autosave, a
// live-doc editor, a contenteditable with no enclosing form at all. The DOM
// `type` action itself never POSTs, but a handler it triggers CAN, and does
// not need a submit click to do it. Requiring a sibling submit control narrows
// this recognizer to forms that have an explicit, separate, gated commit
// (httpbin's order form, a login form, a search box with a button); a
// standalone autosave-on-input field has no such control and now falls
// through to the fail-closed mutate default / the model. See the
// known-residual note below for the one shape this still cannot distinguish.
//
// Two further guards keep this fail-closed. A `type` whose target is somehow
// also a submit control (Submits — structurally impossible for a text input,
// kept as belt-and-suspenders) is not cleared. The sensitive-value concern is
// out of scope here: a.Text for an input can resolve to the typed value, but
// the judge's decision never depends on that value — the fingerprint, not the
// keystrokes, is what the web_action gate binds — so typing a password or a
// transfer amount is classified identically to typing a search query (both
// benign; the later submit is what gates).
func isFieldEntry(a Action) bool {
	if a.Op != "type" {
		return false
	}
	return !a.Submits && a.FormHasSubmit
}

// Known residual: FormHasSubmit cannot distinguish a genuine fill-then-submit
// field from an instant-save control that happens to live INSIDE a form that
// ALSO has an (unrelated) submit control — e.g. a settings page with one
// <form> wrapping both a "notifications: on/off" checkbox carrying its own
// onchange -> POST handler and a separate "Save profile" submit button. That
// checkbox still clears here: the heuristic only asks "does this form have A
// submit control," not "does THIS control's own handler also POST on
// change." This is a known, narrower residual than the pre-FormHasSubmit
// state (which cleared every field/toggle unconditionally, regardless of any
// enclosing form or submit control) and is backstopped by the explicit,
// human-approved web_host grant any browser toolbox already requires before
// it can act on a page at all — it is not a silent fail-open outside that
// grant's scope.

// isSelectionToggle matches a `click` that only CHOOSES a value — a radio,
// checkbox, listbox/menu option (selectionRoles) — but ONLY when the acted
// element's enclosing <form> itself contains a submit control
// (Action.FormHasSubmit): the same fill-THEN-submit pattern isFieldEntry
// requires. Like typing, a selection changes local form state and, ON ITS
// OWN, commits nothing until a separate submit, so it is benign even inside a
// form whose action path is a mutating one (httpbin's /post, a /checkout, an
// /order).
//
// The FormHasSubmit guard exists for the same reason it does on isFieldEntry:
// a radio/checkbox can carry its own onchange -> fetch(POST) handler (an
// instant-save toggle) with no sibling submit control, and that handler CAN
// reach the server on every click even though the click itself is a local
// selection. Requiring a submit control in the enclosing form excludes that
// standalone case, which now falls through to the fail-closed mutate
// default / the model.
//
// A submit control is never cleared here regardless (Submits guard): the role
// set excludes buttons, but the guard makes the "a commit is never a
// selection" invariant explicit and robust to a mislabeled control.
func isSelectionToggle(a Action) bool {
	if a.Op != "click" {
		return false
	}
	if a.Submits {
		return false
	}
	if !a.FormHasSubmit {
		return false
	}
	return selectionRoles[a.Role]
}

// isLabelClick matches a `click` whose target's AX role folded to the
// generic "text" bucket (snapshot.go's interestingRole maps StaticText,
// "text" and "paragraph" all onto "text") — i.e. the target is NOT an
// interactive control at all: a field's adjacent <label> caption, a
// fieldset legend, a heading, or plain body copy. The read/find tools mint a
// ref for every "interesting" role including this one (snapshot.go), so a
// model reading the page's outline can plausibly call click on a label's
// text ref instead of the control it labels, believing it must "activate"
// the field before typing or selecting into it.
//
// This is positively benign, narrowly: a StaticText node is not a listed
// form-associated element (HTMLFormElement.elements never contains it), so
// clicking it can never structurally SUBMIT a form the way a real submit
// control can — at most it focuses an associated <label for> target (the
// same local, non-committing effect isFieldEntry/isSelectionToggle already
// clear) or runs a JS handler attached directly to that text node (unusual,
// but not impossible). Two guards keep this safe:
//
//   - FormHasSubmit, the SAME fill-THEN-submit scoping isFieldEntry and
//     isSelectionToggle require: cleared only inside a form that has its own,
//     separate commit control, so an unrelated page's inert-text click (no
//     enclosing submit-bearing form) still falls through to the fail-closed
//     mutate default / the model. This is not a NEW residual: it is the one
//     isFieldEntry/isSelectionToggle already accept (a text node's own
//     onclick->fetch(POST) handler, with no sibling submit, is strictly the
//     instant-save-toggle shape those recognizers already name).
//   - a mutatingVerb check on the text itself, mirroring
//     isNavigationalLink/isTabSwitch's disguise defense: label text that
//     reads "Delete my account" or "Confirm purchase" is not cleared even
//     though its role folded to "text".
//
// Submits is checked too (defensive/belt-and-suspenders): a StaticText can
// never be isSubmitControl's true branch (it only matches BUTTON/INPUT
// tags), but the guard keeps this recognizer symmetric with every other one
// here and robust to a future change in what folds to "text".
func isLabelClick(a Action) bool {
	if a.Op != "click" || a.Role != "text" {
		return false
	}
	if a.Submits || !a.FormHasSubmit {
		return false
	}
	return !containsAny(a.Text, mutatingVerbs)
}

// isTabSwitch matches clicking an AX "tab" role: switching between
// pre-rendered panels is a read-only view-state change, never a server-side
// mutation. The "tab" role itself is the primary positive signal (a
// dedicated ARIA role declaring tab-strip semantics, not inferred from
// text) — but actionFactsScript derives FormAction from the nearest
// enclosing <form> or <a href> ancestor independent of ARIA role, so a
// tab-styled anchor can still carry a destructive href underneath (e.g. an
// `<a role="tab" href="/account/delete">`). FormAction and Text are both
// checked against mutatingVerbs so that disguise does not slip through —
// mirroring isNavigationalLink's href-path-and-text check rather than
// trusting the role alone.
func isTabSwitch(a Action) bool {
	if a.Op != "click" || a.Role != "tab" {
		return false
	}
	if containsAny(a.FormAction, mutatingVerbs) {
		return false
	}
	return !containsAny(a.Text, mutatingVerbs)
}

// containsAny reports whether text (case-insensitively) contains any of
// needles.
func containsAny(text string, needles []string) bool {
	lower := strings.ToLower(text)
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func init() {
	Register(NewDeterministic())
}
