package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNoteBudgetsSubtractHuhsOwnFrame is the axis three of the four copies this
// replaces got right and one got wrong: huh renders a note's text inside a card
// whose frame takes two columns, so a budget equal to the body width passes
// lines that then wrap.
//
// It is TAUTOLOGICAL against noteBudgetFor — both sides compute
// BodyWidth() - noteFrameColumns — so it will keep agreeing with itself through
// any change to that constant. Kept only as documentation of the shape, since
// it is the cheapest place to read what the number is made of.
// TestNoteBudgetMatchesWhatHuhActuallyRenders below is what actually holds the
// constant to the library, by rendering through a real huh.Form; that is the
// test to look at when this one passes and notes still wrap.
func TestNoteBudgetsSubtractHuhsOwnFrame(t *testing.T) {
	th := standardNoteTheme()

	railed := NewChrome("", []Step{{ID: "s", Label: "Step"}}, th)
	assert.Equal(t, railed.BodyWidth()-noteFrameColumns, RailedNoteBudget().Columns())

	railless := NewChrome("", nil, th)
	assert.Equal(t, railless.BodyWidth()-noteFrameColumns, RaillessNoteBudget().Columns())
}

// TestARailCostsColumns pins the other axis. The two budgets are genuinely
// different numbers, so a caller picking the wrong one is not a stylistic
// choice — it either cuts text that would have fit or passes text that wraps.
func TestARailCostsColumns(t *testing.T) {
	assert.Less(t, RailedNoteBudget().Columns(), RaillessNoteBudget().Columns(),
		"a rail takes columns from the body, so its note budget must be smaller")
	assert.Greater(t, RailedNoteBudget().Columns(), 0)
}

// TestNoteBudgetMatchesWhatHuhActuallyRenders is the check the four hand-rolled
// copies could not make: it renders a note of exactly the budget width through
// huh itself and requires that it come back on one line.
//
// This is what makes the arithmetic above a measurement rather than a belief.
// If huh changes its card frame, this fails — where an assertion against our
// own constant would keep agreeing with itself.
func TestNoteBudgetMatchesWhatHuhActuallyRenders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget NoteBudget
		chrome *Chrome
	}{
		{name: "railed", budget: RailedNoteBudget(), chrome: NewChrome("", []Step{{ID: "s", Label: "Step"}}, standardNoteTheme())},
		{name: "railless", budget: RaillessNoteBudget(), chrome: NewChrome("", nil, standardNoteTheme())},
	} {
		t.Run(tc.name+": a line at the budget renders unwrapped", func(t *testing.T) {
			line := strings.Repeat("x", tc.budget.Columns())

			form := huh.NewForm(huh.NewGroup(huh.NewNote().Title(line))).
				WithTheme(huh.ThemeBase()).
				WithWidth(tc.chrome.BodyWidth())
			// Init builds the current group; without it View renders the
			// form's pre-run frame and no field at all.
			_ = form.Init()

			require.Contains(t, form.View(), line, "huh wrapped a line the budget said would fit")
		})
	}
}

func TestNoteBudgetOverflows(t *testing.T) {
	b := RailedNoteBudget()
	short := strings.Repeat("a", b.Columns())
	long := strings.Repeat("b", b.Columns()+1)

	assert.Empty(t, b.Overflows(short), "a line exactly at the budget fits")
	assert.Equal(t, []string{long}, b.Overflows(short+"\n"+long+"\n"+short),
		"only the over-wide line is reported, and it is reported whole")
	assert.Empty(t, b.Overflows(""), "an empty block overflows nothing")
}

// TestNoteBudgetMeasuresDisplayColumnsNotRunes pins the third discrepancy
// between the copies: one counted runes. A CJK ideograph is one rune and two
// columns, so a rune count passes a line at twice the width it draws.
func TestNoteBudgetMeasuresDisplayColumnsNotRunes(t *testing.T) {
	b := RailedNoteBudget()
	// Half the budget in ideographs is exactly the budget in columns; one more
	// is over. Both are well under the budget counted in runes.
	line := strings.Repeat("字", b.Columns()/2+1)
	require.Less(t, len([]rune(line)), b.Columns(), "this fixture must fit if runes were counted, or it tests nothing")
	require.Greater(t, lipgloss.Width(line), b.Columns())

	assert.NotEmpty(t, b.Overflows(line), "a wide-glyph line over the budget must be reported")
}

func TestNoteBudgetElide(t *testing.T) {
	b := RailedNoteBudget()
	cases := []struct {
		name string
		in   string
		want func(t *testing.T, got string)
	}{
		{
			name: "a line that fits is returned unchanged",
			in:   "short enough",
			want: func(t *testing.T, got string) { assert.Equal(t, "short enough", got) },
		},
		{
			name: "an over-long line is cut to the budget and marked",
			in:   strings.Repeat("x", b.Columns()+40),
			want: func(t *testing.T, got string) {
				assert.LessOrEqual(t, lipgloss.Width(got), b.Columns())
				assert.True(t, strings.HasSuffix(got, "…"), "a cut must be marked, or it reads as the whole value")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.want(t, b.Elide(tc.in)) })
	}
}

// TestNoteBudgetElideDegradesRatherThanPanicking covers the budget a very
// narrow chrome could produce. Nothing constructs one today, but Elide is
// exported and does arithmetic on its receiver.
func TestNoteBudgetElideDegradesRatherThanPanicking(t *testing.T) {
	for _, b := range []NoteBudget{0, 1, -5} {
		assert.NotPanics(t, func() {
			assert.Equal(t, "…", b.Elide("anything at all"))
		})
	}
}

func TestNoteBudgetFit(t *testing.T) {
	b := RaillessNoteBudget()
	long := strings.Repeat("y", b.Columns()+10)

	got := b.Fit("  first line  \n" + long + "\n\nlast  ")
	assert.Empty(t, b.Overflows(got), "every line of a fitted block must fit")
	assert.Equal(t, "first line", strings.Split(got, "\n")[0], "trailing whitespace is trimmed per line")
	assert.Equal(t, "", b.Fit("   "), "a blank block stays blank rather than becoming an ellipsis")
}
