package steelthread

// Unexported internals this package's EXTERNAL test package needs.
//
// The tests are `package steelthread_test` on purpose — they exercise the
// capture through the same surface a caller has — but a few derivations are
// intricate enough to deserve a direct test rather than only being observed
// through Capture's output. This file is the one place that widens the seam,
// so a reader can see exactly how much of the internals the tests reach.

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// ElideSkillSourcesForTest exposes elideSkillSources, the structural rewrite
// that moves a third-party skill repo's authority onto a stand-in and empties
// the skills that came from it.
//
// Tested directly rather than only through Capture because the two things it
// must get right are properties of the REWRITTEN MANIFESTS, not of a bundle: the
// provenance gate has to still pass over the emitted pair, and the refusal on a
// prompt-visible skill has to fire before anything is written. Both are cheapest
// to state against the fixture the rewrite returns, and the whole-capture tests
// then exercise the same function through its real caller.
func ElideSkillSourcesForTest(f FixtureInput, rules []SkillElision) (FixtureInput, []bt.SkillElision, error) {
	return elideSkillSources(f, rules)
}

// GenerateStandInWithWordForTest runs the shared stand-in generator under a
// caller-supplied stem word and admissibility test.
//
// It exists for ONE branch: the layout's `accept` hook, which rejects a
// candidate whose SHAPE its own parser would refuse and lets the widening loop
// try again. No stem shipped today reaches it — "elided.example/" truncates from
// the right into a valid authority at every width, and "redacted" has no
// separator to truncate into — so a mutation deleting the hook survives every
// test that goes through the real layouts. That makes it an untested branch
// guarding a future stem edit, which is exactly the shape of a guard that is
// quietly not working when someone finally needs it.
//
// The seam takes the word and the predicate rather than a whole standInStem so
// the external test package does not need the type; what it drives is the
// mechanism (the generator consults accept and keeps widening), not a layout
// this package ships.
func GenerateStandInWithWordForTest(old, word string, accept func(string) bool) (string, error) {
	return generateStandIn(old, map[string]bool{}, nil, standInStem{
		word:   word,
		domain: "steelthread-standin-seam-test",
		what:   "test",
		accept: accept,
	})
}

// SandboxToolsForTest exposes sandboxTools, which resolves each declared
// toolBundle to the SpiceboxClass tool names its toolspecs reach.
//
// Tested directly because the indirection is the part that is easy to get
// wrong: a toolspec is matched to a class tool by the toolspec's TOOLKIT name,
// not by its own name, and a wrong derivation produces a plausible-looking
// LLM-facing name that matches nothing in the transcript. Observed only through
// Capture, that surfaces as an unrelated finding about a tool call.
func SandboxToolsForTest(in FixtureInput) map[string][]string { return sandboxTools(in) }

// OpaqueValueForTest exposes opaqueValue, the value-shape rule that decides
// whether a token:/password: field carries credential MATERIAL or a credential's
// NAME.
//
// Tested directly because it is a judgement with many neighbouring cases — a
// UUID group, a timestamp fragment, a hyphenated identity name, a hex digest —
// and observing it only through a whole capture would need one fixture per case
// to say which side of the line each falls on.
func OpaqueValueForTest(v []byte) bool { return opaqueValue(v) }

// PatternShape describes one row of the structural pattern table, flattened so
// a test can assert over every row without reaching the unexported type.
type PatternShape struct {
	Name       string
	NumSubexp  int
	ValueGated bool
}

// StructuralPatternsForTest exposes the pattern table so a test can assert a
// structural property of every row rather than of one row it remembered to name.
func StructuralPatternsForTest() []PatternShape {
	out := make([]PatternShape, 0, len(structuralPatterns))
	for _, p := range structuralPatterns {
		out = append(out, PatternShape{
			Name:       p.Name,
			NumSubexp:  p.Re.NumSubexp(),
			ValueGated: p.Value != nil,
		})
	}
	return out
}

// ClassSkillRefsForTest exposes classSkillRefs, the one-line derivation that
// decides whether a capture is refused for opting into skills nothing here
// emits. Direct, because "a nil class and a class with no skills both say
// nothing" is a property of the derivation and observing it only through a
// whole capture would need a fixture per case.
func ClassSkillRefsForTest(class *spiceboxv1alpha1.AgentClass) []string {
	return classSkillRefs(class)
}
