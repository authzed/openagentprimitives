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
// semantics. Kept as a per-package wrapper so the rest of this
// validator's code can use the short name.
func failedOn(d *Decision, path, msg string) {
	core.FailedOn(d, path, msg)
}

// finalize applies redaction to every user-visible string on the
// Decision and emits the redaction descriptor map. Always called as the
// last step before returning a Decision.
//
// No parsed-call callback: core.Finalize walks every exported field of
// ParsedCall by shape. A callback naming fields explicitly would leave any
// field it forgot — Tail and Argv carry the same raw parse — unmasked, and a
// field list kept in sync by hand is exactly that defect.
func finalize(d *Decision, r *redact.Redactor) *Decision {
	return core.Finalize[*ParsedCall](d, r, nil)
}
