// Package progress renders oap install progress as a sequence of named phases.
// Each phase can run a readiness wait with a live spinner, surface diagnostics
// when the wait stalls, and let the user keep waiting instead of aborting. Two
// renderers implement Reporter — a checklist for a terminal and a streaming
// renderer for non-TTY/CI — selected by New based on cliout.IsTTY(out). The
// installer is written once against the interface.
package progress

import (
	"context"
	"io"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// Poll reports whether the awaited condition holds yet.
type Poll func(context.Context) (done bool, err error)

// Diagnose gathers a best-effort explanation of why a wait is stalled.
type Diagnose func(context.Context) (wait.Diagnosis, error)

// FixFunc is an install-supplied hook that launches the user's own AI CLI to
// diagnose and fix a stalled component. component is the failing component's
// name and diag is the diagnostics already gathered for it. It returns an error
// only if the launch itself failed — that error is surfaced and the keep-waiting
// prompt is RE-ASKED; it never aborts the wait. A nil FixFunc (the default)
// keeps the keep-waiting prompt at [Y/n]; a non-nil one extends it to [Y/n/f].
//
// The progress package never imports the launcher/exec machinery: the installer
// supplies the FixFunc (see cmd/oap), so this package stays rendering-only.
type FixFunc func(ctx context.Context, component string, diag wait.Diagnosis) error

// Reporter renders the overall install progress.
type Reporter interface {
	Phase(name string) Phase
	Info(format string, a ...any)
	OK(format string, a ...any)
	Warn(format string, a ...any)
	// SetFixHook installs the AI-fixer hook used by the keep-waiting prompt.
	// Pass nil (or never call it) to keep the prompt at [Y/n]. Called once,
	// before any phase runs a wait.
	SetFixHook(FixFunc)
	// Suspend runs fn with the live progress display quiesced and the bottom
	// region cleared, handing fn the renderer's underlying writer AND stdin reader
	// so it can own the terminal for interactive I/O (a y/N prompt) or to stream a
	// long subprocess's output (e.g. `gcloud … --gateway-api=standard`) without the
	// checklist drawing over it. The stdin reader lets a streamed subprocess
	// inherit the real terminal so it detects an interactive TTY. Concurrent
	// redraws are suppressed for the duration and the region is restored when fn
	// returns. Renderers without a live region simply call fn with their writer.
	Suspend(fn func(out io.Writer, in io.Reader))
	Close() error
}

// Phase is one top-level milestone (a checklist row).
type Phase interface {
	Detail(format string, a ...any)
	// Progress switches this row to a determinate progress bar showing
	// current/total and an optional detail (e.g. a byte count). Safe to call
	// repeatedly to advance the bar. On a terminal it renders a unicode bar
	// (label [████████░░░░] 12/20 detail); on a non-TTY stream it prints a
	// throttled line (label: 12/20 (detail)) only when the percentage crosses a
	// 10% boundary, so callers may advance it freely without spamming output. A
	// bar row reaching current==total then calling Done() shows the ✓ as usual.
	Progress(current, total int, detail string)
	// Await polls until the condition is ready or the user gives up. recheckBase
	// is a context that is NOT subject to the overall --timeout; each
	// keep-waiting round polls against it so an exhausted timeout cannot make
	// "Y" a no-op. eta is the expected time-to-ready: when non-zero a one-time
	// soft-warn is emitted if that duration elapses before the component is
	// ready; the poll continues regardless. Callers without a meaningful ETA
	// pass 0 for eta.
	Await(ctx, recheckBase context.Context, deadline, eta time.Duration, poll Poll, diagnose Diagnose) error
	// AwaitOptional is like Await but NEVER prompts to keep waiting: on the
	// deadline it surfaces diagnostics and returns the timeout error so the
	// caller can continue in a degraded state. For optional/non-fatal components.
	AwaitOptional(ctx, recheckBase context.Context, deadline, eta time.Duration, poll Poll, diagnose Diagnose) error
	Done()
	// DoneWith marks the phase done like Done but keeps a short trailing note on
	// the row (e.g. "already running" when a re-run found the component already
	// up). An empty note is exactly Done().
	DoneWith(note string)
	Fail()
	// Skip marks the phase as skipped/degraded (distinct from Fail): the
	// component is optional and the install is continuing without it.
	Skip(reason string)
	// Status sets a live status string on the row — for example the last output
	// line of the ongoing operation. When non-empty the wait label renders as
	// "<status>  (<elapsed> · ~<eta>)" so the current step is visible at a
	// glance; when empty (the default) the row falls back to the generic
	// "waiting (<elapsed> · ~<eta>)". Safe to call from any goroutine; the
	// implementation takes the renderer's mutex. Done()/Fail() overwrite the
	// row status independently, so the caller need not clear Status before
	// calling them.
	Status(s string)
}

// New returns the checklist renderer on a terminal, otherwise the streaming
// renderer. stdin backs the keep-waiting prompt; assumeYes (the install --yes
// flag) suppresses prompting so non-interactive runs fail fast with diagnostics.
func New(out io.Writer, stdin io.Reader, assumeYes bool) Reporter {
	if cliout.IsTTY(out) {
		return newChecklist(out, stdin, assumeYes)
	}
	return newStreaming(out, stdin, assumeYes)
}

// RailProvider supplies a persistent left step rail the checklist renders
// beside its own rows — the unified init wizard's own step navigation (Build /
// Install / Status), kept on screen while the phase it names runs. The
// checklist calls Steps()/Active() fresh on every redraw, so advancing the
// wizard's active step repaints the rail with no extra plumbing back into
// this package: the wizard just mutates whatever Active() reads from.
type RailProvider interface {
	Steps() []tui.Step
	Active() int
}

// NewWithRail is like New, but on a terminal the checklist composites a
// persistent rail column (sourced from rail) to the left of its rows.
// rail == nil behaves EXACTLY like New — it is forwarded there directly —
// so the non-wizard oap install/oap init path is provably unaffected by this
// entry point existing at all.
func NewWithRail(out io.Writer, stdin io.Reader, assumeYes bool, rail RailProvider) Reporter {
	if rail == nil {
		return New(out, stdin, assumeYes)
	}
	if cliout.IsTTY(out) {
		return newChecklistWithRail(out, stdin, assumeYes, detectTheme(out), rail)
	}
	return newStreaming(out, stdin, assumeYes)
}
