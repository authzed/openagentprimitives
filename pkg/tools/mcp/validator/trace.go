package validator

import (
	"github.com/authzed/openagentprimitives/pkg/authz/validator/core"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

// addTrace appends an entry to Decision.Trace. All trace strings are
// redacted by finalize before the Decision leaves the package.
func addTrace(d *Decision, path, status, detail, rule string) {
	core.AddTrace(d, path, status, detail, rule)
}

// failedOn marks the Decision as denied. See core.FailedOn for full
// semantics; the defensive Allow=false the docstring promises lives
// there. Kept as a per-package wrapper so the rest of this validator's
// code can use the short name.
func failedOn(d *Decision, path, msg string) {
	core.FailedOn(d, path, msg)
}

// finalize applies redaction to every user-visible string on the
// Decision and emits the redaction descriptor map. Always called as the
// last step before returning a Decision.
//
// The parsed-args scrub walks Args recursively — MCP JSON args may
// carry arbitrarily nested maps and slices, and a shallow walk would
// miss sensitive values inside nested structures.
func finalize(d *Decision, r *redact.Redactor) *Decision {
	return core.Finalize(d, r, func(pa *ParsedArgs, r *redact.Redactor) {
		if pa == nil {
			return
		}
		core.RedactAnyMap(pa.Args, r)
	})
}
