package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTableRendersHeadersAndAlignedColumns(t *testing.T) {
	out := NewTable(NewTheme(Caps{Width: 80}), "NAME", "KIND").
		Row("demo-channel", "slack").
		Row("another-demo-channel", "fake").
		Render()

	lines := splitNonEmpty(out)
	require.Len(t, lines, 3, "header plus two rows")
	assert.Contains(t, lines[0], "NAME")
	assert.Contains(t, lines[0], "KIND")
	assert.Equal(t, displayColumnOf(t, lines[1], "slack"), displayColumnOf(t, lines[2], "fake"),
		"the second column must align across rows of differing first-column width")
}

func TestTableWithNoRowsRendersHeadersOnly(t *testing.T) {
	out := NewTable(NewTheme(Caps{Width: 80}), "NAME").Render()
	assert.Len(t, splitNonEmpty(out), 1)
}

func TestTablePadsShortRowsToTheHeaderColumnCount(t *testing.T) {
	// Pin the whole rendering: Caps leaves Color false, so Theme.Render
	// returns each cell verbatim and the output is fully deterministic. An
	// implementation that stopped at the row's own cell count — dropping the
	// two trailing pads a three-column header demands — still contains
	// "only-one", so only an exact match distinguishes it.
	out := NewTable(NewTheme(Caps{Width: 80}), "A", "B", "C").Row("only-one").Render()

	assert.Equal(t, "A         B  C\nonly-one     \n", out,
		"a row shorter than the header renders as empty trailing columns, padded to the header's widths")
}

func TestTableIsPlainWhenColorDisabled(t *testing.T) {
	out := NewTable(NewTheme(Caps{Color: false, Width: 80}), "NAME").Row("demo-channel").Render()
	assert.False(t, hasANSI(out))
}

// TestTableKeepsRowsAlignedWhenOneCellIsAWideRune guards the rendered shape: a
// wide character in a non-final column ("🎯": 4 bytes, 1 rune, 2 display
// columns) must not move the next column for the rows around it. A writeRow that
// dropped a pad, padded by a constant, or padded from the wrong row breaks this.
//
// It deliberately does NOT claim to prove the width measure. Render pads with
// `widths[i] - lipgloss.Width(cell) + gap`, so every row starts a column at
// `widths[i] + gap` whatever unit widths[i] was counted in — rows stay aligned
// with each other even under a byte or rune measure, and an alignment assertion
// cannot see the difference. That claim belongs to the test below.
func TestTableKeepsRowsAlignedWhenOneCellIsAWideRune(t *testing.T) {
	out := NewTable(NewTheme(Caps{Width: 80, Color: false}), "A", "B", "C").
		Row("🎯", "mid", "end").
		Row("abc", "xyz", "123").
		Render()

	lines := splitNonEmpty(out)
	require.Len(t, lines, 3, "header plus two data rows")

	assert.Equal(t, displayColumnOf(t, lines[1], "mid"), displayColumnOf(t, lines[2], "xyz"),
		"column B must start at the same display column in every row")
}

// TestTableSizesAColumnInDisplayColumnsNotBytesOrRunes is the measure test:
// it renders the same table twice, changing only whether the widest cell in
// column A is wide-rune text or ASCII of the *same display width*, and requires
// the layout to come out identical.
//
// Stated as a comparison rather than a hardcoded column number because the
// arithmetic hides a trap — see the alignment test above. What a wrong measure
// actually does is size the column to the wrong number of columns, and that is
// visible only against a reference of known display width.
//
// "一二三" is 6 display columns, 9 bytes, 3 runes; "abcdef" is 6 display columns,
// 6 bytes, 6 runes. The cell measured is the *narrow* row's second column, not
// the wide row's: padAfter floors at the gap, so the widest cell in a column
// always gets exactly the gap and cannot report an undercount. Its neighbour can.
// Under a byte measure column A is sized 9 and the narrow row's B shifts right;
// under a rune measure it is sized 3 and B shifts left.
func TestTableSizesAColumnInDisplayColumnsNotBytesOrRunes(t *testing.T) {
	render := func(widest string) int {
		out := NewTable(NewTheme(Caps{Width: 80, Color: false}), "A", "B").
			Row(widest, "mid").
			Row("ab", "xyz").
			Render()
		lines := splitNonEmpty(out)
		require.Len(t, lines, 3, "header plus two data rows")
		return displayColumnOf(t, lines[2], "xyz")
	}

	assert.Equal(t, render("abcdef"), render("一二三"),
		"a cell 6 display columns wide must size its column the same whether it is wide-rune or ASCII")
}

// TestPadAfterFloorsAtTheGapForACellWiderThanItsColumn pins the clamp.
//
// Render measures a column as the maximum display width over its own cells, so
// a cell can only exceed that maximum if the measure and the pad disagree —
// unreachable through NewTable/Row/Render today, which is why the guard is
// exercised here rather than through a rendered table. What it rules out is the
// crash: strings.Repeat panics on a negative count, so before the clamp a single
// over-wide cell took the whole command down.
func TestPadAfterFloorsAtTheGapForACellWiderThanItsColumn(t *testing.T) {
	cases := []struct {
		name     string
		colWidth int
		cell     int
		want     int
	}{
		{name: "cell narrower than its column: leftover room plus the gap", colWidth: 10, cell: 4, want: 8},
		{name: "cell exactly its column's width: the gap alone", colWidth: 10, cell: 10, want: columnGap},
		{name: "cell one column too wide: the gap, not one less", colWidth: 10, cell: 11, want: columnGap},
		{name: "cell wider than its column by more than the gap: the gap, not a negative count", colWidth: 3, cell: 12, want: columnGap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, padAfter(tc.colWidth, tc.cell))
			assert.GreaterOrEqual(t, padAfter(tc.colWidth, tc.cell), columnGap,
				"neighbouring cells must always stay separable by whitespace")
		})
	}
}

// TestTableLenCountsRowsNotHeaders pins the accessor the "no X found" branch
// keys off. A command that discovers its rows across several list calls cannot
// ask its inputs whether the result is empty — it can only ask the table — and
// a Len that counted the header row would make every empty result look
// populated, which is how a lone header ships instead of an empty-state line.
func TestTableLenCountsRowsNotHeaders(t *testing.T) {
	tbl := NewTable(NewTheme(Caps{}), "NAME", "AGE")
	assert.Equal(t, 0, tbl.Len(), "a table with headers and no rows is empty")

	tbl.Row("demo-agent", "3h")
	assert.Equal(t, 1, tbl.Len())
	tbl.Row("other-agent", "1d")
	assert.Equal(t, 2, tbl.Len())
}
