package tui

import (
	"context"
	"io"
	"os"

	"github.com/charmbracelet/huh"
)

// Driver presents one prepared group and blocks until it is answered.
//
// screenID is the presenting screen's ID: drivers that show where the user is
// in the wizard resolve it against their own ordered step list rather than
// counting presentations, because skipped and work-only screens are never
// presented yet still occupy a position.
type Driver interface {
	Present(ctx context.Context, screenID string, g *huh.Group) error
}

// DriverParams is everything driver selection needs.
type DriverParams struct {
	// Theme carries the Caps the choice is made from, and styles whichever
	// driver is chosen. A nil Theme is the uncolored one.
	Theme *Theme
	// Chrome frames the TTY driver's forms. Nil renders a run with no rail.
	Chrome *Chrome
	// In and Out are the streams the interactive drivers prompt over.
	In  io.Reader
	Out io.Writer
	// NonInteractive is the --non-interactive flag.
	NonInteractive bool
	// Inline builds the terminal driver without the alternate screen, for a
	// run whose answers depend on the output around it. See Options.Inline for
	// the rule that decides it; it is carried here so a command that builds
	// ONE driver for several runs states that decision once.
	Inline bool
}

// DriverFor resolves the Driver for a run.
//
// This is the ONE place the three-way choice is made. No command branches on
// Caps.TTY itself, so "which renderer does this terminal get" cannot be
// answered two different ways in two commands — the same reason Detect is the
// only place TTY-ness is established.
//
// The order of the two decisions is itself the contract:
//
//   - NonInteractive wins over any capability. A user who asked not to be
//     prompted must not be prompted merely because a terminal is attached;
//     the fail-closed driver turns a missing answer into an error naming the
//     screen instead of a question nobody is there to read.
//   - Otherwise Caps decides, and color-off means plain. Someone who set
//     NO_COLOR or --no-color did not ask for a monochrome bubbletea
//     takeover, they asked for output they can read and pipe.
//   - Caps describe the WRITER. The terminal driver additionally requires a
//     reader somebody could be answering on — see readerCanAnswer.
//
// Inline is NOT a fourth branch. It selects nothing: the same two decisions
// pick the driver, and Inline only says which terminal buffer the chosen
// terminal driver draws in. A run that resolved to the line-oriented or
// fail-closed driver is unaffected by it, which is what stops "ask inline"
// from becoming a way to ask on a terminal that has nobody at it.
func DriverFor(p DriverParams) Driver {
	if p.NonInteractive {
		return NonInteractive()
	}
	if themeOrDefault(p.Theme).Caps.mode() == modeTTY && readerCanAnswer(p.In) {
		return TTY(TTYOpts{Out: p.Out, Theme: p.Theme, Chrome: p.Chrome, Inline: p.Inline})
	}
	return Plain(p.In, p.Out, p.Theme)
}

// readerCanAnswer reports whether the terminal driver may be selected for a
// run reading from in.
//
// The two interactive drivers do not read the same stream. The plain driver
// reads the reader it is handed; the terminal driver hands bubbletea the
// terminal and reads PROCESS stdin, ignoring In entirely (TTYOpts takes only
// Out). So a terminal WRITER is not on its own evidence that a question can be
// answered: `some-command < answers.txt` from a terminal has capabilities that
// say bubbletea and a reader the terminal driver would throw away — and
// huh's accessible renderer turns the resulting end-of-input into each field's
// default with a NIL ERROR, which is how a run fabricates answers nobody gave.
//
// Two cases, and the distinction is load-bearing:
//
//   - A nil reader is huh's "read os.Stdin" signal (see Plain), and every
//     command that leaves In unset is saying exactly that. It must keep
//     selecting the terminal driver, or those commands lose their terminal UI
//     altogether.
//   - A non-nil reader that is not a terminal file — a script, a pipe, a test's
//     buffer — means somebody supplied answers through In. The plain driver is
//     the one that reads them.
//
// Downgrading rather than refusing is deliberate: the plain driver reads the
// stream the answers are actually on, so a piped run keeps working. Refusing
// would break every scripted caller to protect the unscripted one.
func readerCanAnswer(in io.Reader) bool {
	if in == nil {
		return true
	}
	f, ok := in.(*os.File)
	return ok && isTerminal(f)
}

// themeOrDefault substitutes the uncolored theme for a nil one. Every driver
// constructor goes through it: a missing style must degrade to plain text,
// never panic partway through a wizard on a terminal the user is sitting at.
func themeOrDefault(th *Theme) *Theme {
	if th == nil {
		return NewTheme(Caps{})
	}
	return th
}
