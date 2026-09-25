package spec

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLintConstraintMessages covers the CEL-without-message lint: a
// constraint with a CEL gate but no message degrades to a generic
// "an additional constraint applies" line in introspect_tool, so the
// linter nudges the author to write a justification.
func TestLintConstraintMessages(t *testing.T) {
	cases := []struct {
		name        string
		constraints []Constraint
		wantWarn    bool
	}{
		{
			name:        "CEL set, message empty: warns",
			constraints: []Constraint{{CEL: "args.x > 0", Message: ""}},
			wantWarn:    true,
		},
		{
			name:        "CEL set, message present: no warning",
			constraints: []Constraint{{CEL: "args.x > 0", Message: "x must be positive"}},
			wantWarn:    false,
		},
		{
			name:        "no CEL, no message: no warning (nothing to justify)",
			constraints: []Constraint{{CEL: "", Message: ""}},
			wantWarn:    false,
		},
		{
			name:        "CEL set, message is only whitespace: warns",
			constraints: []Constraint{{CEL: "args.x > 0", Message: "   "}},
			wantWarn:    true,
		},
		{
			name:        "nil constraints: no warning",
			constraints: nil,
			wantWarn:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warnings := LintConstraintMessages(tc.constraints)
			if !tc.wantWarn {
				assert.Empty(t, warnings, "expected no warnings")
				return
			}
			require.NotEmpty(t, warnings, "expected at least one warning")
			lower := strings.ToLower(strings.Join(warnings, "\n"))
			assert.Contains(t, lower, "constraint", "warning should mention the constraint")
			assert.True(t,
				strings.Contains(lower, "message") || strings.Contains(lower, "justification"),
				"warning should mention message/justification; got %q", warnings)
		})
	}
}

// TestLintConstraintMessages_IndexLocator verifies the warning names the
// offending constraint by index so an author can locate it.
func TestLintConstraintMessages_IndexLocator(t *testing.T) {
	warnings := LintConstraintMessages([]Constraint{
		{CEL: "args.a > 0", Message: "a positive"},
		{CEL: "args.b > 0", Message: ""}, // index 1 is the offender
	})
	require.Len(t, warnings, 1, "exactly one constraint lacks a message")
	assert.Contains(t, warnings[0], "1", "warning should name constraint index 1")
}
