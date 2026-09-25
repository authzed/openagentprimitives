package render

import (
	"fmt"
	"strings"
)

func renderText(r *report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "MCP tool: %s\n", r.Tool)
	if r.Intent != "" {
		fmt.Fprintf(&b, "Intent: %s\n", r.Intent)
	}
	if r.Description != "" {
		fmt.Fprintf(&b, "Description: %s\n", r.Description)
	}
	b.WriteByte('\n')

	if len(r.AllowedFields) > 0 {
		fmt.Fprintf(&b, "ALLOWED FIELDS: %s\n", strings.Join(r.AllowedFields, ", "))
	}
	if len(r.SensitiveFields) > 0 {
		fmt.Fprintf(&b, "SENSITIVE FIELDS (never set these — injected automatically): %s\n",
			strings.Join(r.SensitiveFields, ", "))
	}
	if len(r.Constraints) > 0 {
		b.WriteString("\nCONSTRAINTS:\n")
		for _, c := range r.Constraints {
			fmt.Fprintf(&b, "  • %s\n", c.Message)
		}
	}
	if len(r.HardStops) > 0 {
		b.WriteString("\nHARD STOPS:\n")
		for _, h := range r.HardStops {
			fmt.Fprintf(&b, "  ✗ %s\n", h)
		}
	}
	if len(r.WritesRels) > 0 {
		b.WriteString("\nWRITES RELATIONSHIPS:\n")
		for _, w := range r.WritesRels {
			fmt.Fprintf(&b, "  • %s\n", w)
		}
	}
	return b.String()
}
