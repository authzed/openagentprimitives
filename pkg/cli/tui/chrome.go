package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Step-rail glyphs. Exported as constants (not literals at call sites) so the
// tests and the renderer cannot disagree about what "done" looks like.
const (
	doneMark    = "✓"
	activeMark  = "▸"
	pendingMark = " "
)

// railMinWidth keeps the rail column readable when every label is short.
const railMinWidth = 14

// railGutter is the blank column count reserved between the rail column and
// the body column.
const railGutter = 3

// minBodyWidth is the smallest body column Chrome will draw the rail for.
// Below it, the rail is stealing so much of the terminal that there is no
// usable space left for the form itself, so the rail is dropped instead.
const minBodyWidth = 20

// narrowWidth is the terminal width below which there is no longer room for
// a rail column and a usable body column side by side, regardless of how
// short the step labels are. Below it, Chrome falls back to the same
// single-column layout used when there are no steps at all.
const narrowWidth = 40

// Step is one entry of the rail: the ID a screen is presented under, and the
// label the rail shows for it.
type Step struct {
	ID    string
	Label string
}

// Steps derives the rail from the screens themselves. Deriving rather than
// transcribing is what keeps the rail honest: a screen inserted into the
// wizard shows up in the rail, in its place, with no second list to update.
func Steps(screens []Screen) []Step {
	out := make([]Step, 0, len(screens))
	for _, s := range screens {
		out = append(out, Step{ID: s.ID(), Label: s.Label()})
	}
	return out
}

// Chrome is the frame drawn around a form in TTY mode: a title bar and a
// persistent step rail.
//
// It draws no keybinding footer. huh renders per-field help inside the form
// body — the keys shown there are the keys the focused field actually binds,
// and they change as the user moves between an input, a select and a confirm.
// A second, static line of hints could only repeat or contradict it.
type Chrome struct {
	title string
	steps []Step
	theme *Theme
}

// NewChrome builds the frame for a run. steps are the wizard's screens, in
// order; pass nil for a single-screen run, which omits the rail entirely.
func NewChrome(title string, steps []Step, th *Theme) *Chrome {
	return &Chrome{title: title, steps: steps, theme: th}
}

