package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// columnGap is the space between table columns.
const columnGap = 2

// Table renders aligned columnar output for list and detail commands, themed
// identically to the wizards. Built by chaining Row calls, then Render.
type Table struct {
	theme   *Theme
	headers []string
	rows    [][]string
}

// NewTable starts a table with the given header cells.
func NewTable(th *Theme, headers ...string) *Table {
	return &Table{theme: th, headers: headers}
}

// Row appends a row. Rows shorter than the header are padded with empty cells;
// rows longer are kept whole, so an unexpected extra column is visible rather
// than silently dropped.
func (t *Table) Row(cells ...string) *Table {
	t.rows = append(t.rows, cells)
	return t
}

// Len reports how many rows have been appended, so a command that discovers
// its rows while building the table can still write the "no X found" line
// instead of a lone header. Rendering a header over nothing looks like a
// truncated result rather than an empty one.
func (t *Table) Len() int { return len(t.rows) }

// Render returns the formatted table.
func (t *Table) Render() string {
	cols := len(t.headers)
	for _, r := range t.rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return ""
	}

	widths := make([]int, cols)
	measure := func(cells []string) {
		for i, c := range cells {
			if n := lipgloss.Width(c); n > widths[i] {
				widths[i] = n
			}
		}
	}
	measure(t.headers)
	for _, r := range t.rows {
		measure(r)
	}

	var b strings.Builder
	writeRow := func(cells []string, style func(string) string) {
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(style(cell))
			if i < cols-1 {
				b.WriteString(strings.Repeat(" ", padAfter(widths[i], lipgloss.Width(cell))))
			}
		}
		b.WriteString("\n")
	}

	if len(t.headers) > 0 {
		writeRow(t.headers, func(s string) string { return t.theme.Render(t.theme.Label, s) })
	}
	for _, r := range t.rows {
		writeRow(r, func(s string) string { return t.theme.Render(t.theme.Value, s) })
	}
	return b.String()
}

// padAfter returns the spaces that follow a cell of cellWidth display columns
// in a column sized to colWidth: the room left over, plus the gap.
//
// The floor is the gap, not zero. colWidth is the maximum display width over
// every cell in the column, so a cell wider than its column means the two were
// measured with different units — the render is going to be ragged either way,
// and the choice is only what a too-wide cell does to its neighbour. Repeating a
// negative count panics, and a list command that crashes on one wide value is
// worse than one that prints a ragged row. Falling back to zero would be worse
// still in a different way: these tables are read by `awk '{print $1}'`, and two
// cells with nothing between them are one field, so a display bug would become a
// data bug for every script consuming the output.
func padAfter(colWidth, cellWidth int) int {
	return max(colWidth-cellWidth+columnGap, columnGap)
}
