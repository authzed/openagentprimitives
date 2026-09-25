package tui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// summaryMark prefixes every summary line. Each note is by definition a
// completed decision, so there is no pending/failed variant here.
const summaryMark = "✓"

// RenderSummary writes the post-run summary — the block that stays in
// scrollback once the run is over, which bubbletea's own frame does not
// (huh renders an empty view while quitting, in either buffer). Labels are
// padded so values form a column, which is what makes a ten-line summary
// scannable.
//
// Padding is measured in display columns, the unit the terminal aligns in:
// a CJK ideograph or an emoji is one rune and two columns, so a rune count
// would leave every wide label's value one column short of its neighbours.
//
// Writing nothing for an empty note list is deliberate: a run that decided
// nothing must not leave stray blank lines behind.
func RenderSummary(w io.Writer, th *Theme, notes []Note) error {
	if len(notes) == 0 {
		return nil
	}

	width := 0
	for _, n := range notes {
		if c := lipgloss.Width(n.Label); c > width {
			width = c
		}
	}

	var b strings.Builder
	for _, n := range notes {
		pad := strings.Repeat(" ", width-lipgloss.Width(n.Label))
		fmt.Fprintf(&b, "  %s %s%s   %s\n",
			th.Render(th.Success, summaryMark),
			th.Render(th.Label, n.Label),
			pad,
			th.Render(th.Value, n.Value),
		)
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("tui: write summary: %w", err)
	}
	return nil
}
