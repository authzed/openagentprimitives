package v1alpha1

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRosterNames(t *testing.T) {
	cases := []struct {
		name string
		spec AgentClassSpec
		want []string
	}{
		{"unset: delegates to nobody", AgentClassSpec{}, nil},
		{"declared roster", AgentClassSpec{Subagents: []string{"demo-coder", "demo-sre"}}, []string{"demo-coder", "demo-sre"}},
		{"duplicates collapse", AgentClassSpec{Subagents: []string{"demo-coder", "demo-coder"}}, []string{"demo-coder"}},
		{"order preserved, not sorted", AgentClassSpec{Subagents: []string{"demo-sre", "demo-coder"}}, []string{"demo-sre", "demo-coder"}},
		{
			// subagentModes raises a member's ceiling; it does not add members.
			// A class named only there is not on the roster, and the AgentClass
			// controller refuses that shape outright — but RosterNames must not
			// be the thing that quietly admits it in the meantime.
			"subagentModes does not confer membership",
			AgentClassSpec{SubagentModes: map[string][]string{"demo-coder": {"chat"}}},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.spec.RosterNames())
		})
	}
}

func TestPermittedSubagentModes(t *testing.T) {
	cases := []struct {
		name string
		spec AgentClassSpec
		ask  string
		want []string
	}{
		{
			// The Track 1a shape: a roster written before spec.subagentModes
			// existed. It must keep delegating, and it must keep delegating
			// single_turn ONLY.
			name: "old-shape []string roster: single_turn only, nothing implied",
			spec: AgentClassSpec{Subagents: []string{"demo-coder"}},
			ask:  "demo-coder",
			want: []string{SubagentModeSingleTurn},
		},
		{
			name: "off-roster class: nothing permitted, so the check fails closed on its own",
			spec: AgentClassSpec{Subagents: []string{"demo-coder"}},
			ask:  "demo-sre",
			want: nil,
		},
		{
			name: "no roster at all: nothing permitted",
			spec: AgentClassSpec{},
			ask:  "demo-coder",
			want: nil,
		},
		{
			// Two facts in one row, because they are the same fact from both
			// sides: single_turn is added (the agent may always narrow), and
			// task is NOT (chat does not imply the mode next to it).
			name: "declared chat: single_turn is added and task is not",
			spec: AgentClassSpec{
				Subagents:     []string{"demo-coder"},
				SubagentModes: map[string][]string{"demo-coder": {SubagentModeChat}},
			},
			ask:  "demo-coder",
			want: []string{SubagentModeSingleTurn, SubagentModeChat},
		},
		{
			name: "a widening declared for one member does not reach another",
			spec: AgentClassSpec{
				Subagents:     []string{"demo-coder", "demo-sre"},
				SubagentModes: map[string][]string{"demo-coder": {SubagentModeChat}},
			},
			ask:  "demo-sre",
			want: []string{SubagentModeSingleTurn},
		},
		{
			name: "explicit single_turn does not duplicate the implied one",
			spec: AgentClassSpec{
				Subagents:     []string{"demo-coder"},
				SubagentModes: map[string][]string{"demo-coder": {SubagentModeSingleTurn, SubagentModeTask}},
			},
			ask:  "demo-coder",
			want: []string{SubagentModeSingleTurn, SubagentModeTask},
		},
		{
			name: "an unrecognized declared mode is dropped, never returned as permitted",
			spec: AgentClassSpec{
				Subagents:     []string{"demo-coder"},
				SubagentModes: map[string][]string{"demo-coder": {"chatt", SubagentModeTask}},
			},
			ask:  "demo-coder",
			want: []string{SubagentModeSingleTurn, SubagentModeTask},
		},
		{
			name: "empty ask: nothing permitted rather than a roster-wide answer",
			spec: AgentClassSpec{Subagents: []string{"demo-coder"}},
			ask:  "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.spec.PermittedSubagentModes(tc.ask))
		})
	}
}

