package progress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
)

// promptMu serializes concurrent keep-waiting prompts across all phases and
// renderers: only one component may prompt at a time; others block until the
// active prompt resolves, then re-check their own condition under the fresh
// recheck window.
var promptMu sync.Mutex

const (
	firstBackoff       = 2 * time.Second
	maxBackoff         = 20 * time.Second
	frameRate          = 100 * time.Millisecond
	keepWaitingRecheck = 30 * time.Second
)

// waitSink is the renderer-specific surface awaitLoop draws through. Both
// renderers' Phase types implement it.
type waitSink interface {
	onPoll(name string, elapsed, nextIn, eta time.Duration)
	tick(name string, elapsed, until, eta time.Duration)
	animated() bool
	warn(format string, a ...any)
	renderDiagnosis(d wait.Diagnosis)
	interactive() bool
	// fixHook returns the installer-supplied AI-fixer hook, or nil when none was
	// set. awaitLoop uses it to decide whether the keep-waiting prompt offers the
	// [f] key.
	fixHook() FixFunc
	// keepWaiting prompts the user. onFix is non-nil only when an AI fixer is
	// available: the prompt then offers [Y/n/f], and pressing 'f' invokes onFix
	// (surfacing any error) before RE-ASKING. With onFix nil the prompt is the
	// original [Y/n].
	keepWaiting(name string, onFix func() error) bool
}

// awaitLoop polls until ready, or — on each deadline without readiness —
// surfaces diagnostics and (interactively) asks whether to keep waiting. It
// returns nil on readiness, the poll's error verbatim, ctx.Err() on parent
// cancellation, or a give-up error after a non-interactive timeout / a declined
// prompt. When optional is true the loop never consults interactive()/keepWaiting():
// it returns the timeout error immediately after surfacing diagnostics so the
// caller can continue in a degraded state.
//
// recheckBase is a context that is NOT subject to the overall --timeout; it is
// used for each keep-waiting recheck window so that an exhausted overall timeout
// does not make "Y" a no-op. Callers that don't need this distinction pass the
// same ctx for both parameters.
//
// eta is the expected time-to-ready. When non-zero, a one-time soft-warn fires
// if that duration elapses before the component is ready; the poll continues.
// Pass 0 to disable the soft-warn.
func awaitLoop(ctx, recheckBase context.Context, sink waitSink, name string, deadline time.Duration, poll Poll, diagnose Diagnose, optional bool, eta time.Duration) error {
	return awaitLoopRecheck(ctx, recheckBase, sink, name, deadline, keepWaitingRecheck, poll, diagnose, optional, eta)
}

// awaitLoopRecheck is the parameterized implementation of awaitLoop. The first
// wait uses initialDeadline under ctx; each subsequent keep-waiting round uses
// recheckWindow under recheckBase so an exhausted overall timeout cannot make
// "Y" a no-op. The test passes a tiny recheckWindow (e.g. 40ms) to keep the
// test fast.
//
// eta (when non-zero) drives a goroutine that fires a one-time soft-warn after
// that duration if the component is still not ready. The goroutine is cleaned up
// when awaitLoopRecheck returns (via warnDone channel), so there is no goroutine
// leak. The warn itself is delivered through sink.warn (mutex-protected in both
// renderer implementations), making this -race safe.
func awaitLoopRecheck(ctx, recheckBase context.Context, sink waitSink, name string, initialDeadline, recheckWindow time.Duration, poll Poll, diagnose Diagnose, optional bool, eta time.Duration) error {
	// ETA soft-warn: fire once when eta has elapsed and the component is still
	// not ready. The deferred closer signals the goroutine via warnDone and
	// then waits for it to exit (via warnWg) before awaitLoopRecheck returns.
	// Waiting ensures that any write to sink from the goroutine is sequenced
	// before the caller inspects the sink, keeping -race clean.
	var etaWarnOnce sync.Once
	if eta > 0 {
		warnDone := make(chan struct{})
		var warnWg sync.WaitGroup
		warnWg.Add(1)
		defer func() {
			close(warnDone)
			warnWg.Wait()
		}()
		go func() {
			defer warnWg.Done()
			t := time.NewTimer(eta)
			defer t.Stop()
			select {
			case <-t.C:
				etaWarnOnce.Do(func() {
					sink.warn("%s is taking longer than expected (~%s)…", name, eta.Round(time.Second))
				})
			case <-warnDone:
			}
		}()
	}

	base, window := ctx, initialDeadline
	for {
		err := pollUntilDeadline(base, sink, name, window, poll, eta)
		if err == nil {
			return nil
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return err // poll error or Ctrl-C on the active base
		}
		sink.warn("%s still not ready (%s)", name, window.Round(time.Second))
		var diag wait.Diagnosis
		if diagnose != nil {
			if d, derr := diagnose(base); derr != nil {
				sink.warn("  couldn't gather diagnostics: %v", derr)
			} else {
				diag = d
				sink.renderDiagnosis(d)
			}
		}
		if optional {
			return fmt.Errorf("%s not ready after %s: %w", name, window.Round(time.Second), context.DeadlineExceeded)
		}
		if !sink.interactive() {
			return fmt.Errorf("%s not ready after %s: %w", name, window.Round(time.Second), context.DeadlineExceeded)
		}
		// When an AI fixer is available, build the per-prompt onFix closure that
		// captures THIS component's gathered diagnostics + the SIGINT-cancelable
		// recheck base (so Ctrl-C during the AI session is honoured). keepWaiting
		// invokes it on 'f' and re-asks afterward.
		var onFix func() error
		if fh := sink.fixHook(); fh != nil {
			d := diag
			onFix = func() error { return fh(recheckBase, name, d) }
		}
		if !sink.keepWaiting(name, onFix) {
			return fmt.Errorf("%s not ready and waiting was stopped", name)
		}
		// Subsequent rounds: short recheck window, against the non-expiring base
		// (so an exhausted overall --timeout doesn't make "Y" a no-op).
		base, window = recheckBase, recheckWindow
	}
}

