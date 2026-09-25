package tui

import (
	"context"
	"errors"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// ttyDriver renders each group as a bubbletea program with chrome around it.
//
// It has two shapes, and inline is the whole difference between them. A
// full-screen run takes the alternate screen and releases it afterwards, so
// the caller's summary is what remains in scrollback and the terminal's own
// history is untouched — and, for as long as the question is up, unreachable.
// An inline run draws in the ordinary buffer instead, below whatever the
// command has already written and above whatever it writes next.
type ttyDriver struct {
	out    io.Writer
	theme  *Theme
	chrome *Chrome

	// inline drops the alternate screen from the program's options. See
	// Options.Inline for the rule that decides it.
	inline bool

	// step, when set, is the rail position EVERY group this driver presents is
	// drawn under, whatever ID the presenting screen carries. It is set only by
	// Reframe, for a caller whose rail steps are its own units of work rather
	// than the screens — see Reframe for why a shared driver needs it.
	//
	// Empty is the ordinary case and means "resolve each screen's own ID",
	// which is what keeps the highlight on the step the user is answering in a
	// run whose rail IS its screens.
	step string

	// runProgram drives a composed model to completion. It is a field so that
	// everything Present decides around it — which rail step is active, what a
	// cancellation means, which errors surface — is exercisable without a
	// pseudo-terminal.
	runProgram func(ctx context.Context, m *chromeModel) error
}

// TTYOpts is how the terminal driver is built. One struct rather than a
// positional bool, because Inline reads as a mode at the call site and every
// caller of this constructor is stating which of the two shapes above it wants.
type TTYOpts struct {
	// Out is the stream the program renders to.
	Out io.Writer
	// Theme styles the form and the chrome. Nil is the uncolored theme.
	Theme *Theme
	// Chrome frames each group. Nil is a run with no rail.
	Chrome *Chrome
	// Inline draws in the terminal's ordinary buffer rather than taking the
	// alternate screen. See Options.Inline for the rule, which is the run's to
	// state and not this constructor's to guess.
	Inline bool
}

// TTY returns the terminal driver.
func TTY(o TTYOpts) Driver {
	th := themeOrDefault(o.Theme)
	ch := o.Chrome
	if ch == nil {
		ch = NewChrome("", nil, th)
	}
	d := &ttyDriver{out: o.Out, theme: th, chrome: ch, inline: o.Inline}
	d.runProgram = d.runTeaProgram
	return d
}

// Present renders one group inside the chrome and blocks until the user
// answers it, cancels, or the program fails.
//
// Cancellation is an error, never a value. huh reports Ctrl+C by setting
// StateAborted and returning its CancelCmd: it emits no error, and Errors()
// holds only field-validation failures, so it stays empty for a field the
// user never submitted. A Present that only inspected those two would return
// nil for a canceled screen, and the sequencer would go on to Apply it and
// present the next one — building, from partial answers, the very resource
// the user pressed Ctrl+C to avoid creating.
func (d *ttyDriver) Present(ctx context.Context, screenID string, g *huh.Group) error {
	// Size the form to the column Chrome will actually place it into, not the
	// raw terminal width — WithWidth also sets huh's own wrap point, so a
	// wider value here re-wraps the form at one width while Render's rail
	// gutter re-wraps it again at a narrower one, breaking layout mid-render.
	form := huh.NewForm(g).WithTheme(d.theme.Form).WithWidth(d.chrome.BodyWidth())

	// The rail position is resolved from d.railStep, never from screenID
	// directly; the error paths below still name screenID, because what failed
	// is the screen the user was on and not the caller's unit of work.
	m := &chromeModel{form: form, chrome: d.chrome, idx: d.chrome.StepIndex(d.railStep(screenID))}
	err := d.runProgram(ctx, m)

	switch {
	// tea.ErrInterrupted is the same user gesture arriving by the other
	// route, when an InterruptMsg reaches the program rather than the form.
	case m.form.State == huh.StateAborted, errors.Is(err, tea.ErrInterrupted):
		return fmt.Errorf("tui: screen %q canceled: %w", screenID, huh.ErrUserAborted)
	case err != nil:
		return fmt.Errorf("tui: run screen %q: %w", screenID, err)
	}

	// Read the model's form, not the local: chromeModel.Update reassigns it
	// from whatever huh's own Update returns, so it is the one with the
	// answered state on it.
	if formErrs := m.form.Errors(); len(formErrs) > 0 {
		return fmt.Errorf("tui: screen %q: %w", screenID, errors.Join(formErrs...))
	}
	return nil
}

// railStep is the chrome step a group presented under screenID is drawn in:
// this driver's pinned one when it has been reframed, and otherwise the
// screen's own ID.
func (d *ttyDriver) railStep(screenID string) string {
	if d.step != "" {
		return d.step
	}
	return screenID
}

// Reframe implements Reframer: the same terminal, a different frame.
//
// The copy shares everything that describes the TERMINAL — the writer, the
// theme, whether the alternate screen is taken — and replaces only what
// describes the FRAME. That is the whole point: a reframed driver must keep
// drawing into the same place as the one it came from, or a command's several
// runs stop looking like one command again.
//
// runProgram is copied ALONG WITH the rest and deliberately not rebound. It is
// a method value closed over the original receiver, and everything it reads
// from that receiver (the writer, the alternate-screen decision) is identical
// in the copy — while the chrome it actually renders arrives on the
// chromeModel it is handed, not off the receiver. Rebinding it would instead
// throw away a test's substituted program, which is the one seam that makes
// this driver exercisable without a pseudo-terminal.
func (d *ttyDriver) Reframe(ch *Chrome, stepID string) Driver {
	if ch == nil {
		return d
	}
	n := *d
	n.chrome, n.step = ch, stepID
	return &n
}

// runTeaProgram drives the model as a bubbletea program.
func (d *ttyDriver) runTeaProgram(ctx context.Context, m *chromeModel) error {
	_, err := tea.NewProgram(m, d.programOptions(ctx)...).Run()
	return err
}

// programOptions is the whole of what Inline changes: one option, appended for
// a run that takes the screen and omitted for one that does not.
//
// Split out of runTeaProgram because that function cannot be called without a
// terminal, and this is the only place the run's decision becomes something a
// test can read back. What a test can prove about it stops here — that the
// option list handed to bubbletea does or does not carry the alternate screen.
// What the terminal then DRAWS is not observable from this process.
func (d *ttyDriver) programOptions(ctx context.Context) []tea.ProgramOption {
	opts := []tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithOutput(d.out),
	}
	if !d.inline {
		opts = append(opts, tea.WithAltScreen())
	}
	return opts
}

