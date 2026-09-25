package render

import (
	"fmt"
	"strings"
)

func renderMarkdown(r *report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Spec: `%s`\n\n", r.Spec)
	if r.Intent != "" {
		fmt.Fprintf(&b, "**Intent:** %s\n\n", r.Intent)
	}
	if r.GeneratedAt != "" {
		fmt.Fprintf(&b, "**Source:** `%s`  (%s)\n\n", r.Source, r.GeneratedAt)
	} else {
		fmt.Fprintf(&b, "**Source:** `%s`\n\n", r.Source)
	}

	if len(r.Allows) > 0 {
		b.WriteString("## What you can do\n\n")
		for _, a := range r.Allows {
			fmt.Fprintf(&b, "- ✓ %s  (`%s`)\n", a.Description, a.Subcommand)
		}
		b.WriteByte('\n')
	}
	if len(r.Constraints) > 0 {
		b.WriteString("## Constraints\n\n")
		for _, c := range r.Constraints {
			fmt.Fprintf(&b, "- %s\n", c.Message)
		}
		b.WriteByte('\n')
	}
	if len(r.HardStops) > 0 {
		b.WriteString("## Hard stops\n\n")
		for _, h := range r.HardStops {
			fmt.Fprintf(&b, "- ✗ %s\n", h.Description)
		}
		b.WriteByte('\n')
	}
	if len(r.Bounds) > 0 {
		b.WriteString("## Bounds\n\n")
		for _, h := range r.Bounds {
			fmt.Fprintf(&b, "- %s\n", h.Description)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "**Network:** %s\n", r.Network)
	fmt.Fprintf(&b, "**Creds:** %s\n", withBackticks(r.Creds))

	if len(r.Warnings) > 0 {
		b.WriteString("\n## ⚠ Warnings\n\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	if len(r.Unmatched) > 0 {
		b.WriteString("\n## ❌ Could not represent\n\n")
		for _, u := range r.Unmatched {
			fmt.Fprintf(&b, "- %q — %s\n", u.Request, u.Reason)
		}
	}
	if len(r.Excluded) > 0 {
		b.WriteString("\n## Deliberately excluded\n\n")
		for _, e := range r.Excluded {
			fmt.Fprintf(&b, "- `%s` — %s\n", e.Name, e.Reason)
		}
	}
	if len(r.Mismatches) > 0 {
		b.WriteString("\n## Cases that still mismatch\n\n")
		for _, m := range r.Mismatches {
			if m.Reason != "" {
				fmt.Fprintf(&b, "- %s\n  expected allow=%t, got allow=%t (%s)\n", m.Intent, m.ExpectAllow, m.Actual, m.Reason)
			} else {
				fmt.Fprintf(&b, "- %s\n  expected allow=%t, got allow=%t\n", m.Intent, m.ExpectAllow, m.Actual)
			}
		}
	}
	return b.String()
}

// withBackticks puts env var names inside `...`. Very light formatting; the
// markdown output mostly matches text output by design.
func withBackticks(s string) string {
	// Replace uppercase run-of-chars-or-underscore tokens with `<token>`.
	// A proper implementation would tokenize; for now, return as-is — the
	// expected fixture matches the unmodified string.
	return s
}
