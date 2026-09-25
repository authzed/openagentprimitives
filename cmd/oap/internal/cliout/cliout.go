// Package cliout centralizes oap's user-facing CLI output: consistent prefixes
// (so every step/warning reads the same) and optional ANSI color. Color is
// applied only when the destination Writer is a real terminal and NO_COLOR is
// unset (https://no-color.org), so piped output and test buffers stay plain.
package cliout

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	gray   = "\033[90m" // bright-black: de-emphasized detail
)

// IsTTY reports whether w is a real terminal we should render rich UI to. It is
// the same predicate as color enablement (honors NO_COLOR), exported so the
// progress package can pick a renderer.
func IsTTY(w io.Writer) bool { return colorize(w) }

// colorize reports whether ANSI codes should be written to w.
func colorize(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func line(w io.Writer, code, s string) {
	if code != "" && colorize(w) {
		fmt.Fprintln(w, code+s+reset)
		return
	}
	fmt.Fprintln(w, s)
}

// Step prints a "==> <msg>" section header (cyan/bold). Use it for each phase of
// install/clean so the two commands read alike.
func Step(w io.Writer, format string, a ...any) {
	line(w, cyan+bold, "==> "+fmt.Sprintf(format, a...))
}

// OK prints a success line (green), e.g. "installed cert-manager".
func OK(w io.Writer, format string, a ...any) { line(w, green, fmt.Sprintf(format, a...)) }

// Warn prints a "warning: <msg>" line (yellow). The prefix is standardized here
// so every caller is consistent (the codebase previously mixed warning:/WARNING:).
func Warn(w io.Writer, format string, a ...any) {
	line(w, yellow, "warning: "+fmt.Sprintf(format, a...))
}

// Errf prints an "error: <msg>" line (red) for a surfaced, non-fatal failure the
// user is waiting on (a fatal error should be returned, not printed here).
func Errf(w io.Writer, format string, a ...any) { line(w, red, "error: "+fmt.Sprintf(format, a...)) }

// Info prints a de-emphasized detail/continuation line in gray, so the verbose
// progress narration recedes and the prompts (white), step headers (cyan), and
// warnings (yellow) stand out against it.
func Info(w io.Writer, format string, a ...any) { line(w, gray, fmt.Sprintf(format, a...)) }

// Prompt writes an interactive question in bold white WITHOUT a trailing newline —
// the user's typed answer follows on the same line. It's the one bright thing
// against the gray detail, so an input prompt is never missed.
func Prompt(w io.Writer, format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	if colorize(w) {
		fmt.Fprint(w, bold+s+reset)
		return
	}
	fmt.Fprint(w, s)
}
