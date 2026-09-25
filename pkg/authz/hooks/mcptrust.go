package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
)

// MCPSpecResolver resolves, for an LLM-facing tool name, the static MCP spec
// and the SERVER-SIDE tool name (the key in the spec's allowlist). The
// server-side name differs from the LLM name (<serverName>_<toolName>); the
// validator looks the tool up in the spec by the server-side name, so passing
// the LLM name would always miss the allowlist. Returns (nil, "") when the tool
// has no MCP spec (meta/sandbox tools).
type MCPSpecResolver func(llmToolName string) (spec *mcpspec.Spec, validatorToolName string)

// McpTrust ports the SEP-1913 pre-call validator deny to a PreToolCall hook.
// It runs validator.Check against the tool's static spec (allowedFields,
// deny.effects, deny.trust, constraints) and denies on the first failed rule.
// No-ops (Allow) when the tool has no MCP spec (meta/sandbox tools).
type McpTrust struct {
	specLookup MCPSpecResolver
}

// NewMcpTrust creates a McpTrust hook. specLookup resolves the MCP spec AND the
// server-side tool name for an LLM-facing tool name, returning (nil, "") when
// the tool has no MCP spec (meta/sandbox tools).
func NewMcpTrust(specLookup MCPSpecResolver) *McpTrust {
	return &McpTrust{specLookup: specLookup}
}

func (h *McpTrust) Name() string             { return "mcp_trust" }
func (h *McpTrust) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreToolCall} }

func (h *McpTrust) Eval(_ context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil || h.specLookup == nil {
		return pipeline.Decision{}
	}
	spec, validatorToolName := h.specLookup(in.Tool.Name)
	if spec == nil {
		return pipeline.Decision{} // non-MCP tool: nothing to validate
	}
	if validatorToolName == "" {
		// Resolver returned a spec but no server-side name — fall back to the
		// LLM name (single-tool / test specs are keyed by the same name).
		validatorToolName = in.Tool.Name
	}

	var argsMap map[string]any
	if len(in.Tool.Args) > 0 {
		if err := json.Unmarshal(in.Tool.Args, &argsMap); err != nil {
			// Malformed args JSON: cannot validate against the spec. Fail closed —
			// an unparseable blob must not slip past with an empty argsMap that
			// skips every arg-based rule. (Mirrors the validator-error deny below.)
			return pipeline.Decision{Verdict: pipeline.Deny, Reason: "mcp trust: unparseable tool args: " + err.Error()}
		}
	}

	// Every MCP/sandbox tool call arrives wrapped as {operation_id, _reason,
	// args} (tool.WrapInputSchema); validator.Check — and its allowedFields
	// check in particular — must see the INNER args, never the wrapper's own
	// keys. Falling through to the wrapper would check "operation_id"/
	// "_reason"/"args" against the spec's allowedFields and deny every call,
	// regardless of what the caller actually sent.
	//
	// A binding declared with no arguments marshals its inner "args" as JSON
	// null (encoding/json's nil-RawMessage MarshalJSON emits "null"), and an
	// envelope that omits the key entirely means the same thing: the caller
	// supplied no arguments. Both are a legitimate zero-argument call, so
	// they validate against an empty args map rather than being denied.
	// Only a present, non-null, non-object "args" (a string, number,
	// boolean, or array) is genuinely malformed — SEP-1913 args are always
	// an object or absent — and fails closed rather than being read as the
	// wrapper or as permission.
	if argsMap != nil {
		switch inner := argsMap["args"].(type) {
		case nil:
			argsMap = map[string]any{}
		case map[string]any:
			argsMap = inner
		default:
			return pipeline.Decision{
				Verdict: pipeline.Deny,
				Reason:  "mcp trust: tool args envelope's \"args\" field must be an object or omitted, got " + jsonValueKind(inner),
			}
		}
	}

	dec, err := validator.Check(spec, validator.Invocation{ToolName: validatorToolName, Args: argsMap})
	if err != nil {
		// The spec could not be APPLIED — malformed or unevaluatable CEL, a
		// CEL env that would not build. Fail closed, as any unevaluatable gate
		// must, but mark it: this deny is an authoring defect in the agent's
		// own definition, not a judgement about the caller, and no argument or
		// identity gets past it. Reporting it as an access denial would send
		// the one person who noticed to ask for a permission that cannot help,
		// while the operator who can fix it hears nothing.
		return pipeline.Decision{
			Verdict:    pipeline.Deny,
			Reason:     "mcp trust: validator error: " + err.Error(),
			Definition: definitionErrorFor(in.Tool.Name, spec.Name, err),
		}
	}
	if !dec.Allow {
		reason := dec.Reason
		if dec.FailedOn != nil {
			failedMsg := dec.FailedOn.Path
			if dec.FailedOn.Message != "" {
				failedMsg = dec.FailedOn.Message
			}
			reason = failedMsg
		}
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  "mcp trust denied: " + reason,
			Audit: []pipeline.AuditRecord{{
				Kind:   "mcp_trust_denied",
				Fields: map[string]any{"tool": in.Tool.Name, "failedOn": reason},
			}},
		}
	}
	return pipeline.Decision{}
}

var _ pipeline.Hook = (*McpTrust)(nil)

// definitionErrorFor translates a validator failure into the pipeline's
// operator-facing vocabulary. This hook is the last place that knows both what
// broke (a CEL rule, at a path, with an expression) and what it belongs to
// (this tool, on this MCP server); one hop later there is only a string.
//
// A ConstraintError contributes its locus and the authored expression, which
// is what actually lets an operator find the rule. Any other error — a CEL env
// that would not build, say — still yields a DefinitionError, because it is
// still the definition failing rather than the caller; it simply has no
// position to report.
func definitionErrorFor(llmToolName, serverName string, err error) error {
	subject := "tool " + strconv.Quote(llmToolName)
	if serverName != "" {
		subject += " on MCP server " + strconv.Quote(serverName)
	}
	de := &pipeline.DefinitionError{Subject: subject, Err: err}
	var ce *validator.ConstraintError
	if errors.As(err, &ce) {
		de.Locus = ce.Path
		de.Detail = ce.CEL
	}
	return de
}

// jsonValueKind names the JSON shape of a value produced by encoding/json's
// default unmarshal-into-any decoding (string, float64, bool, []any, plus
// map[string]any and nil which callers here handle before reaching this
// function), for a deny reason a human can act on without leaking a Go type
// name to the caller.
func jsonValueKind(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	default:
		return "an unrecognized value"
	}
}