// StepIndex returns the rail position of the screen presented under id, or -1
// when the chrome carries no such step (which Render marks every step pending
// for).
//
// Position is the screen's place in the WIZARD, never a count of screens
// presented so far: a screen that branches away or does work without asking
// still holds its place in the rail, so resolving by ID is the only way the
// highlight can stay on the step the user is actually answering.
func (c *Chrome) StepIndex(id string) int {
	for i, s := range c.steps {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// Render composes the frame around body, marking step `active` as current.
// An out-of-range active index renders every step as pending rather than
// panicking — a driver bug must not take the user's terminal down. No line
// of the result exceeds the terminal width: the title is truncated to fit,
// and whenever showsRail says there isn't room for a rail column and a usable
// body column together, the rail is dropped entirely so neither one is
// squeezed into an overflowing line.
func (c *Chrome) Render(active int, body string) string {
	width := c.width()

	head := Truncate(c.theme.Render(c.theme.Title, c.title), width)

	if !c.showsRail(width) {
		return strings.Join([]string{head, "", body}, "\n")
	}

	// An active index outside the step range (a driver bug, not a user
	// mistake) must not partially mark steps done — normalize it to a
	// sentinel that satisfies neither branch below, so every step falls
	// through to pending rather than a too-large active marking all of
	// them "done".
	if active < 0 || active >= len(c.steps) {
		active = -1
	}

	railWidth, bodyWidth := c.layout(width)

	var rail strings.Builder
	for i, s := range c.steps {
		switch {
		case i < active:
			rail.WriteString(c.theme.Render(c.theme.RailDone, doneMark+" "+s.Label))
		case i == active:
			rail.WriteString(c.theme.Render(c.theme.RailActive, activeMark+" "+s.Label))
		default:
			rail.WriteString(c.theme.Render(c.theme.Rail, pendingMark+" "+s.Label))
		}
		if i < len(c.steps)-1 {
			rail.WriteString("\n")
		}
	}

	// The railGutter column is a real blank spacer between the rail and the
	// body: layout() already reserved railGutter cells out of the body budget,
	// so without an actual gutter column the reserved gap simply vanished and
	// the two columns sat railMinWidth-slack apart. Rendering it here spends the
	// reserved width and keeps this rail visually consistent with the checklist
	// composite (progress.compositeBlock), which reserves and renders the same
	// gutter.
	cols := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(railWidth).Render(rail.String()),
		lipgloss.NewStyle().Width(railGutter).Render(""),
		lipgloss.NewStyle().Width(bodyWidth).Render(body),
	)

	return strings.Join([]string{head, "", cols}, "\n")
}

// BodyWidth returns the column budget Render will give the form body at the
// chrome's current terminal width: the full width whenever showsRail is
// false (Render draws no rail in that case either), otherwise whatever is
// left after the rail column and its gutter.
//
// The TTY driver MUST size huh's own form to this, not the raw terminal
// width — lipgloss's Style.Width (used below to place the body) also sets
// huh's own wrap point, so sizing the form wider than the column Render
// then places it into re-wraps an already-wrapped layout a second time.
func (c *Chrome) BodyWidth() int {
	width := c.width()
	if !c.showsRail(width) {
		return width
	}
	_, bodyWidth := c.layout(width)
	return bodyWidth
}

// width returns the effective terminal width, falling back to defaultWidth
// when the theme's capabilities don't carry a usable one.
func (c *Chrome) width() int {
	if w := c.theme.Caps.Width; w > 0 {
		return w
	}
	return defaultWidth
}

// showsRail is the single decision point for whether Render draws the step
// rail at the given terminal width — Render and BodyWidth both call through
// it rather than each spelling out the condition, so they cannot disagree
// about when the rail exists. False when there are no steps, when the
// terminal is narrower than narrowWidth outright, or when the rail column a
// long step label demands (plus its gutter and a minimum usable body column)
// would not fit even in a wide-enough terminal — a single very long label
// must not push the joined line past width.
func (c *Chrome) showsRail(width int) bool {
	if len(c.steps) == 0 || width < narrowWidth {
		return false
	}
	return c.railWidth()+railGutter+minBodyWidth <= width
}

// railWidth computes the rail column width the step labels demand. The +2 is
// the per-step glyph and the space after it.
func (c *Chrome) railWidth() int {
	w := railMinWidth
	for _, s := range c.steps {
		if n := lipgloss.Width(s.Label) + 2; n > w {
			w = n
		}
	}
	return w
}

// layout computes the rail column width and the body column width left over
// at the given terminal width, for a width where showsRail(width) is true.
// Shared verbatim by Render and BodyWidth so the two cannot drift out of
// sync — BodyWidth is only useful to a caller if it is exactly what Render
// will go on to give the body.
func (c *Chrome) layout(width int) (railWidth, bodyWidth int) {
	railWidth = c.railWidth()
	bodyWidth = width - railWidth - railGutter
	if bodyWidth < 1 {
		bodyWidth = 1
	}
	return railWidth, bodyWidth
}

// Truncate hard-cuts s to at most width display columns rather than letting an
// unusually long title or footer hint overflow the terminal. It preserves
// embedded ANSI styling and is byte-identical to s when s already fits —
// required so color-disabled output stays byte-clean.
//
// Exported because it is the one piece of Chrome a long-lived full-screen model
// can reuse. Chrome itself takes its width from Theme.Caps, measured once when
// the run is built; a model that owns the alternate screen learns its width from
// tea.WindowSizeMsg and re-learns it on every resize, so it applies this rule at
// its own live width instead of borrowing Chrome's frozen one.
func Truncate(s string, width int) string {
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}
