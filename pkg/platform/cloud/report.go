package cloud

import "io"

// Reporter is pkg/platform/cloud's user-facing output sink, so cloud backends
// need no dependency on cmd/oap's cliout: cmd/oap injects a cliout-backed
// adapter, tests use NopReporter. Interactive() is the TTY check that decides
// whether prompting is sensible.
type Reporter interface {
	Step(format string, args ...any)
	OK(format string, args ...any)
	Info(format string, args ...any)
	Warn(format string, args ...any)
	Interactive() bool
	// Suspend runs fn with exclusive ownership of the terminal, handing it the
	// underlying writer AND the terminal's stdin reader. A live-progress
	// reporter (cmd/oap's checklist) quiesces and clears its bottom region for
	// the duration so fn's prompt or streamed subprocess output is not drawn
	// over it; plain reporters just call fn with their writer. The stdin reader
	// lets a streamed subprocess (GcloudStreaming) inherit the real terminal, so
	// gcloud detects a TTY and renders its live spinner rather than a static
	// fallback; it is nil for reporters that own no terminal stdin.
	Suspend(fn func(out io.Writer, in io.Reader))
}

// NopReporter discards all output and reports non-interactive.
type NopReporter struct{}

func (NopReporter) Step(string, ...any)                   {}
func (NopReporter) OK(string, ...any)                     {}
func (NopReporter) Info(string, ...any)                   {}
func (NopReporter) Warn(string, ...any)                   {}
func (NopReporter) Interactive() bool                     { return false }
func (NopReporter) Suspend(fn func(io.Writer, io.Reader)) { fn(io.Discard, nil) }
