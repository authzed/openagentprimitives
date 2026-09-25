package apcmd

import (
	"bufio"
	"io"
	"os"
	"strings"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// DetectCaps reports the terminal capabilities of out. A writer that is not a
// real *os.File — a test buffer, a pipe wrapped by a command's OutOrStdout —
// has no capabilities at all, which is what drives the non-interactive driver.
func DetectCaps(out io.Writer, noColor bool) tui.Caps {
	f, ok := out.(*os.File)
	if !ok {
		return tui.Caps{}
	}
	return tui.Detect(f, noColor)
}

// Theme is the tui theme for a command writing to out: capabilities come from
// that stream, and --no-color comes from these Globals.
//
// Every command resolves its theme through here rather than spelling
// tui.NewTheme(DetectCaps(out, g.NoColor)) itself, so no command can build a
// theme from capabilities it invented — which is the mistake that puts escape
// codes down a pipe.
func (g *Globals) Theme(out io.Writer) *tui.Theme {
	return tui.NewTheme(DetectCaps(out, g.NoColor))
}

// Table starts a themed tui.Table for a command writing to out.
//
// It is the one way an `oap` command renders columns. Convenience only — a
// command that also needs the theme for something else resolves Theme(out)
// once and calls tui.NewTable itself, which is the same table.
func (g *Globals) Table(out io.Writer, headers ...string) *tui.Table {
	return tui.NewTable(g.Theme(out), headers...)
}

// StdinIsInteractive reports whether f is a character device, i.e. a real
// terminal rather than a pipe or a redirected file. Commands use it to decide
// whether prompting the user is even possible.
func StdinIsInteractive(f *os.File) bool {
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

// Confirm returns the operator's y/N answer. assumeYes short-circuits to true;
// a non-TTY without assumeYes is always false (never block CI on a prompt);
// otherwise it reads one line from in and accepts only y/yes (case-insensitive).
func Confirm(in io.Reader, out io.Writer, prompt string, assumeYes, isTTY bool) bool {
	if assumeYes {
		return true
	}
	if !isTTY {
		return false
	}
	cliout.Prompt(out, "%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