func TestPermittedSubagentModes_NilSpecIsSafeAndPermitsNothing(t *testing.T) {
	var spec *AgentClassSpec
	assert.Nil(t, spec.PermittedSubagentModes("demo-coder"),
		"a nil spec must answer 'nothing permitted' rather than panic: the accessor is a fail-closed gate input")
}

func TestIsSubagentMode(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"single_turn is a mode", SubagentModeSingleTurn, true},
		{"task is a mode", SubagentModeTask, true},
		{"chat is a mode", SubagentModeChat, true},
		{"attended is a mode", SubagentModeAttended, true},
		{"a misspelling is not a mode", "chatt", false},
		{"empty is NOT a mode: unset must stay distinguishable from misspelled", "", false},
		{"case matters", "Chat", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsSubagentMode(tc.mode))
		})
	}
}

func TestSubagentModesAll_ReturnsAFreshSliceCallersCannotCorrupt(t *testing.T) {
	first := SubagentModesAll()
	assert.Equal(t, []string{SubagentModeSingleTurn, SubagentModeTask, SubagentModeChat, SubagentModeAttended}, first,
		"narrowest first, so a refusal message reads as a ladder")

	first[0] = "clobbered"
	assert.Equal(t, SubagentModeSingleTurn, SubagentModesAll()[0],
		"a caller mutating the returned slice must not corrupt the canonical list")
}

func TestSplitRosterEntry(t *testing.T) {
	valid := "reviewer@sha256:" + strings.Repeat("ab", 32)
	cases := []struct {
		entry, wantName, wantDigest string
		wantPinned                  bool
	}{
		{"reviewer", "reviewer", "", false},
		{valid, "reviewer", "sha256:" + strings.Repeat("ab", 32), true},
		// Only an exact @sha256:<64 hex> suffix is a pin; anything else is a
		// literal (and the CRD pattern refuses it at admission).
		{"reviewer@sha256:short", "reviewer@sha256:short", "", false},
		{"reviewer@latest", "reviewer@latest", "", false},
	}
	for _, tc := range cases {
		name, digest, pinned := SplitRosterEntry(tc.entry)
		assert.Equal(t, tc.wantName, name, tc.entry)
		assert.Equal(t, tc.wantDigest, digest, tc.entry)
		assert.Equal(t, tc.wantPinned, pinned, tc.entry)
	}
}

func TestRosterNames_StripsPins(t *testing.T) {
	pin := "sha256:" + strings.Repeat("cd", 32)
	s := &AgentClassSpec{Subagents: []string{"a", "b@" + pin, "a@" + pin}}
	assert.Equal(t, []string{"a", "b"}, s.RosterNames())
}

func TestRosterPins(t *testing.T) {
	p1 := "sha256:" + strings.Repeat("11", 32)
	p2 := "sha256:" + strings.Repeat("22", 32)
	s := &AgentClassSpec{Subagents: []string{"plain", "one@" + p1, "dup@" + p1, "dup@" + p1, "conflict@" + p1, "conflict@" + p2}}
	pins := s.RosterPins()
	assert.NotContains(t, pins, "plain")
	assert.Equal(t, []string{p1}, pins["one"])
	assert.Equal(t, []string{p1}, pins["dup"], "identical pins dedupe")
	assert.Equal(t, []string{p1, p2}, pins["conflict"], "distinct pins both surface for the controller to refuse")
}

func TestPermittedSubagentModes_WorksWithPinnedEntry(t *testing.T) {
	pin := "sha256:" + strings.Repeat("ef", 32)
	s := &AgentClassSpec{
		Subagents:     []string{"helper@" + pin},
		SubagentModes: map[string][]string{"helper": {SubagentModeTask}},
	}
	assert.Equal(t, []string{SubagentModeSingleTurn, SubagentModeTask}, s.PermittedSubagentModes("helper"))
}
