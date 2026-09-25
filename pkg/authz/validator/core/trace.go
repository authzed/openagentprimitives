package core

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

// AddTrace appends an entry to Decision.Trace. All trace strings are
// redacted by Finalize before the Decision leaves the validator.
func AddTrace[T any](d *Decision[T], path, status, detail, rule string) {
	d.Trace = append(d.Trace, CheckResult{
		Path:   path,
		Status: status,
		Detail: detail,
		Rule:   rule,
	})
}

// FailedOn marks the Decision as denied, recording the failing check
// reference and a "denied by <path>: <message>" reason. When msg is
// empty (rare — every phase populates it), the reason collapses to
// "denied by <path>" alone.
//
// The message is the actionable bit (e.g. "unknown flag --limit",
// "tool not in allowlist") — it gets baked into Reason so callers that
// surface only Reason (the agent's tool_result error, the runner's
// IsError content, kubectl describe) get an informative explanation,
// not just a phase name. FailedOn is preserved structurally for callers
// that want to render the path + message separately.
//
// Defensively sets Allow=false so a future phase that forgets to return
// after FailedOn cannot leak through the happy-path Allow=true at the
// bottom of Check.
func FailedOn[T any](d *Decision[T], path, msg string) {
	d.Allow = false
	d.FailedOn = &CheckRef{Path: path, Message: msg}
	if msg == "" {
		d.Reason = fmt.Sprintf("denied by %s", path)
		return
	}
	d.Reason = fmt.Sprintf("denied by %s: %s", path, msg)
}

// Finalize applies redaction to every user-visible string on the
// Decision and emits the redaction descriptor map. Always called as the
// last step before returning a Decision. Safe with a nil Redactor — the
// per-package validator may pass nil during initialization or in tests
// that don't exercise redaction.
//
// Parsed is scrubbed HERE, generically (RedactAny walks it by shape, see
// redact.go), rather than by each validator listing its own fields. The
// contract lives on Decision.Parsed — "string values already have redaction
// applied" — so it is enforced where it is declared, and a validator added
// later inherits it. Leaving it to the per-package callback is exactly how
// ParsedCall.Tail and ParsedCall.Argv came to be printed verbatim beside a
// masked Flags and Positional.
//
// The optional scrubParsed callback remains for package-specific handling
// beyond the generic walk. Pass nil when there is none.
func Finalize[T any](d *Decision[T], r *redact.Redactor, scrubParsed func(T, *redact.Redactor)) *Decision[T] {
	if r == nil {
		return d
	}
	d.Reason = r.RedactInString(d.Reason)
	if d.FailedOn != nil {
		d.FailedOn.Message = r.RedactInString(d.FailedOn.Message)
	}
	for i := range d.Trace {
		d.Trace[i].Detail = r.RedactInString(d.Trace[i].Detail)
	}
	scrubbed := RedactAny(d.Parsed, r)
	if typed, ok := scrubbed.(T); ok {
		d.Parsed = typed
	} else {
		// RedactAny could not express the scrubbed value as a T: the parsed
		// view is a shape the walk cannot visit field by field, so it fell
		// back to a masked rendering. Drop the field rather than emit the
		// unscrubbed original — and say so, rather than dropping it silently.
		var zero T
		d.Parsed = zero
		if scrubbed != nil {
			d.Warnings = append(d.Warnings, Warning{
				Kind:    WarnParsedUnrepresentable,
				Message: "the parsed invocation could not be redacted into its own type and was omitted",
			})
		}
	}
	if scrubParsed != nil {
		scrubParsed(d.Parsed, r)
	}
	d.Redactions = r.Emit()
	return d
}
