package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// noteFrameColumns is what huh's own field frame takes out of the form width
// before a note's first character.
//
// huh v0.7.0 renders a note's text at `width - Card.GetHorizontalFrameSize()`
// (field_note.go:222), and that frame is two columns in BOTH states: the
// focused card sets PaddingLeft(1) with BorderLeft(true) (theme.go:98), and the
// blurred card copies it replacing the border with HiddenBorder (theme.go:115),
// which still occupies its column. So the two columns come out whether or not
// the note is the active field, and a budget that skips them is optimistic by
// exactly the amount that makes a full-width line wrap.
const noteFrameColumns = 2

// noteFloorWidth is the terminal width every budget here is measured at.
//
// A FLOOR rather than the live terminal width, and deliberately so: a block
// that fits at eighty columns fits every wider terminal, while measuring
// against the live width would make what a wizard SAYS depend on how the window
// happened to be sized — so the same run would show an approver different text
// on a laptop and on a projector. It is optimistic on a narrower terminal,
// where huh will wrap what this budget passed; that is the accepted direction
// to be wrong in, since the alternative is cutting text nobody needed cut.
const noteFloorWidth = 80

// NoteBudget is the number of display columns one huh note has for its text.
//
// It exists because that number was derived four separate times in this
// repository, differing on two axes — whether the frame above was subtracted,
// and whether the chrome measured drew a step rail — so two callers rendering
// the same block disagreed about whether it fit. Both axes are still real; what
// is centralized is the arithmetic and the two named answers.
type NoteBudget int

// RailedNoteBudget is the budget for a run that draws a step rail: a wizard
// with a known sequence of screens, which is every Wizard/Flow in this project.
//
// Measured from the same Chrome the TTY driver sizes its forms with, so the two
// cannot drift apart. The rail is sized max(railMinWidth, longest label + 2),
// so any wizard whose step labels are 12 columns or fewer gets exactly this;
// a longer label narrows its own notes and is the caller's to shorten.
func RailedNoteBudget() NoteBudget {
	return noteBudgetFor(NewChrome("", []Step{{ID: "s", Label: "Step"}}, standardNoteTheme()))
}

// RaillessNoteBudget is the budget for a run that draws no rail — a command
// asking one question mid-stream, or one whose questions arrive in an order
// decided by something other than a screen list, so there is no sequence to
// show. It is about five columns wider than the railed one.
//
// Callers must not use it to be generous: those five columns are the difference
// between an approval's block naming a host and cutting it, so the choice has
// to match the chrome the run actually presents.
func RaillessNoteBudget() NoteBudget {
	return noteBudgetFor(NewChrome("", nil, standardNoteTheme()))
}

// standardNoteTheme is the terminal every budget is measured against.
func standardNoteTheme() *Theme {
	return NewTheme(Caps{TTY: true, Color: true, Width: noteFloorWidth})
}

func noteBudgetFor(c *Chrome) NoteBudget {
	return NoteBudget(c.BodyWidth() - noteFrameColumns)
}

// Columns returns the budget as a plain column count, for a caller composing
// its own line within it.
func (b NoteBudget) Columns() int { return int(b) }

// Overflows reports the lines of block a note cannot render without wrapping
// them, or nil when every line fits.
//
// Prose that wraps is merely untidy. An ADDRESS that wraps is broken: the user
// is expected to copy it, and huh puts the second half on its own line with no
// indication the two belong together — so what reaches them is two strings,
// neither of which works. Callers assert on this so a block cannot grow past
// the budget unnoticed.
//
// Measured in display columns, the unit a terminal lays out in and the unit huh
// wraps on. A rune count — which one of the four copies used — lets a line of
// CJK or emoji through at twice the width it actually draws.
func (b NoteBudget) Overflows(block string) []string {
	var over []string
	for _, line := range strings.Split(block, "\n") {
		if lipgloss.Width(line) > int(b) {
			over = append(over, line)
		}
	}
	return over
}

// Elide hard-cuts a line to the budget, marking the cut. A line that already
// fits is returned unchanged.
//
// A marked cut is honest about being incomplete where a silent wrap is not,
// which matters most where the block is the thing being approved: an approver
// reading the second half of a wrapped hostname is reading something the spec
// does not say.
func (b NoteBudget) Elide(line string) string {
	limit := int(b)
	if lipgloss.Width(line) <= limit {
		return line
	}
	if limit <= 1 {
		return "…"
	}
	return lipgloss.NewStyle().MaxWidth(limit-1).Render(line) + "…"
}

// Fit cuts every line of block to the budget, trimming the block's surrounding
// whitespace and each line's trailing whitespace. An empty block stays empty.
//
// For a block this package did not compose — one an agent or a manifest
// supplied — where the alternative to cutting is letting huh wrap it.
func (b NoteBudget) Fit(block string) string {
	block = strings.TrimSpace(block)
	if block == "" {
		return ""
	}
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		lines[i] = b.Elide(strings.TrimRight(line, " \t"))
	}
	return strings.Join(lines, "\n")
}
