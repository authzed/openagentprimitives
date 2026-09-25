package bronzethread

import (
	"fmt"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
)

// TraceLines renders a run's authorization-relevant shape: which gate events
// fired against which phase, and which permission checks resolved which way.
//
// This is what a golden pins. Property assertions say what a scenario's author
// thought to claim; a golden says everything that happened, so a change nobody
// named still shows up in a diff. That is the value — and it is also the risk,
// because a golden that moves on scheduling noise gets regenerated blindly
// until it asserts nothing. Everything here exists to keep it stable:
//
//   - SORTED, not chronological. Record order across two independently-written
//     logs is not reproducible, and this must not be the thing that decides
//     whether a suite is green. Where order genuinely carries meaning, the
//     property assertions already pin it: Assert.Authz.Decisions compares the
//     recorded sequence per key, which is how "denied, then allowed after
//     approval" stays distinguishable from "allowed outright".
//   - DEDUPLICATED. A key checked three times because a call was retried is one
//     fact about authority, and the retry count varies with timing.
//   - Deduplicated per (key, OUTCOME), never per key. Collapsing a denial into
//     a later allow on the same key would erase the approval flow — the single
//     most important sequence this feature has — so the pair survives even
//     though the ordering between them does not.
//
// Timestamps, digests and record IDs are all excluded for the same reason: they
// change on every run and say nothing about behavior.
//
// See TraceLinesWithFlow for the flow-control half.

// TraceFlow is the flow-control half of a trace: what the info-leakage and
// trifecta gates decided, and what provenance a run minted.
//
// It exists because the two logs above cover neither. A scenario about
// per-datum provenance — a tag minted, a slot bound, a delegation refused for
// combining untrusted input with the ability to act — writes nothing to the
// plan-gate log and nothing to authz_decision, so its golden came out EMPTY.
// An empty golden is worse than no golden: it is a file that can never fail,
// which is the rot the promote-don't-start rule exists to prevent.
type TraceFlow struct {
	// Audit is the infoleakage_audit log, which despite the name carries every
	// read-gate verdict AND the trifecta's (kinds "trifecta",
	// "trifecta_closure_denied", "trifecta_unresolvable").
	Audit []infoleakageaudit.AuditRecord
	// Tags are the pt-tags the run minted.
	Tags []pttag.TagRecord
}

// unstableFlowKinds are audit kinds excluded from a trace because they do not
// reproduce between a run and a replay of that same run.
//
// Both were caught by the steel-thread round trip, which captures a golden from
// a source run and immediately replays it: the source recorded them, the replay
// did not. Neither is a per-call verdict — session_end is written by the
// cleanup hook as a session finishes, and approval_resolved when an approval
// settles — so whether either has landed by the time the driver folds its
// trace depends on where the run is, not on what it decided.
//
// Including them makes a golden that diffs against its own replay on day one,
// gets regenerated unread, and stops asserting anything. That is the precise
// rot this file is written against.
//
// Everything else stays: a per-call verdict is recorded during the call it
// describes, before anything reads it.
var unstableFlowKinds = map[string]bool{
	"session_end":       true,
	"approval_resolved": true,
}

// flowLines renders TraceFlow.
//
// Tag IDs are deliberately EXCLUDED: they are minted per run and would move the
// golden on every execution, which is the failure mode this whole file is
// written against. What is pinned is the SHAPE — how many tags, whether each
// was untrusted, and how many readers it resolved to — because that is what a
// regression would change.
func flowLines(f TraceFlow, add func(string)) {
	for _, a := range f.Audit {
		if unstableFlowKinds[a.Kind] {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "flow  %-24s", a.Kind)
		if a.Tool != "" {
			fmt.Fprintf(&b, " tool=%s", a.Tool)
		}
		// Details carries the trifecta's leg count and refused verdict. Only
		// the two stable keys are rendered; the rest vary per run.
		for _, k := range []string{"legs", "refused", "mode"} {
			if v, ok := a.Details[k]; ok {
				fmt.Fprintf(&b, " %s=%s", k, v)
			}
		}
		add(strings.TrimRight(b.String(), " "))
	}
	for _, t := range f.Tags {
		add(fmt.Sprintf("tag   kind=%s untrusted=%t readers=%d sources=%d",
			t.Kind, t.UntrustedOrigin, len(t.DirectReaders), len(t.Sources)))
	}
}

func TraceLines(gate []plangateaudit.Content, decisions []authzdecision.Decision) []string {
	return TraceLinesWithFlow(gate, decisions, TraceFlow{})
}

// TraceLinesWithFlow is TraceLines plus the flow-control records.
func TraceLinesWithFlow(gate []plangateaudit.Content, decisions []authzdecision.Decision, flow TraceFlow) []string {
	var out []string

	seen := map[string]bool{}
	add := func(line string) {
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}

	for _, g := range gate {
		var b strings.Builder
		fmt.Fprintf(&b, "gate  %-15s", g.Event)
		if g.PhaseIndex != nil {
			fmt.Fprintf(&b, " phase=%d", *g.PhaseIndex)
		}
		if g.Handle != "" {
			fmt.Fprintf(&b, " handle=%s", g.Handle)
		}
		if g.Tool != "" {
			fmt.Fprintf(&b, " tool=%s", g.Tool)
		}
		if g.Outcome != "" {
			fmt.Fprintf(&b, " outcome=%s", g.Outcome)
		}
		add(strings.TrimRight(b.String(), " "))
	}

	for _, d := range decisions {
		add(fmt.Sprintf("authz %s:%s#%s %s",
			d.ResourceType, d.ResourceID, d.Permission, d.Outcome))
	}

	flowLines(flow, add)

	slices.Sort(out)
	return out
}
