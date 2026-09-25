// Package besteffort centralizes the "log-and-continue" idiom for
// operations whose failure is recoverable but whose silent loss would
// leave operators without diagnostics — AGENTS.md's "Never silently
// drop errors", learned from a respond_to_user envelope that failed
// in the channelsd → Slack chain and was swallowed by three layers of
// `_, _ = …`, leaving the user with no message and no log to grep.
//
// Use Log only where the caller CANNOT return the error (a defer in a
// goroutine, a fallback delivery inside an error handler) yet silent
// failure would mask a downstream symptom. Where the caller can
// propagate, return the error instead.
package besteffort

// InfoFunc is the Info method value of either slog.Logger or
// logr.Logger — both expose `func(msg string, args ...any)`, so
// callers in either log world plug in without an adapter package.
type InfoFunc = func(msg string, args ...any)

// Log records a best-effort failure when err is non-nil, reporting
// whether it logged so callers can chain a follow-up diagnostic
// without re-checking err:
//
//	if besteffort.Log(logger.Info, "patch placeholder", err, "channel", name) {
//	    // optional extra step (e.g. surface to user via fallback)
//	}
//
// kvs is the usual structured-context list (key, value, …); err is
// prepended as ("err", err.Error()) so it is grep-able without every
// caller remembering to include it.
func Log(info InfoFunc, op string, err error, kvs ...any) bool {
	if err == nil {
		return false
	}
	args := make([]any, 0, len(kvs)+2)
	args = append(args, "err", err.Error())
	args = append(args, kvs...)
	info("best-effort: "+op+" failed", args...)
	return true
}