// chromeModel composes Chrome around huh.Form, which is itself a tea.Model.
// idx is the position of this screen in the chrome's rail.
type chromeModel struct {
	form   *huh.Form
	chrome *Chrome
	idx    int
}

func (m *chromeModel) Init() tea.Cmd { return m.form.Init() }

func (m *chromeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.form.Update(msg)
	if f, ok := next.(*huh.Form); ok {
		m.form = f
	}
	// Both terminal states quit the program; which one it was is read off the
	// form afterwards, by Present.
	if m.form.State == huh.StateCompleted || m.form.State == huh.StateAborted {
		return m, tea.Quit
	}
	return m, cmd
}

// View frames the form in the chrome — until the form has quit, at which
// point it renders nothing at all.
//
// The empty frame is not cosmetic. bubbletea writes the model's LAST view as
// the program's final frame, and an INLINE run leaves that frame in the
// terminal's ordinary buffer rather than discarding it with the alt screen.
// huh renders an empty view once its form has quit, so a chrome drawn around
// it leaves a bare title bar and a blank line in the scrollback for every
// question asked — a command that asks three over one driver stacks three of
// them, which is what an operator sees as the title repeating. Rendering
// nothing instead makes bubbletea clear the region it drew, which is also what
// a huh form run on its own does.
func (m *chromeModel) View() string {
	if m.form.State != huh.StateNormal {
		return ""
	}
	return m.chrome.Render(m.idx, m.form.View())
}
