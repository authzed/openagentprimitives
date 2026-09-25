// Package core holds the shared decision/trace types both validator
// packages — pkg/tools/toolspec/validator (CLI argv) and pkg/tools/mcp/validator
// (MCP JSON args) — return.
//
// The package exists because the two validators agreed on the same
// outcome surface (Allow + Reason + FailedOn + Trace + Warnings +
// Redactions) but each declared its own copies of the types + helpers.
// Drift was inevitable. Consolidating here lets each validator focus on
// its domain (per-phase rule evaluation) without re-implementing the
// decision shape.
//
// Generic parameter `T` is the per-package "parsed invocation" type —
// CLI argv (toolspec's *ParsedCall) or MCP JSON args (mcp's
// *ParsedArgs). It lives on `Decision.Parsed` so callers see exactly
// one parsed-invocation field with a typed shape.
package core

import "github.com/authzed/openagentprimitives/pkg/tools/redact"

// Status values for CheckResult.Status. Identical across both
// validators by contract; renderers in either CLI surface
// (`oap tools mcp explain`, `oap tools toolspec explain`) print these verbatim.
const (
	StatusPass            = "pass"
	StatusFail            = "fail"
	StatusSkip            = "skip"
	StatusOverrideApplied = "override-applied"
)

// Decision is the validator's structured reply: did the call pass, why
// or why not, what was checked along the way, and what redactions were
// applied to the trace. Generic in T so each validator can attach its
// own typed "parsed invocation" view without losing type safety at the
// call site. Whatever T is, the Parsed field marshals under the key `parsed`.
type Decision[T any] struct {
	// Allow is the verdict; false always comes with a Reason.
	Allow bool `json:"allow"`
	// Reason explains a deny in one line; empty on allow.
	Reason string `json:"reason,omitempty"`
	// FailedOn names the rule that denied; nil on allow.
	FailedOn *CheckRef `json:"failedOn,omitempty"`
	// Trace is every check in evaluation order; empty when nothing ran.
	Trace []CheckResult `json:"trace,omitempty"`
	// Warnings are non-fatal notes that did not affect the verdict.
	Warnings []Warning `json:"warnings,omitempty"`
	// Redactions maps each emitted token back to what kind of secret it replaced.
	Redactions map[string]redact.Descriptor `json:"redactions,omitempty"`

	// Parsed is the per-package redaction-safe view of the invocation
	// the pipeline acted on. Concrete type is the package's own
	// *ParsedCall / *ParsedArgs (or any other shape a future validator
	// chooses for its domain).
	//
	// Redaction-safe means: Finalize walks EVERY exported string-bearing
	// field, recursively and by shape, replacing each registered sensitive
	// value with its token — see RedactAny. It must stay shape-driven for
	// whatever T a validator picks; a hand-maintained per-package field list
	// silently misses fields and makes this promise false. The one documented
	// limit is that map KEYS are not rewritten (see RedactAnyMap).
	Parsed T `json:"parsed,omitempty"`
}

// CheckRef names the rule that produced a deny.
type CheckRef struct {
	// Path identifies the denying rule within the policy document.
	Path string `json:"path"`
	// Message is the rule's own explanation; empty falls back to Decision.Reason.
	Message string `json:"message,omitempty"`
}

// CheckResult is one entry in the ordered trace.
type CheckResult struct {
	// Path identifies the check within the policy document.
	Path string `json:"path"`
	// Status is one of the Status* constants above, printed verbatim by the CLIs.
	Status string `json:"status"`
	// Detail is human-facing context; empty when the status speaks for itself.
	Detail string `json:"detail,omitempty"`
	// Rule is the rule text that produced this result; empty for built-in checks.
	Rule string `json:"rule,omitempty"`
}

// Warning carries non-fatal notes (e.g., VersionUnverified for toolspec,
// DenyEffectsUnenforceable for MCP fields with no annotation analog).
type Warning struct {
	// Kind is a stable machine-readable tag callers may switch on.
	Kind string `json:"kind"`
	// Message is the human-facing text.
	Message string `json:"message"`
}

// WarnParsedUnrepresentable is the Warning.Kind Finalize records when the
// parsed view could not be scrubbed back into its own type and was therefore
// omitted from the Decision. Dropping the field is the fail-closed choice —
// emitting an unscrubbed one would leak — but it is never done silently.
const WarnParsedUnrepresentable = "ParsedUnrepresentable"
