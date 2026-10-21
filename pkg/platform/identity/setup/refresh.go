// refresh.go — the --refresh flag's scoping and its "did you mean?" help.
//
// --refresh names ONE credential, by the SuggestedName printed in the
// "already set up: <name>" status line, and re-runs just that credential's
// setup flow forced past the idempotency check. The engine (Run →
// runRequirement) drives a *refreshFilter: it skips every other credential,
// marks the named one matched, and — when the name matched nothing — answers
// with the nearest candidates so a typo gets a pointer instead of a silent
// no-op where the user believes they refreshed something and did not.
package setup

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xrash/smetrics"
)

// refreshFilter scopes a run to the single credential named by --refresh.
//
// want == "" means the flag was not given and the filter is INERT: consider
// returns true for everything (so every credential runs as before), active is
// false, and unmatchedErr is nil. A nil *refreshFilter is treated the same, so
// callers need not guard. When want is set, the filter records each credential
// name the run could have matched (for suggestions), runs only the named one,
// and records whether that name was ever seen.
type refreshFilter struct {
	want       string
	only       string          // the --only scope that narrowed candidates, for error context
	matched    bool
	candidates []string        // distinct credential names seen, in encounter order
	seen       map[string]bool // dedups candidates by name
}

// active reports whether --refresh scoped this run.
func (f *refreshFilter) active() bool { return f != nil && f.want != "" }

// consider records name as a candidate (deduped) and reports whether this
// credential's flow should run: true only for the FIRST requirement matching
// the named credential — and, because the filter is active, that one is then
// forced. When the filter is inert it records nothing and returns true, so a
// run without --refresh is unchanged.
//
// The engine dedups requirements on name|provider, so the same credential name
// required through two different providers reaches consider twice. Only the
// first is run: without --refresh the second would be skipped as already set up
// by the first, so returning false here keeps --refresh from prompting twice
// and having the second store overwrite the first.
func (f *refreshFilter) consider(name string) bool {
	if !f.active() {
		return true
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if !f.seen[name] {
		f.seen[name] = true
		f.candidates = append(f.candidates, name)
	}
	if name == f.want {
		if f.matched {
			return false // already handled under the first provider; don't prompt again
		}
		f.matched = true
		return true
	}
	return false
}

// unmatchedErr reports a --refresh value that named no credential in the run's
// requirements — nil when the flag was inert or the name did match. It mirrors
// the Slack near-miss pass (channelkinds/slack/lookup_nearmiss.go): closest
// names by edit distance, so a typo gets "did you mean …?" rather than a clean
// exit that refreshed nothing.
func (f *refreshFilter) unmatchedErr() error {
	if !f.active() || f.matched {
		return nil
	}
	// Name the --only scope when set: it narrowed the candidate set before this
	// filter saw any credential, so a correct --refresh name can still land here
	// because --only filtered out the target that carried it.
	scope := ""
	if f.only != "" {
		scope = fmt.Sprintf(" within --only %q", f.only)
	}
	if len(f.candidates) == 0 {
		return fmt.Errorf("setup: --refresh %q: no credentials to refresh%s", f.want, scope)
	}
	avail := append([]string(nil), f.candidates...)
	sort.Strings(avail)
	msg := fmt.Sprintf("setup: no credential named %q to refresh%s", f.want, scope)
	if near := nearestNames(f.want, f.candidates); len(near) > 0 {
		msg += "; did you mean " + joinQuotedOr(near) + "?"
	}
	return fmt.Errorf("%s (available: %s)", msg, strings.Join(avail, ", "))
}

// nearestNames returns the candidate names within edit distance of want,
// closest first (ties alphabetical), capped at a few. Same WagnerFischer
// Levenshtein + length-scaled threshold the Slack near-miss pass uses, so the
// two "did you mean" surfaces behave consistently.
func nearestNames(want string, candidates []string) []string {
	maxDist := nameMatchThreshold(want)
	type scored struct {
		name string
		dist int
	}
	var out []scored
	for _, c := range candidates {
		if d := smetrics.WagnerFischer(want, c, 1, 1, 1); d <= maxDist {
			out = append(out, scored{c, d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
		}
		return out[i].name < out[j].name
	})
	const maxSuggestions = 3
	if len(out) > maxSuggestions {
		out = out[:maxSuggestions]
	}
	names := make([]string, len(out))
	for i := range out {
		names[i] = out[i].name
	}
	return names
}

// nameMatchThreshold scales the acceptable edit distance with the length of
// the requested name — about one edit per five characters, minimum two — so a
// short name does not fuzzy-match every credential while a longer one still
// tolerates a transposition or two. Mirrors nearMissThreshold in the Slack
// near-miss pass.
func nameMatchThreshold(want string) int {
	t := (len([]rune(want)) + 4) / 5
	if t < 2 {
		t = 2
	}
	return t
}

// joinQuotedOr renders names as a human-facing list: `"a"`, `"a" or "b"`,
// `"a", "b" or "c"`.
func joinQuotedOr(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = fmt.Sprintf("%q", n)
	}
	switch len(q) {
	case 0:
		return ""
	case 1:
		return q[0]
	case 2:
		return q[0] + " or " + q[1]
	default:
		return strings.Join(q[:len(q)-1], ", ") + " or " + q[len(q)-1]
	}
}
