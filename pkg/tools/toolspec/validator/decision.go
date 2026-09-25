package validator

import "github.com/authzed/openagentprimitives/pkg/authz/validator/core"

// Status values for CheckResult.Status. Re-exported from core so
// existing callers keep their import path.
const (
	StatusPass            = core.StatusPass
	StatusFail            = core.StatusFail
	StatusSkip            = core.StatusSkip
	StatusOverrideApplied = core.StatusOverrideApplied
)

// Decision is the validator's structured output. Parsed (JSON key `parsed`)
// carries the parsed argv as the pipeline saw it, populated whenever the parse
// step succeeds, even when a later phase fails. Its string values already have
// redaction applied — see ParsedCall for what that covers.
type Decision = core.Decision[*ParsedCall]

// CheckRef names the rule that produced a deny.
type CheckRef = core.CheckRef

// CheckResult is one entry in the ordered trace.
type CheckResult = core.CheckResult

// Warning carries non-fatal notes (e.g., VersionUnverified).
type Warning = core.Warning

// ParsedCall is a redaction-safe, JSON-serializable view of the parser.Call.
// CLI-domain specific: subcommand path + flags + positional args.
//
// Redaction-safe means EVERY field below, not a subset: finalize hands the
// whole struct to core.Finalize, which walks it by shape (strings, slices of
// strings, nested maps, and anything a later field turns out to be) and
// replaces each registered sensitive value with its token. A value is
// registered when the toolkit marks its flag or env var sensitive, or when
// the spec lists it under sensitive.{flags,env,positional}.
//
// Two limits, stated rather than left to be discovered:
//
//   - Map KEYS are not rewritten. Flags and Positional are keyed by names the
//     toolkit declares, never by values. See core.RedactAnyMap.
//   - Nothing here can mask a value the redactor was never told about, so a
//     secret passed through a flag or slot that no one declared sensitive is
//     printed as given. That is a spec-authoring gap, not a walk gap.
type ParsedCall struct {
	Subcommand     string         `json:"subcommand"`
	SubcommandPath []string       `json:"subcommandPath,omitempty"`
	Flags          map[string]any `json:"flags,omitempty"`
	Positional     map[string]any `json:"positional,omitempty"`
	Tail           []string       `json:"tail,omitempty"`
	Argv           []string       `json:"argv,omitempty"`
}
