package tui

import (
	"bufio"
	"context"
	"io"

	"github.com/charmbracelet/huh"
)

// plainDriver renders line-oriented prompts through huh's accessible mode,
// reading answers from in and writing to out. It takes over nothing and emits
// no ANSI when the theme has color disabled.
//
// This is the driver used off-TTY, under NO_COLOR, and by every test — which
// is why wizard tests need no pseudo-terminal.
//
// Cancellation here needs no detection: huh's accessible renderer reads
// line-oriented input with the terminal in its normal cooked mode, so Ctrl+C
// arrives as SIGINT to the process rather than as a keypress to the form.
// There is no "the user canceled" outcome for this driver to mistake for an
// answer — unlike the alt-screen driver, which puts the terminal in raw mode
// and must read that gesture off the form itself.
type plainDriver struct {
	in    io.Reader
	out   io.Writer
	theme *Theme
}

// Plain returns a Driver that prompts over the supplied IO. A nil theme is
// the uncolored one.
//
// in is wrapped so that a read never crosses a newline — see lineReader for
// why a wizard would otherwise lose every answer after the first. A nil in is
// left nil, which is huh's "read os.Stdin" signal; wrapping it would hand huh
// a non-nil reader with nothing behind it.
func Plain(in io.Reader, out io.Writer, th *Theme) Driver {
	if in != nil {
		in = newLineReader(in)
	}
	return &plainDriver{in: in, out: out, theme: themeOrDefault(th)}
}

func (d *plainDriver) Present(ctx context.Context, _ string, g *huh.Group) error {
	return huh.NewForm(g).
		WithTheme(d.theme.Form).
		WithAccessible(true).
		WithInput(d.in).
		WithOutput(d.out).
		RunWithContext(ctx)
}

// lineReader yields at most one line per Read.
//
// huh's accessible renderer builds a FRESH bufio.Scanner for every field it
// prompts, and a Scanner fills its buffer greedily. Handed a reader with more
// than one line available — a pipe, a file, a test's strings.Reader — the
// first field's scanner swallows the whole thing, answers itself from line
// one, and is then discarded along with everything it buffered. Every
// subsequent field reads EOF and takes its default, and because that renderer
// has no way to report a read error the run completes with a nil error and
// silently blank answers.
//
// Buffering here instead, once per driver, is what makes the reader survive
// from one field (and one screen) to the next: a Scanner that can never read
// past a newline has nothing left over to lose.
type lineReader struct {
	br *bufio.Reader
	// pending holds the read error deferred by a Read that had bytes to
	// return. bufio.Reader clears its own stored error once it reports it, so
	// an underlying reader that surfaces a transport failure exactly once
	// would have it dropped on the floor if this Read simply returned (n, nil)
	// and forgot it.
	pending error
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{br: bufio.NewReader(r)} }

func (r *lineReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pending != nil {
		err := r.pending
		r.pending = nil
		return 0, err
	}
	n := 0
	for n < len(p) {
		b, err := r.br.ReadByte()
		if err != nil {
			// Report the bytes already copied now and the error on the next
			// Read. Returning (n>0, err) together is legal but invites callers
			// that ignore n on a non-nil error.
			if n > 0 {
				r.pending = err
				return n, nil
			}
			return 0, err
		}
		p[n] = b
		n++
		if b == '\n' {
			break
		}
	}
	return n, nil
}
