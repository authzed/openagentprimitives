package tui

import (
	"io"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Palette is the entire color vocabulary of `oap`. Adding a color means adding
// it HERE, not inline at a call site — that is what keeps a wizard and a list
// command looking like the same product. ANSI 256 indices are used rather than
// hex so the terminal's own theme stays authoritative.
const (
	colorAccent  = lipgloss.Color("6")  // cyan: titles, active rail step
	colorSuccess = lipgloss.Color("10") // green: completed markers
	colorWarn    = lipgloss.Color("11") // yellow
	colorError   = lipgloss.Color("9")  // red
	colorSubtle  = lipgloss.Color("8")  // gray: descriptions, hints
	colorValue   = lipgloss.Color("7")  // default foreground for values
	colorInverse = lipgloss.Color("15") // badge foreground

	// Speaker colors. A transcript needs four markers a reader can tell apart
	// at a glance, and the six above cannot supply them: green, yellow and red
	// already mean succeeded, warned and failed everywhere else in `oap`, so
	// dressing "assistant" in one of them would report a status the turn does
	// not have. These two are therefore the palette's only non-status colors.
	colorRoleUser      = lipgloss.Color("12") // blue: a person's turn
	colorRoleAssistant = lipgloss.Color("13") // magenta: the agent's turn
)

// Theme is the single definition of how `oap` looks. It yields BOTH a
// *huh.Theme (for form fields) and lipgloss styles (for chrome, summaries, and
// tables) so the two can never drift apart.
type Theme struct {
	Form *huh.Theme

	Title   lipgloss.Style
	Subtle  lipgloss.Style
	Success lipgloss.Style
	Warn    lipgloss.Style
	Err     lipgloss.Style
	Badge   lipgloss.Style
	Label   lipgloss.Style
	Value   lipgloss.Style

	Rail       lipgloss.Style // a pending step in the rail
	RailActive lipgloss.Style // the step being answered
	RailDone   lipgloss.Style // an answered step

	// Frame is the box drawn around a pane of a long-lived full-screen TUI —
	// `oap agent chat`'s timeline and overlays, `oap tools mcp probe`'s two
	// panes. Those models cannot use Chrome (see Truncate for why), so without
	// this the border would be hand-rolled at each call site off lipgloss's
	// global renderer, which is how two surfaces of one product end up framed
	// differently.
	//
	// Apply it directly (th.Frame.Width(w).Render(s)), NOT through Render: a
	// border is structure, not color, and Render deliberately returns its input
	// untouched when color is off — which would drop the box entirely. The
	// color-off Frame is built on an Ascii-profile renderer instead, so it
	// still draws its border runes and still emits no escape codes.
	Frame lipgloss.Style

	// Selected is the highlight for the row under the cursor in a long-lived
	// list. Reverse video rather than a palette color because the row it marks
	// already carries its own colors (a tool name, its badges) that a
	// foreground override would flatten.
	Selected lipgloss.Style

	// Speaker markers for a transcript — `oap agent run`, `oap session logs`,
	// `oap agent chat`. They live on the shared theme rather than at the two
	// renderers that draw transcripts so "who is talking" is one answer, and
	// they are bold because each is a banner introducing a block, not a word
	// inside one.
	RoleUser      lipgloss.Style
	RoleAssistant lipgloss.Style
	RoleSystem    lipgloss.Style
	RoleTool      lipgloss.Style

	Caps Caps
}

// NewTheme builds the theme for the given capabilities. When color is
// disabled every style is the zero style, so Render returns its input
// unchanged and output is byte-clean for pipes and CI.
func NewTheme(c Caps) *Theme {
	t := &Theme{Caps: c, Form: huh.ThemeBase()}
	if !c.Color {
		// Zero-value lipgloss.Style renders text unchanged. huh.ThemeBase()
		// is likewise the uncolored base, so forms stay legible.
		//
		// Frame is the exception: it has to keep drawing its border runes, so
		// it cannot be the zero style. Building it on an Ascii-profile renderer
		// makes it structurally identical to the colored one and provably
		// escape-free, rather than leaving that to whatever profile lipgloss's
		// global renderer happened to detect from os.Stdout.
		plain := lipgloss.NewRenderer(io.Discard)
		plain.SetColorProfile(termenv.Ascii)
		t.Frame = plain.NewStyle().Border(lipgloss.RoundedBorder())
		forceASCIIMarkers(t.Form)
		return t
	}

	// The renderer's writer is deliberately inert: nothing here is written
	// through it, and every style below resolves against the explicit profile
	// set on the next line. A real stream would instead make lipgloss probe
	// that terminal — an isatty syscall now, and an OSC 11 background query
	// that writes to and blocks on the stream the moment an AdaptiveColor is
	// added — against a stream Caps did not measure.
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	base := func() lipgloss.Style { return r.NewStyle() }

	t.Title = base().Bold(true).Foreground(colorAccent)
	t.Subtle = base().Foreground(colorSubtle)
	t.Success = base().Foreground(colorSuccess)
	t.Warn = base().Foreground(colorWarn)
	t.Err = base().Foreground(colorError)
	t.Badge = base().Bold(true).Background(colorSubtle).Foreground(colorInverse).Padding(0, 1)
	t.Label = base().Foreground(colorSubtle)
	t.Value = base().Foreground(colorValue)

	t.Rail = base().Foreground(colorSubtle)
	t.RailActive = base().Bold(true).Foreground(colorAccent)
	t.RailDone = base().Foreground(colorSuccess)

	// The frame is chrome around content, so it takes the subtle color rather
	// than the accent huh gives a FOCUSED field's border — a pane border that
	// competed with the focused field would read as the thing to look at.
	t.Frame = base().Border(lipgloss.RoundedBorder()).BorderForeground(colorSubtle)
	t.Selected = base().Reverse(true)

	t.RoleUser = base().Bold(true).Foreground(colorRoleUser)
	t.RoleAssistant = base().Bold(true).Foreground(colorRoleAssistant)
	t.RoleSystem = base().Bold(true).Foreground(colorSubtle)
	t.RoleTool = base().Bold(true).Foreground(colorAccent)

	ft := huh.ThemeBase()
	ft.Focused.Title = ft.Focused.Title.Foreground(colorAccent).Bold(true)
	ft.Focused.NoteTitle = ft.Focused.NoteTitle.Foreground(colorAccent).Bold(true)
	ft.Focused.Description = ft.Focused.Description.Foreground(colorSubtle)
	ft.Focused.Base = ft.Focused.Base.BorderForeground(colorAccent)
	ft.Focused.SelectSelector = ft.Focused.SelectSelector.Foreground(colorAccent)
	ft.Focused.SelectedOption = ft.Focused.SelectedOption.Foreground(colorSuccess)
	ft.Focused.SelectedPrefix = ft.Focused.SelectedPrefix.Foreground(colorSuccess)
	ft.Focused.ErrorIndicator = ft.Focused.ErrorIndicator.Foreground(colorError)
	ft.Focused.ErrorMessage = ft.Focused.ErrorMessage.Foreground(colorError)

	// Blurred stays derived from huh.ThemeBase()'s own blurred styles (hidden
	// border, blank next/prev indicators already set there) instead of
	// aliasing Focused wholesale — that alias made every field in a
	// multi-field group render a bold accent-colored title and a visible
	// select cursor/highlight at once, with no way to tell which field was
	// actually focused. Only the colors that make sense independent of focus
	// carry over, so a blurred field is still legible and on-palette rather
	// than reverting to no color at all — just visually subordinate to the
	// one field that IS focused. An error is a fact about the field's value,
	// not about focus, so ErrorIndicator/ErrorMessage stay red even blurred —
	// a user tabbing away from an invalid field must not see it go quiet.
	ft.Blurred.Title = ft.Blurred.Title.Foreground(colorSubtle)
	ft.Blurred.NoteTitle = ft.Blurred.NoteTitle.Foreground(colorSubtle)
	ft.Blurred.Description = ft.Blurred.Description.Foreground(colorSubtle)
	ft.Blurred.ErrorIndicator = ft.Blurred.ErrorIndicator.Foreground(colorError)
	ft.Blurred.ErrorMessage = ft.Blurred.ErrorMessage.Foreground(colorError)
	forceASCIIMarkers(ft)
	t.Form = ft

	return t
}

// forceASCIIMarkers replaces huh's interactive marker glyphs with ASCII.
//
// huh's base theme marks a selected multi-select row with • (U+2022) and pages
// a long option list with → and ← (U+2192, U+2190). All three are East-Asian
// *ambiguous-width* runes: bubbletea v1.3.x's renderer counts them as one cell
// while a terminal set to grapheme-width-method=unicode draws them as two,
// which desyncs the repaint and leaves a ghost copy of every line the cursor
// moves off — enough to make a wizard unusable.
//
// Applied here rather than by the one command that first hit it, because the
// glyphs come from huh's base theme and so every wizard in this project carries
// the exposure — a multi-select is simply where it shows first. SetString
// preserves each style's foreground color and replaces only the glyph.
func forceASCIIMarkers(ft *huh.Theme) {
	for _, s := range []*huh.FieldStyles{&ft.Focused, &ft.Blurred} {
		s.SelectedPrefix = s.SelectedPrefix.SetString("[x] ")
		s.UnselectedPrefix = s.UnselectedPrefix.SetString("[ ] ")
		s.NextIndicator = s.NextIndicator.SetString(">")
		s.PrevIndicator = s.PrevIndicator.SetString("<")
	}
}

// Render applies a style, returning text unchanged when color is disabled.
func (t *Theme) Render(s lipgloss.Style, text string) string {
	if !t.Caps.Color {
		return text
	}
	return s.Render(text)
}
