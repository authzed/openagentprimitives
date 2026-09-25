// Package deplogs disables zerolog's process-global logger for dependencies
// that use it, so library chatter cannot reach terminals or component logs.
//
// SpiceDB v1.54 uses its own Nop logger for schema compilation. Earlier
// releases used zerolog's package-global logger, which defaults to stderr;
// keep this guard so a dependency change cannot restore that noisy behavior.
//
// Silencing it costs nothing: none of our own logging goes through zerolog
// (components use logr/zap, klog, or slog), and this is the only non-test file
// in the tree that imports it.
//
// SpiceDB's current internal Nop logger and the operator's explicit zap logger
// are separate from zerolog's process-global logger.
package deplogs

import "github.com/rs/zerolog"

// init silences dependency logging for anything that links this package,
// including code that has no main() to call Silence from: `go test` binaries,
// and the in-process e2e harness, which constructs component code directly
// rather than exec'ing a built binary.
//
// Packages that compile SpiceDB schemas blank-import deplogs so the guard
// follows them into every binary and in-process test harness.
func init() { Silence() }

// Silence mutes libraries that log to process-global loggers. It is
// idempotent, safe to call from more than one place, and safe to call
// concurrently (zerolog's global level is an atomic).
//
// Call it as the first statement of main(); init above also covers consumers
// without a main function.
func Silence() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
}
