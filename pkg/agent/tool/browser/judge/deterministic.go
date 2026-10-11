package judge

// deterministic.go is the built-in ConsequenceJudge that resolves the
// obvious, cheap cases WITHOUT a model: a plain link-follow, a search/filter
// textbox entry, and a tab switch over pre-rendered content. Everything it is
// not POSITIVELY confident is benign returns TierMutate — the fail-closed
// default and the package's central security invariant. The deterministic
// judge must never return observe/navigate/session unless the action
// positively matches one of the benign patterns below; anything ambiguous,
// empty, or merely "probably fine" falls through to mutate.
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

// searchKeywords are words that, found in a textbox's accessible name,
// identify it as a search/filter/query entry field rather than an arbitrary
// data-entry field (which stays fail-closed to mutate — typing into an
// unidentified textbox could be filling a money-transfer amount just as
// easily as a search query).
var searchKeywords = []string{"search", "filter", "query", "find"}

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
	case isNavigationalLink(a):
		return Verdict{
			Tier:      TierNavigate,
			Probs:     map[Tier]float64{TierNavigate: 1.0},
			Rationale: fmt.Sprintf("click on a navigational link (href %q, no mutating verb in its href path or text)", a.FormAction),
		}, nil
	case isSearchEntry(a):
		return Verdict{
			Tier:      TierNavigate,
			Probs:     map[Tier]float64{TierNavigate: 1.0},
			Rationale: "typing into a search/filter/query textbox",
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

// isSearchEntry matches typing into a search/filter/query field: either the
// AX role is the dedicated "searchbox" role, or a generic "textbox" whose
// accessible text/label identifies it as one via searchKeywords.
//
// Like isNavigationalLink/isTabSwitch, a search-looking ROLE or TEXT is not
// trusted alone: actionFactsScript populates FormAction for a textbox target
// (the enclosing <form action>) exactly as it does for a link, so a
// search-styled input nested in a destructive form (e.g. a searchbox whose
// enclosing form posts to /accounts/transfer/execute) would otherwise commit
// on submit while looking like a filter. A destructive FormAction PATH beats
// the search-looking role/text and falls through to the fail-closed mutate
// default — the check is applied to BOTH the searchbox-role and the
// textbox-keyword branch.
func isSearchEntry(a Action) bool {
	if a.Op != "type" {
		return false
	}
	if containsAny(a.FormAction, mutatingVerbs) {
		return false
	}
	if a.Role == "searchbox" {
		return true
	}
	if a.Role == "textbox" {
		return containsAny(a.Text, searchKeywords)
	}
	return false
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
