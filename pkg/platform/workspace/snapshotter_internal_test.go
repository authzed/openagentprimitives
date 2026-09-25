package workspace

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSanitizeQualifier pins the sanitization contract: lowercase,
// [a-z0-9-] only (other runes dropped), bounded to 40 chars. The output
// is embedded in Job names, label-adjacent metadata, and on-disk path
// segments, so it must never carry apiserver-invalid or path-unsafe runes.
func TestSanitizeQualifier(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "already-clean passes through", in: "parent-fk1", want: "parent-fk1"},
		{name: "uppercase lowered", in: "Parent-FK1", want: "parent-fk1"},
		{name: "invalid runes dropped", in: "a.b_c/d e:f", want: "abcdef"},
		{name: "all-invalid sanitizes to empty", in: "!!!///:::", want: ""},
		{name: "unicode dropped", in: "café-λ-1", want: "caf--1"},
		{name: "exactly 40 chars passes through untruncated", in: strings.Repeat("a", 40), want: strings.Repeat("a", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizeQualifier(tc.in))
		})
	}
}

// TestSanitizeQualifier_LongInputsKeepDistinguishingTail is the regression
// test for the front-truncation collision: real session names put the fork
// discriminator at the END (…-fk<6hex>), and takeover-of-takeover names
// reach 40 chars — plain prefix truncation would collapse two distinct
// forks onto one snapshot identity. Over-length qualifiers must instead
// fold a hash of the FULL sanitized input into the output.
func TestSanitizeQualifier_LongInputsKeepDistinguishingTail(t *testing.T) {
	// Two 49-char qualifiers sharing a 40-char prefix, differing only in
	// the trailing fork discriminator (the exact shape front-truncation
	// used to erase).
	prefix := "slack-example-c6fd53d5-icf4f456-ic0c06c5" // 40 chars
	q1 := prefix + "-fk1a2b3c"
	q2 := prefix + "-fk9e8d7f"

	s1 := sanitizeQualifier(q1)
	s2 := sanitizeQualifier(q2)

	assert.NotEqual(t, s1, s2,
		"long qualifiers differing only at the tail must sanitize distinctly")

	for _, s := range []string{s1, s2} {
		assert.LessOrEqual(t, len(s), 40, "output must stay within the 40-char bound")
		assert.Regexp(t, "^[a-z0-9-]+$", s, "output must stay [a-z0-9-]")
		assert.True(t, strings.HasPrefix(s, prefix[:24]),
			"output keeps a readable 24-char prefix of the input")
	}

	// Deterministic: the same input always yields the same output (Job
	// names and paths must be stable across reconcile passes).
	assert.Equal(t, s1, sanitizeQualifier(q1))

	// Hashing keys off the FULL sanitized form, so inputs that sanitize
	// identically stay identical.
	assert.Equal(t, s1, sanitizeQualifier(strings.ToUpper(q1)))
}

// TestTurnSegment_QualifierFolding pins the segment shapes: non-qualified
// output is byte-identical to the historical format (JIT snapshot names and
// on-disk paths must not shift under existing sessions), and a qualifier
// folds in as its own segment.
func TestTurnSegment_QualifierFolding(t *testing.T) {
	cases := []struct {
		name string
		h    SnapshotHandle
		want string
	}{
		{name: "JIT (non-negative, no qualifier) — historical format",
			h: SnapshotHandle{TurnIndex: 3, Sequence: 0}, want: "000003-000"},
		{name: "ad-hoc, no qualifier — historical format",
			h: SnapshotHandle{TurnIndex: -1, Sequence: 0}, want: "adhoc-000"},
		{name: "ad-hoc, qualified — adhoc-<qualifier>-NNN",
			h: SnapshotHandle{TurnIndex: -1, Sequence: 0, Qualifier: "parent-fk1"}, want: "adhoc-parent-fk1-000"},
		{name: "ad-hoc, qualifier sanitized",
			h: SnapshotHandle{TurnIndex: -1, Sequence: 0, Qualifier: "Parent.FK1"}, want: "adhoc-parentfk1-000"},
		{name: "ad-hoc, all-invalid qualifier treated as absent",
			h: SnapshotHandle{TurnIndex: -1, Sequence: 0, Qualifier: "///"}, want: "adhoc-000"},
		{name: "non-negative with qualifier folds it in too (never silently dropped)",
			h: SnapshotHandle{TurnIndex: 3, Sequence: 1, Qualifier: "q1"}, want: "000003-q1-001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.h.turnSegment())
		})
	}
}
