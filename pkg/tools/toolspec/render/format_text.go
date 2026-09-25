package render

import (
	"fmt"
	"strings"
)

func renderText(r *report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Spec: %s\n", r.Spec)
	if r.Intent != "" {
		fmt.Fprintf(&b, "Intent: %s\n", r.Intent)
	}
	if r.GeneratedAt != "" {
		fmt.Fprintf(&b, "Source: %s  (%s)\n", r.Source, r.GeneratedAt)
	} else {
		fmt.Fprintf(&b, "Source: %s\n", r.Source)
	}
	b.WriteByte('\n')

	if len(r.Allows) > 0 {
		b.WriteString("WHAT YOU CAN DO:\n")
		for _, a := range r.Allows {
			fmt.Fprintf(&b, "  ✓ %-52s (%s)\n", a.Description, a.Subcommand)
		}
		b.WriteByte('\n')
	}
	if len(r.Constraints) > 0 {
		b.WriteString("CONSTRAINTS:\n")
		for _, c := range r.Constraints {
			fmt.Fprintf(&b, "  • %s\n", c.Message)
		}
		b.WriteByte('\n')
	}
	if len(r.HardStops) > 0 {
		b.WriteString("HARD STOPS:\n")
		for _, h := range r.HardStops {
			fmt.Fprintf(&b, "  ✗ %s\n", h.Description)
		}
		b.WriteByte('\n')
	}
	if len(r.Bounds) > 0 {
		b.WriteString("BOUNDS:\n")
		for _, h := range r.Bounds {
			fmt.Fprintf(&b, "  • %s\n", h.Description)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "NETWORK: %s\n", r.Network)
	fmt.Fprintf(&b, "CREDS:   %s\n", r.Creds)

	if len(r.Warnings) > 0 {
		b.WriteString("\n⚠  WARNINGS:\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "  • %s\n", w)
		}
	}
	if len(r.Unmatched) > 0 {
		b.WriteString("\n❌  I COULDN'T REPRESENT:\n")
		for _, u := range r.Unmatched {
			fmt.Fprintf(&b, "  • %q — %s\n", u.Request, u.Reason)
		}
	}
	if len(r.Excluded) > 0 {
		b.WriteString("\nDELIBERATELY EXCLUDED:\n")
		for _, e := range r.Excluded {
			fmt.Fprintf(&b, "  • %s — %s\n", e.Name, e.Reason)
		}
	}
	if len(r.Mismatches) > 0 {
		b.WriteString("\nCASES THAT STILL MISMATCH:\n")
		for _, m := range r.Mismatches {
			fmt.Fprintf(&b, "  • %s\n", m.Intent)
			if m.Reason != "" {
				fmt.Fprintf(&b, "    expected allow=%t, got allow=%t (%s)\n", m.ExpectAllow, m.Actual, m.Reason)
			} else {
				fmt.Fprintf(&b, "    expected allow=%t, got allow=%t\n", m.ExpectAllow, m.Actual)
			}
		}
	}
	return b.String()
}
