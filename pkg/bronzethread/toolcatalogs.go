package bronzethread

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
)

// What the run was OFFERED, per turn, and what the replay does with it.
//
// A transcript records what the model CALLED and never what it COULD have
// called, and those differ in the case that matters: a tool withheld by a
// capability gate or a plan-gate phase is invisible in a transcript, because
// not calling a tool you were never offered looks exactly like not calling a
// tool you declined. The tool_catalog memory Kind records the difference; a
// bundle carries it forward, and the replay offers exactly what was recorded.
//
// # The recorded set is an assertion
//
// At each turn the replay's own tool set is compared against the one in force:
//
//   - a recorded tool MISSING at replay is a HARD FAILURE. The system stopped
//     offering something it offered, which is a regression whether it was a
//     gate that closed or a synthesizer that broke.
//   - an EXTRA tool is withheld — the request offers the recorded set, not the
//     computed one — and, unless the bundle declared it, is also a hard
//     failure. A gate that stops withholding is the exact regression this
//     exists to catch, and it is invisible in every other assertion a bundle
//     can make.
//   - an extra the bundle DECLARED (Bundle.ExpectedExtraTools) is withheld in
//     silence. The one legitimate source is a gate the capture itself removed
//     to make the bundle replayable at all; see that field's doc.
//
// Withholding rather than merely reporting is what makes the rest of the bundle
// replay: a model offered tools the captured run could not see would be
// answering a different question at every step downstream.
//
// A bundle with no catalogs pins nothing and runs exactly as it did before,
// the same compatibility rule MintedIDs takes.

// ToolCatalog is one recorded change to the offered tool set.
//
// One entry per CHANGE, not per turn — the set is constant across most of a
// session, and it is the movement that carries the information. The set in
// force at a turn is the last entry at or before it; see toolcatalog.InForce,
// which both this package and the memory Kind's own reader resolve through.
type ToolCatalog struct {
	// FromTurnIndex is the transcript index this set took effect at. It stays
	// in force until the next entry.
	FromTurnIndex int `json:"fromTurnIndex"`
	// Tools are the LLM-facing tool names offered from that turn, sorted.
	Tools []string `json:"tools"`
}

// validateToolCatalogs refuses a catalog list the replay could not honour.
//
// Each failure is silent at replay if it is not caught here. An unsorted or
// duplicated Tools list still "works" but makes the diff a reader gets on a
// mismatch unreadable; out-of-order or repeated turn indices make the step
// function ambiguous, so the set in force at a turn depends on the order the
// entries happen to be marshalled in; and an empty Tools list would assert the
// run was offered nothing, which is never true (respond_to_user is always
// there) and would fail every turn it covered for the wrong reason.
func (b Bundle) validateToolCatalogs() error {
	prev := -1
	for i, c := range b.ToolCatalogs {
		if c.FromTurnIndex < 0 {
			return fmt.Errorf("toolCatalogs[%d] has a negative fromTurnIndex (%d); a transcript index is never negative",
				i, c.FromTurnIndex)
		}
		if c.FromTurnIndex <= prev {
			return fmt.Errorf("toolCatalogs[%d] fromTurnIndex %d does not follow %d: entries must be in strictly "+
				"increasing turn order, or which set is in force at a turn depends on marshalling order",
				i, c.FromTurnIndex, prev)
		}
		prev = c.FromTurnIndex
		if len(c.Tools) == 0 {
			return fmt.Errorf("toolCatalogs[%d] (turn %d) lists no tools; an empty catalog claims the run was "+
				"offered nothing, which is never true", i, c.FromTurnIndex)
		}
		if !sort.StringsAreSorted(c.Tools) {
			return fmt.Errorf("toolCatalogs[%d] (turn %d) is not sorted; the Kind stores it sorted and a "+
				"mismatch diff is unreadable otherwise", i, c.FromTurnIndex)
		}
		for j := 1; j < len(c.Tools); j++ {
			if c.Tools[j] == c.Tools[j-1] {
				return fmt.Errorf("toolCatalogs[%d] (turn %d) lists %q twice", i, c.FromTurnIndex, c.Tools[j])
			}
		}
	}

	if len(b.ExpectedExtraTools) > 0 && len(b.ToolCatalogs) == 0 {
		return fmt.Errorf("expectedExtraTools names %d tool(s) but the bundle pins no toolCatalogs, so nothing "+
			"compares a tool set and the exemption excuses nothing", len(b.ExpectedExtraTools))
	}
	seen := map[string]bool{}
	for _, n := range b.ExpectedExtraTools {
		if seen[n] {
			return fmt.Errorf("expectedExtraTools names %q twice", n)
		}
		seen[n] = true
	}
	return nil
}

// ToolCatalogCheck holds a replay to the tool set its captured run recorded.
//
// Guarded because the runner consults it from the turn loop while the test
// goroutine constructed it, the same reason MintedIDSequence is.
type ToolCatalogCheck struct {
	mu        sync.Mutex
	changes   []toolcatalog.Content
	extras    map[string]bool
	reported  map[string]bool
	consulted bool
	report    func(string)
}

