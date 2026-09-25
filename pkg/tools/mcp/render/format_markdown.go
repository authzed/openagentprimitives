package render

import (
	"fmt"
	"strings"
)

func renderMarkdown(r *report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# MCP tool: `%s`\n\n", r.Tool)
	if r.Intent != "" {
		fmt.Fprintf(&b, "**Intent:** %s\n\n", r.Intent)
	}
	if r.Description != "" {
		fmt.Fprintf(&b, "**Description:** %s\n\n", r.Description)
	}

	if len(r.AllowedFields) > 0 {
		b.WriteString("## Allowed fields\n\n")
		for _, f := range r.AllowedFields {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		b.WriteByte('\n')
	}
	if len(r.SensitiveFields) > 0 {
		b.WriteString("## Sensitive fields\n\n")
		b.WriteString("Never set these — they are injected automatically.\n\n")
		for _, f := range r.SensitiveFields {
			fmt.Fprintf(&b, "- `%s`\n", f)
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
			fmt.Fprintf(&b, "- ✗ %s\n", h)
		}
		b.WriteByte('\n')
	}
	if len(r.WritesRels) > 0 {
		b.WriteString("## Writes relationships\n\n")
		for _, w := range r.WritesRels {
			fmt.Fprintf(&b, "- `%s`\n", w)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
