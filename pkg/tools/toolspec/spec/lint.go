package spec

import (
	"fmt"
	"strings"
)

// celSnippetMax bounds how much of a constraint's CEL text the linter
// echoes into a warning — the constraint index is the real locator, the
// snippet is only a hint, so a full CEL dump is unwarranted.
const celSnippetMax = 40

// LintConstraintMessages returns non-fatal authoring warnings, one per
// constraint that has a CEL expression but an empty (or whitespace-only)
// message. introspect_tool surfaces Constraint.Message as the
// agent-facing justification of a CEL gate; a CEL-without-message
// constraint degrades to a generic "an additional constraint applies"
// line, so the linter nudges the author to fill `message`.
//
// It is a lint, not a validation error: an empty message is permitted,
// and a constraint with no CEL has nothing to justify (skipped).
func LintConstraintMessages(constraints []Constraint) []string {
	var warnings []string
	for i, c := range constraints {
		if strings.TrimSpace(c.CEL) == "" {
			continue
		}
		if strings.TrimSpace(c.Message) != "" {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"constraint %d (%q) has a CEL expression but no message — agents "+
				"see a generic \"an additional constraint applies\" line instead "+
				"of a real justification; add a `message`.",
			i, celSnippet(c.CEL)))
	}
	return warnings
}

// celSnippet returns a short, single-line prefix of a CEL expression
// suitable for an author-facing hint.
func celSnippet(cel string) string {
	s := strings.TrimSpace(cel)
	if len(s) > celSnippetMax {
		return s[:celSnippetMax] + "…"
	}
	return s
}