// NewToolCatalogCheck builds the check for one replay.
//
// report is how a mismatch becomes a visible failure. It is called on the
// goroutine running the turn loop, so a test wires it to t.Errorf rather than
// t.Fatalf — Fatalf's Goexit there would abandon the runner's stack rather than
// the test's. A nil report PANICS instead of passing quietly: a mismatch
// nothing reports is the silent ride-through this whole mechanism replaces.
func NewToolCatalogCheck(catalogs []ToolCatalog, expectedExtras []string, report func(string)) *ToolCatalogCheck {
	if report == nil {
		report = func(msg string) { panic("bronzethread: " + msg) }
	}
	changes := make([]toolcatalog.Content, 0, len(catalogs))
	for _, c := range catalogs {
		changes = append(changes, toolcatalog.Content{FromTurnIndex: c.FromTurnIndex, Tools: c.Tools})
	}
	extras := make(map[string]bool, len(expectedExtras))
	for _, n := range expectedExtras {
		extras[n] = true
	}
	return &ToolCatalogCheck{
		changes:  changes,
		extras:   extras,
		reported: map[string]bool{},
		report:   report,
	}
}

// HeldFromAssembly names the tools a replay must keep OUT of the capability
// assembly, rather than merely out of each request's tool list.
//
// The distinction is not cosmetic, and it is the difference between this
// mechanism working and not. Some things read the tool set ONCE, when the
// session is composed: introspect_tool's name index, the system prompt's tool
// listing, the plan-gate surface. A tool the captured run gained MID-session
// was absent from all three there, and stays absent from all three for the rest
// of that run even after it is being offered — which is exactly what happens in
// production when the mid-session refresher adds a tool. A replay that composes
// with the tool present reproduces none of that, and the tell is an
// introspect_tool result listing tools the recorded one did not name.
//
// So: a declared extra that the FIRST recorded catalog does not contain is held
// out of the assembly and rejoins the live set afterwards, arriving the way the
// refresher's tools arrive. An extra the first catalog DOES contain was already
// there when the run composed, and is left alone.
//
// Empty when the bundle pins no catalogs — there is then no recorded set to
// hold anything back from.
func (c *ToolCatalogCheck) HeldFromAssembly() []string {
	if c == nil || len(c.changes) == 0 || len(c.extras) == 0 {
		return nil
	}
	first := c.changes[0].Tools
	var out []string
	for name := range c.extras {
		if !slices.Contains(first, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Filter returns the func to wire into the runner's replay seam, or nil when
// the bundle pinned no catalogs.
//
// nil is the compatibility path AND the honest one: a bundle that records no
// catalog is making no claim about what the run was offered, so the runner
// offers what it composed, exactly as in production.
func (c *ToolCatalogCheck) Filter() func(turnIndex int, offered []string) []string {
	if c == nil || len(c.changes) == 0 {
		return nil
	}
	return c.offer
}

// offer answers which of a turn's computed tools to actually offer, reporting
// every difference from what was recorded.
func (c *ToolCatalogCheck) offer(turnIndex int, offered []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consulted = true

	want := toolcatalog.InForce(c.changes, turnIndex)
	if want == nil {
		// No record covers this turn — the run reached a Send before the first
		// recorded catalog. UNKNOWN, not "nothing": make no claim and offer
		// what was composed.
		return offered
	}

	have := make(map[string]bool, len(offered))
	for _, n := range offered {
		have[n] = true
	}

	var missing []string
	for _, n := range want {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		c.reportOnce("missing:"+quoteList(missing), fmt.Sprintf(
			"tool catalog at turn %d: the captured run was offered %s and this run was NOT — a tool the "+
				"system used to offer is gone, which is a gate that closed, a synthesizer that failed, or a "+
				"fixture that no longer declares it",
			turnIndex, quoteList(missing)))
	}

	var undeclared []string
	keep := make([]string, 0, len(offered))
	for _, n := range offered {
		if slices.Contains(want, n) {
			keep = append(keep, n)
			continue
		}
		// An extra. Withheld either way — the model must see the tool set the
		// captured run saw, or every step after this answers a different
		// question — but only an UNDECLARED one is a finding.
		if !c.extras[n] {
			undeclared = append(undeclared, n)
		}
	}
	if len(undeclared) > 0 {
		c.reportOnce("extra:"+quoteList(undeclared), fmt.Sprintf(
			"tool catalog at turn %d: this run offers %s and the captured run did NOT, and the bundle does not "+
				"declare them in expectedExtraTools — a gate that used to withhold a tool has stopped, which is "+
				"the regression the recorded catalog exists to catch",
			turnIndex, quoteList(undeclared)))
	}
	return keep
}

// Consulted reports whether the runner ever asked this check what to offer.
//
// It exists because a correct bundle produces NO findings, so a check that was
// never wired in looks exactly like a check that found nothing — and every
// catalog assertion would then be silently absent while the suite stayed green.
// That is the assertion-that-asserts-nothing failure this whole mechanism is
// meant to remove, so a driver confirms the question was asked at all.
func (c *ToolCatalogCheck) Consulted() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consulted
}

// reportOnce reports a difference the first time it is seen and never again.
//
// key is the difference — a direction plus the tools involved — and NOT the
// message, which names the turn: a recorded set stays in force across many
// turns, so one difference recurs on every one of them, and keying on the
// message would report each. That buries the finding under repetitions of
// itself, and makes the count read as a measure of severity when it only
// measures how many turns happened to follow.
//
// The message that DOES get through names the first turn the difference
// appeared at, which is the one worth chasing.
func (c *ToolCatalogCheck) reportOnce(key, msg string) {
	if c.reported[key] {
		return
	}
	c.reported[key] = true
	c.report(msg)
}

// quoteList renders names for a message: quoted, comma-separated, in a stable
// order so two runs of the same failure produce the same text.
func quoteList(names []string) string {
	s := slices.Clone(names)
	sort.Strings(s)
	for i, n := range s {
		s[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(s, ", ")
}
