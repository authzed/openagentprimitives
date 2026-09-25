// Package mdxutil holds the small text helpers the docs generators (clidocs,
// crddocs) share for emitting MDX safely: escaping prose so it can't open a JSX
// tag/expression, and rendering strings into frontmatter and table cells.
package mdxutil

import (
	"fmt"
	"strings"
)

// Frontmatter renders the `export const meta = {…}` block every generated doc
// page begins with. Values are rendered as safe single-quoted JS strings.
func Frontmatter(title, section, group string, order int, description string) string {
	var b strings.Builder
	b.WriteString("export const meta = {\n")
	fmt.Fprintf(&b, "  title: %s,\n", JSString(title))
	fmt.Fprintf(&b, "  section: %s,\n", JSString(section))
	fmt.Fprintf(&b, "  group: %s,\n", JSString(group))
	fmt.Fprintf(&b, "  order: %d,\n", order)
	fmt.Fprintf(&b, "  description: %s,\n", JSString(description))
	b.WriteString("}\n\n")
	return b.String()
}

// EscapeMDX makes prose safe to drop into an MDX body: the characters that would
// otherwise open a JSX tag ('<') or expression ('{') are entity-escaped. Apply it
// only to prose — command usages, field paths, and flags belong in code
// spans/fences, which MDX treats as literal and must NOT be escaped.
func EscapeMDX(s string) string {
	return strings.NewReplacer(
		"<", "&lt;",
		">", "&gt;",
		"{", "&#123;",
		"}", "&#125;",
	).Replace(s)
}

// FirstLine trims to the first line and surrounding space (frontmatter
// descriptions are single-line).
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// TableText makes a string safe for a single markdown table cell: first line
// only, MDX-escaped, with the cell separator escaped.
func TableText(s string) string {
	return strings.ReplaceAll(EscapeMDX(FirstLine(s)), "|", "\\|")
}

// CellText makes a possibly-multi-line string safe for a table cell while keeping
// its whole content: newlines collapse to spaces, then MDX-escape + escape the
// cell separator.
func CellText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(EscapeMDX(s), "|", "\\|")
}

// JSString renders a Go string as a single-quoted JS string literal for a
// frontmatter value: newlines collapse, the quote and backslash are escaped, and
// it is truncated to keep descriptions single-line.
func JSString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "'", "\\'")
	if len(s) > 200 {
		s = strings.TrimSpace(s[:200]) + "…"
	}
	return "'" + s + "'"
}
