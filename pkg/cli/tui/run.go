package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Options configures a run.
//
// A command states its terminal and its intent; Run assembles the chrome and
// resolves the driver from them. That is deliberate: the alternative is every
// command repeating the same three-way branch on Caps and the same hand-built
// step list, and fifteen copies of a decision is fifteen chances to make it
// differently.
type Options struct {
	// Theme styles the whole run — form fields, chrome, and the summary and
	// tables the caller renders afterwards from the same *Theme. Required.
	Theme *Theme

	// Title is the chrome's title bar text (e.g. "oap · channel create · slack").
	Title string

	// In and Out are the streams the interactive drivers prompt over.
	// Unused when NonInteractive is set, or when Driver overrides selection.
	In  io.Reader
	Out io.Writer

	// NonInteractive is the --non-interactive flag: the run fails closed at
	// the first screen whose answer was not seeded into State, rather than
	// prompting for it.
	NonInteractive bool

	// Inline draws the run's questions in the terminal's ordinary buffer
	// instead of taking the alternate screen.
	//
	// THE RULE, and it is a property of the RUN rather than of any one
	// caller's taste: a run is inline when its answers depend on the terminal
	// around it — output the run does not itself render, whether streamed
	// between its questions (an install's build progress, an agent's
	// transcript) or printed before it (an address the user has to hand to
	// another system). A run whose questions can be answered from what the
	// form itself shows owns the terminal for its duration and keeps the
	// alternate screen.
	//
	// The alternate screen does not merely cover what is behind it: while it
	// is up the terminal's own scrollback is unreachable, so a user cannot
	// even scroll back to the line the question is about. That is the whole
	// cost, and it is why the rule is about DEPENDENCE and not about
	// aesthetics — a wizard that says everything it needs to say inside its
	// own fields loses nothing by taking the screen.
	//
	// Stating it per run rather than per caller is deliberate: the
	// alternative — every mid-command caller reaching for Plain — puts a
	// driver choice back in the callers, which is what DriverFor exists to
	// prevent. It changes ONE bubbletea program option; the sequencer, the
	// chrome and the theme are identical either way, and it cannot turn a
	// terminal that resolved to the line-oriented or fail-closed driver into
	// an alt-screen one.
	//
	// WHERE A DRIVER IS SHARED, the unit is the driver — the union of the
	// runs that go over it — not each run in isolation. A command that builds
	// one driver and presents several runs on it (which is required, so the
	// buffered reader survives past the first answer) settles Inline once, at
	// construction; a later run cannot opt back out. So classify by asking
	// whether ANY run on that driver depends on output around it. `oap agent
	// install` is the worked example: its adopt question is self-contained,
	// and is inline only because it shares the driver its manifest and build
	// questions need.
	//
	// Unused when Driver overrides selection: a run presenting over a driver
	// somebody else built inherits that driver's answer, which is what keeps
	// the several runs of one command from disagreeing.
	Inline bool

	// Driver, when non-nil, presents every group instead of the one DriverFor
	// would resolve. It is the seam for a bespoke or test driver; commands
	// leave it nil and let their capabilities decide.
	Driver Driver
}

// driverParams is what this run asks DriverFor for.
//
// A method rather than a literal inside RunWith so that what a run declares and
// what driver selection is handed cannot drift — Inline in particular is
// invisible in the rendered output of every driver a test can build, so the
// only place it is observable is here.
func (o Options) driverParams(screens []Screen) DriverParams {
	return DriverParams{
		Theme:          o.Theme,
		Chrome:         NewChrome(o.Title, Steps(screens), o.Theme),
		In:             o.In,
		Out:            o.Out,
		NonInteractive: o.NonInteractive,
		Inline:         o.Inline,
	}
}

// Run drives screens to completion against a fresh State.
func Run(ctx context.Context, screens []Screen, opts Options) (*State, error) {
	return RunWith(ctx, screens, opts, NewState())
}

// RunWith drives screens against a caller-supplied State. Seeding that State
// from flags is how a caller answers screens ahead of time; combined with
// Options.NonInteractive it yields a fail-closed non-interactive mode.
//
// Cancellation is checked only BETWEEN screens, not during one: a context
// canceled while a screen's Apply is running is not observed until the next
// iteration (or not at all, if that was the last screen), so Run returns a
// nil error in that case rather than context.Canceled.
func RunWith(ctx context.Context, screens []Screen, opts Options, st *State) (*State, error) {
	// Defended first so every return path below yields a usable *State — a
	// nil-receiver State.Get would otherwise panic on a caller that inspects
	// state after a validation error.
	if st == nil {
		st = NewState()
	}
	// Recorded before any screen runs, and unconditionally, so a seeded value
	// cannot disagree with the run's actual mode.
	st.SetBool(KeyNonInteractive, opts.NonInteractive)
	if opts.Theme == nil {
		return st, errors.New("tui: Options.Theme is required")
	}

	driver := opts.Driver
	if driver == nil {
		driver = DriverFor(opts.driverParams(screens))
	}

	for _, scr := range screens {
		if err := ctx.Err(); err != nil {
			return st, fmt.Errorf("tui: canceled before screen %q: %w", scr.ID(), err)
		}

		g, err := scr.Prepare(ctx, st)
		switch {
		case errors.Is(err, ErrSkip):
			continue
		case err != nil:
			return st, fmt.Errorf("tui: prepare screen %q: %w", scr.ID(), err)
		}

		if g != nil {
			if err := driver.Present(ctx, scr.ID(), g); err != nil {
				return st, fmt.Errorf("tui: present screen %q: %w", scr.ID(), err)
			}
		}

		if err := scr.Apply(ctx, st); err != nil {
			return st, fmt.Errorf("tui: apply screen %q: %w", scr.ID(), err)
		}
	}
	return st, nil
}