func pollUntilDeadline(ctx context.Context, sink waitSink, name string, deadline time.Duration, poll Poll, eta time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	start := time.Now()
	backoff := firstBackoff
	for {
		done, err := poll(dctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		sink.onPoll(name, time.Since(start), backoff, eta)
		nextAt := time.Now().Add(backoff)
		if err := animate(dctx, sink, name, start, nextAt, eta); err != nil {
			return err
		}
		if backoff < maxBackoff {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// animate waits until nextAt, animating per-frame for animated renderers and
// plain-sleeping for the rest. Returns ctx.Err() if the deadline/cancel fires.
func animate(ctx context.Context, sink waitSink, name string, start, nextAt time.Time, eta time.Duration) error {
	if !sink.animated() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(nextAt)):
			return nil
		}
	}
	ticker := time.NewTicker(frameRate)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if !time.Now().Before(nextAt) {
				return nil
			}
			sink.tick(name, time.Since(start), time.Until(nextAt), eta)
		}
	}
}

// humanETA formats a duration as a short human-readable estimate string.
// For durations ≥ 1 minute it rounds to the nearest minute and trims a
// trailing "0s" (so 7m0s → "7m"). For shorter durations it rounds to
// the nearest second (so 45s → "45s"). The caller is responsible for
// prepending "~" at the call site.
func humanETA(d time.Duration) string {
	if d >= time.Minute {
		return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	}
	return d.Round(time.Second).String()
}

// confirmKeepWaiting prompts default-yes (Enter keeps waiting). Line-based — no
// raw mode — so it composes with a live region (the caller clears/redraws around
// it) and with the rest of install's cooked-mode prompts.
//
// When onFix is non-nil the prompt reads [Y/n/f] and 'f' invokes the AI fixer,
// then RE-ASKS (the user can keep waiting or stop once the AI session returns).
// A Launch error is surfaced — never swallowed — and the prompt is re-asked. The
// caller (the renderer's keepWaiting) is responsible for quiescing the live
// region so the launched CLI owns the terminal while onFix runs.
//
// EOF or a read error (e.g. stdin redirected from /dev/null) returns false so
// `oap install < /dev/null` stops rather than hanging forever. A real empty
// Enter (nil error, line == "\n") still keeps waiting per the spec.
func confirmKeepWaiting(in io.Reader, out io.Writer, name string, onFix func() error) bool {
	reader := bufio.NewReader(in)
	for {
		if onFix != nil {
			cliout.Prompt(out, "Keep waiting for %s? [Y/n/f] (f = let an AI assistant diagnose & fix, Ctrl-C aborts): ", name)
		} else {
			cliout.Prompt(out, "Keep waiting for %s? [Y/n] (Ctrl-C aborts): ", name)
		}
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			// stdin closed / unreadable (e.g. redirected from /dev/null): cannot ask, so stop waiting.
			return false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "n", "no":
			return false
		case "f", "fix":
			if onFix == nil {
				// No fixer available: 'f' is not an offered option, so treat any
				// non-"n" input as the default (keep waiting).
				return true
			}
			if ferr := onFix(); ferr != nil {
				cliout.Warn(out, "AI fixer could not be launched: %v", ferr)
			}
			// Re-ask now the AI session (or its failed launch) has returned.
			continue
		default:
			return true
		}
	}
}

// writeDiagnosis prints a Diagnosis as plain indented lines. Both renderers use
// it (the checklist prints these above its live region).
func writeDiagnosis(out io.Writer, d wait.Diagnosis) {
	if d.Headline != "" {
		fmt.Fprintf(out, "  %s\n", d.Headline)
	}
	for _, cn := range d.Conditions {
		fmt.Fprintf(out, "  condition %s=%s   %s", cn.Type, cn.Status, cn.Reason)
		if cn.Message != "" {
			fmt.Fprintf(out, " — %s", cn.Message)
		}
		fmt.Fprintln(out)
	}
	if d.Pod != "" {
		fmt.Fprintf(out, "  pod %s   %s", d.Pod, d.PodPhase)
		if d.Reason != "" {
			fmt.Fprintf(out, " / %s", d.Reason)
		}
		fmt.Fprintln(out)
	}
	if t := d.Terminated; t != nil {
		fmt.Fprintf(out, "  container %s   exited %d", t.Container, t.ExitCode)
		if t.Reason != "" {
			fmt.Fprintf(out, " / %s", t.Reason)
		}
		fmt.Fprintln(out)
		if t.Message != "" {
			fmt.Fprintf(out, "    %s\n", t.Message)
		}
		for _, line := range t.Logs {
			fmt.Fprintf(out, "    %s\n", line)
		}
	}
	for _, pvc := range d.PVCs {
		fmt.Fprintf(out, "  PVC %s   %s\n", pvc.Name, pvc.Phase)
	}
	for _, e := range d.Events {
		if e.Count > 1 {
			fmt.Fprintf(out, "  event (×%d): %s — %s\n", e.Count, e.Reason, e.Message)
		} else {
			fmt.Fprintf(out, "  event: %s — %s\n", e.Reason, e.Message)
		}
	}
}
