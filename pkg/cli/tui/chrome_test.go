package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// steps builds a rail whose IDs are the lowercased labels, for tests that
// care only about what the rail renders.
func steps(labels ...string) []Step {
	out := make([]Step, 0, len(labels))
	for _, l := range labels {
		out = append(out, Step{ID: strings.ToLower(l), Label: l})
	}
	return out
}

func TestChromeRenderMarksStepsByPosition(t *testing.T) {
	th := NewTheme(Caps{Width: 80})
	c := NewChrome("oap · channel create · slack", steps("Agent", "Tokens", "Review"), th)

	got := c.Render(1, "BODY")

	require.Contains(t, got, "oap · channel create · slack", "title bar must be present")
	require.Contains(t, got, "BODY", "the form body must be embedded")

	assert.Contains(t, got, doneMark+" Agent", "an earlier step is done")
	assert.Contains(t, got, activeMark+" Tokens", "the current step is active")
	assert.Contains(t, got, pendingMark+" Review", "a later step is pending")
}

func TestChromeStepIndexResolvesPositionByID(t *testing.T) {
	c := NewChrome("t", steps("Agent", "Tokens", "Review"), NewTheme(Caps{Width: 80}))

	cases := []struct {
		name string
		id   string
		want int
	}{
		{name: "first step: index 0", id: "agent", want: 0},
		{name: "middle step: index 1", id: "tokens", want: 1},
		{name: "last step: index 2", id: "review", want: 2},
		{name: "unknown ID: -1, the sentinel Render marks every step pending for", id: "nope", want: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, c.StepIndex(tc.id))
		})
	}
}

func TestStepsDerivesTheRailFromTheScreens(t *testing.T) {
	got := Steps([]Screen{
		&fakeScreen{id: "agent", label: "Agent"},
		&fakeScreen{id: "slack-app", label: "Slack app"},
	})

	assert.Equal(t, []Step{
		{ID: "agent", Label: "Agent"},
		{ID: "slack-app", Label: "Slack app"},
	}, got, "the rail is the screens' own IDs and labels, in wizard order")
}

func TestChromeRenderFirstAndLastStep(t *testing.T) {
	th := NewTheme(Caps{Width: 80})
	c := NewChrome("t", steps("One", "Two"), th)

	first := c.Render(0, "B")
	assert.Contains(t, first, activeMark+" One")
	assert.Contains(t, first, pendingMark+" Two")

	last := c.Render(1, "B")
	assert.Contains(t, last, doneMark+" One")
	assert.Contains(t, last, activeMark+" Two")
}

func TestChromeRenderOutOfRangeActiveDoesNotPanic(t *testing.T) {
	c := NewChrome("t", steps("One"), NewTheme(Caps{Width: 80}))

	// Not panicking is necessary but not sufficient: the global constraint is
	// "render every step as pending", not merely "survive". -1 is the exact
	// sentinel the implementation normalizes every out-of-range active to, so
	// it is the case a future "clamp active to 0" refactor would silently
	// break — a clamp would mark step 0 *active*, not pending, with this
	// suite still green if only NotPanics were asserted here.
	cases := []struct {
		name   string
		active int
	}{
		{"negative: renders One pending, not active", -1},
		{"equal to len(steps): renders One pending, not done", len(c.steps)},
		{"far past len(steps): renders One pending, not done", 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			assert.NotPanics(t, func() { got = c.Render(tc.active, "B") })
			assert.Contains(t, got, pendingMark+" One", "out-of-range active marks the only step pending")
			assert.NotContains(t, got, doneMark, "no step may render as done when active is out of range")
			assert.NotContains(t, got, activeMark, "no step may render as active when active is out of range")
		})
	}
}

