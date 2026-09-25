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
// carries the redaction-safe view of the invocation the pipeline acted on, and
// is always populated — including on a deny for an unknown tool.
type Decision = core.Decision[*ParsedArgs]

// CheckRef names the rule that produced a deny.
type CheckRef = core.CheckRef

// CheckResult is one entry in the ordered trace.
type CheckResult = core.CheckResult

// Warning carries non-fatal notes (e.g. DenyEffectsUnenforceable for
// MCP deny.effects fields that have no annotation analog).
type Warning = core.Warning

// ParsedArgs is a redaction-safe, JSON-serializable view of Invocation.
// MCP-domain specific: tool name + arbitrary JSON args.
type ParsedArgs struct {
	ToolName string         `json:"toolName"`
	Args     map[string]any `json:"args,omitempty"`
}
