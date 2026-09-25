package tui

import (
	"os"

	"golang.org/x/term"
)

// defaultWidth is used when the terminal width cannot be measured (a pipe, a
// CI runner, a test). Chosen to match the narrowest terminal we lay out for;
// never fall back to 0, which would collapse every bordered box.
const defaultWidth = 80

// Caps is what the attached terminal can do. Detect is the ONE place these
// facts are established — no consumer calls term.IsTerminal itself, so
// "is this a TTY" cannot be answered two different ways in two commands.
type Caps struct {
	TTY   bool
	Color bool
	Width int
	// Height is the terminal's row count, or 0 when it cannot be measured (a
	// pipe, a CI runner, a test buffer). A renderer that redraws a live region
	// in place must not walk the cursor up past the top of the pane, so a
	// region-drawing consumer clamps to Height-1; 0 means "unknown, do not
	// clamp" — the same don't-know posture Width takes when it falls back.
	Height int
}

// mode is the rendering mode implied by Caps. It is unexported because
// DriverFor is the only thing entitled to act on it: a consumer that could
// read the mode could branch on it, and driver selection living in one place
// is the whole point.
type mode int

const (
	// modePlain renders line-oriented prompts with no ANSI and no screen
	// takeover. Used off-TTY and whenever color is disabled.
	modePlain mode = iota
	// modeTTY renders the full bubbletea experience with chrome. Which buffer
	// it draws in is the run's to say — see Options.Inline — not these
	// capabilities'.
	modeTTY
)

// mode reports how these capabilities should be rendered. Color-off implies
// plain: a user who asked for no color did not ask for a bubbletea takeover
// rendered in monochrome box-drawing characters.
func (c Caps) mode() mode {
	if c.TTY && c.Color {
		return modeTTY
	}
	return modePlain
}

// isTerminal reports whether f is a terminal. The ONE place this package asks
// that question of the operating system: Detect answers it for the stream a run
// writes to, and DriverFor answers it for the stream a run reads from, and the
// two must not be able to answer it differently.
func isTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// Detect establishes capabilities for out. userNoColor reflects the --no-color
// flag; NO_COLOR (any non-empty value) and a non-TTY out each independently
// disable color.
func Detect(out *os.File, userNoColor bool) Caps {
	fd := int(out.Fd())
	isTTY := isTerminal(out)

	width := defaultWidth
	height := 0
	if isTTY {
		if w, h, err := term.GetSize(fd); err == nil {
			if w > 0 {
				width = w
			}
			if h > 0 {
				height = h
			}
		}
	}

	color := isTTY && !userNoColor && os.Getenv("NO_COLOR") == ""
	return Caps{TTY: isTTY, Color: color, Width: width, Height: height}
}
