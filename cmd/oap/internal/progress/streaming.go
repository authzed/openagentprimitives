package progress

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
)

// Compile-time interface assertions.
var _ Reporter = (*streaming)(nil)
var _ Phase = (*streamingPhase)(nil)
var _ waitSink = (*streamingPhase)(nil)

type streaming struct {
	mu        sync.Mutex // guards all writes to out and fix
	out       io.Writer
	stdin     io.Reader
	assumeYes bool
	fix       FixFunc // installer-supplied AI fixer; nil keeps the prompt at [Y/n]
}

func newStreaming(out io.Writer, stdin io.Reader, assumeYes bool) *streaming {
	return &streaming{out: out, stdin: stdin, assumeYes: assumeYes}
}

func (s *streaming) Phase(name string) Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	cliout.Step(s.out, "%s", name)
	return &streamingPhase{s: s, name: name, lastDecile: -1}
}
func (s *streaming) Info(f string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cliout.Info(s.out, f, a...)
}
func (s *streaming) OK(f string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cliout.OK(s.out, f, a...)
}
func (s *streaming) Warn(f string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cliout.Warn(s.out, f, a...)
}

// SetFixHook installs the AI-fixer hook used by the keep-waiting prompt.
func (s *streaming) SetFixHook(fn FixFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fix = fn
}

// Suspend runs fn with the renderer's writer. The streaming renderer has no live
// region to quiesce — output already scrolls naturally — so it only serializes
// against other prompts via promptMu (matching keepWaiting) and runs fn without
// holding s.mu so fn can block on stdin or stream a subprocess.
func (s *streaming) Suspend(fn func(out io.Writer, in io.Reader)) {
	promptMu.Lock()
	defer promptMu.Unlock()
	fn(s.out, s.stdin)
}
func (s *streaming) Close() error { return nil }

type streamingPhase struct {
	s    *streaming
	name string
	// lastDecile is the most recently printed 10%-bucket (0..10) for Progress,
	// or -1 before any Progress call. It throttles bar output on a non-TTY
	// stream to one line per decile boundary. Guarded by s.mu.
	lastDecile int
	// liveStatus is the last build step set via Status(). When non-empty it is
	// included in the onPoll log line. Guarded by s.mu.
	liveStatus string
}

func (p *streamingPhase) Detail(f string, a ...any) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	cliout.Info(p.s.out, f, a...)
}

// Progress prints a throttled progress line. It emits only when current crosses
// into a new 10% bucket (deciles 0..10), so a caller advancing the bar
// thousands of times produces at most ~11 lines. Completion (current==total)
// maps to decile 10, which always differs from any prior ≤9 bucket, so the final
// state is printed exactly once. A zero or negative total is ignored.
func (p *streamingPhase) Progress(current, total int, detail string) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if total <= 0 {
		return
	}
	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}
	decile := current * 10 / total
	if decile == p.lastDecile {
		return
	}
	p.lastDecile = decile
	if detail != "" {
		cliout.Info(p.s.out, "%s: %d/%d (%s)", p.name, current, total, detail)
	} else {
		cliout.Info(p.s.out, "%s: %d/%d", p.name, current, total)
	}
}
func (p *streamingPhase) Done() { p.DoneWith("") }

// DoneWith prints readiness with an optional trailing note (e.g. the
// "already running" re-run indicator).
func (p *streamingPhase) DoneWith(note string) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if note != "" {
		cliout.Info(p.s.out, "%s ready (%s)", p.name, note)
		return
	}
	cliout.Info(p.s.out, "%s ready", p.name)
}
func (p *streamingPhase) Fail() {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	cliout.Errf(p.s.out, "%s failed", p.name)
}

// Await delegates to awaitLoop; *streamingPhase also implements waitSink.
func (p *streamingPhase) Await(ctx, recheckBase context.Context, deadline, eta time.Duration, poll Poll, diagnose Diagnose) error {
	return awaitLoop(ctx, recheckBase, p, p.name, deadline, poll, diagnose, false, eta)
}

func (p *streamingPhase) AwaitOptional(ctx, recheckBase context.Context, deadline, eta time.Duration, poll Poll, diagnose Diagnose) error {
	return awaitLoop(ctx, recheckBase, p, p.name, deadline, poll, diagnose, true, eta)
}

func (p *streamingPhase) Skip(reason string) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	cliout.Warn(p.s.out, "continuing without %s — %s", p.name, reason)
}

// Status records the current live status string. It is included in the next
// onPoll log line. An empty string reverts to the generic "waiting" message.
// Safe to call from any goroutine; takes s.mu.
func (p *streamingPhase) Status(s string) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	p.liveStatus = s
}

// waitSink implementation for *streamingPhase.

func (p *streamingPhase) onPoll(name string, elapsed, nextIn, eta time.Duration) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if p.liveStatus != "" {
		if eta > 0 {
			cliout.Info(p.s.out, "%s: %s (%s elapsed, next check in %s, ~%s expected)",
				name, p.liveStatus, elapsed.Round(time.Second), nextIn.Round(time.Second), humanETA(eta))
		} else {
			cliout.Info(p.s.out, "%s: %s (%s elapsed, next check in %s)",
				name, p.liveStatus, elapsed.Round(time.Second), nextIn.Round(time.Second))
		}
	} else {
		if eta > 0 {
			cliout.Info(p.s.out, "waiting for %s (%s elapsed, next check in %s, ~%s expected)",
				name, elapsed.Round(time.Second), nextIn.Round(time.Second), humanETA(eta))
		} else {
			cliout.Info(p.s.out, "waiting for %s (%s elapsed, next check in %s)",
				name, elapsed.Round(time.Second), nextIn.Round(time.Second))
		}
	}
}
func (p *streamingPhase) tick(string, time.Duration, time.Duration, time.Duration) {} // not animated
func (p *streamingPhase) animated() bool                                           { return false }
func (p *streamingPhase) warn(f string, a ...any) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	cliout.Warn(p.s.out, f, a...)
}
func (p *streamingPhase) renderDiagnosis(d wait.Diagnosis) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	writeDiagnosis(p.s.out, d)
}
func (p *streamingPhase) interactive() bool {
	return cliout.IsTTY(p.s.out) && !p.s.assumeYes
}

// fixHook returns the installer-supplied AI fixer (nil keeps the prompt at [Y/n]).
func (p *streamingPhase) fixHook() FixFunc {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	return p.s.fix
}

// keepWaiting delegates to the shared confirmKeepWaiting (which renders [Y/n] or
// [Y/n/f] and handles the 'f' fixer) via Suspend, which supplies the promptMu
// serialization shared with the checklist renderer.
//
// Note: interactive() always returns false for the streaming renderer (it is
// only selected for non-TTY outputs), so this path is dead in practice but kept
// correct for completeness.
func (p *streamingPhase) keepWaiting(name string, onFix func() error) bool {
	var ok bool
	p.s.Suspend(func(out io.Writer, in io.Reader) {
		ok = confirmKeepWaiting(in, out, name, onFix)
	})
	return ok
}