func TestChromeRenderWithNoStepsOmitsTheRail(t *testing.T) {
	c := NewChrome("t", nil, NewTheme(Caps{Width: 80}))
	got := c.Render(0, "BODY")

	// Pin the whole frame exactly rather than checking for the absence of a
	// glyph: Caps{Width: 80} leaves Color false, so Theme.Render returns text
	// verbatim and the frame is fully deterministic. An exact match also
	// can't be satisfied by an implementation that renders no pending rows at
	// all — a weaker "does not contain X" assertion could be. It equally pins
	// that no keybinding footer is drawn: huh's own per-field help, inside the
	// body, is the only place keys are advertised.
	assert.Equal(t, "t\n\nBODY", got, "no steps: title and body and nothing else")
}

func TestChromeRenderIsPlainWhenColorDisabled(t *testing.T) {
	c := NewChrome("t", steps("One"), NewTheme(Caps{Color: false, Width: 80}))
	assert.False(t, hasANSI(c.Render(0, "B")))
}

func TestChromeStepColumnWidthTracksLongestLabel(t *testing.T) {
	rail := steps("A", "A much longer step label")
	for _, width := range []int{20, 30, 40, 80, 100} {
		t.Run(fmt.Sprintf("width=%d: no line exceeds the terminal width", width), func(t *testing.T) {
			c := NewChrome("t", rail, NewTheme(Caps{Width: width}))
			got := c.Render(0, "B")

			longest := 0
			for _, line := range strings.Split(got, "\n") {
				if w := lipgloss.Width(line); w > longest {
					longest = w
				}
				assert.LessOrEqual(t, lipgloss.Width(line), width, "no line may exceed the terminal width")
			}
			t.Logf("width=%d longest rendered line=%d", width, longest)
		})
	}
}

func TestChromeRenderRailWidthAloneNeverExceedsTerminalWidth(t *testing.T) {
	// A single step label long enough that the rail column its own text
	// demands exceeds the terminal width, at a width (40) that is not
	// "narrow" by a fixed threshold alone — the hole a narrowWidth-only
	// showsRail check cannot see, since a short-labeled chrome renders fine
	// at this same width (see TestChromeStepColumnWidthTracksLongestLabel).
	c := NewChrome("t", steps("Configure the upstream credential source"), NewTheme(Caps{Width: 40}))
	got := c.Render(0, "B")
	for _, line := range strings.Split(got, "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 40,
			"a step label wider than the terminal must not push a line past the terminal width")
	}
}

func TestChromeBodyWidthMatchesRenderedBodyColumn(t *testing.T) {
	rail := steps("Agent", "Tokens", "Review")

	t.Run("wide terminal: BodyWidth is the exact column budget the rail-bearing render enforces", func(t *testing.T) {
		c := NewChrome("t", rail, NewTheme(Caps{Width: 80}))
		bw := c.BodyWidth()
		require.Greater(t, bw, 0, "BodyWidth must be usable")

		// A body sized to exactly BodyWidth() must fit on one line...
		fits := strings.Repeat("x", bw)
		got := c.Render(0, fits)
		assert.Equal(t, 1, countLinesContaining(got, "x"), "a body sized to exactly BodyWidth() renders on one line")
		for _, line := range strings.Split(got, "\n") {
			assert.LessOrEqual(t, lipgloss.Width(line), 80, "no line may exceed the terminal width")
		}

		// ...and one column more must NOT, proving BodyWidth() is the exact
		// boundary Render enforces rather than a merely-safe underestimate.
		overflow := strings.Repeat("x", bw+1)
		got2 := c.Render(0, overflow)
		assert.Greater(t, countLinesContaining(got2, "x"), 1, "a body one column past BodyWidth() wraps onto another line")
	})

	t.Run("narrow terminal: rail is dropped, so BodyWidth is the full terminal width", func(t *testing.T) {
		c := NewChrome("t", rail, NewTheme(Caps{Width: 30}))
		assert.Equal(t, 30, c.BodyWidth(), "no rail column means the body gets the whole terminal width")
	})
}

// countLinesContaining reports how many lines of s contain substr — used to
// tell whether a body rendered on one line or wrapped onto more than one.
func countLinesContaining(s, substr string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}
