package tool

import (
	"encoding/json"
	"fmt"
)

// Tool-call envelope field names. Every kind that participates in the
// operation-audit protocol (sandbox, mcp, future kinds) wraps its
// per-tool args under these field names. Promoted to constants so the
// dispatcher, the runner system prompt, and every InputSchema builder
// reference one source of truth.
const (
	// OperationIDField names the audit-trail field linking each tool
	// call to a logical operation registered via new_operation.
	OperationIDField = "operation_id"

	// ReasonField names the audit-trail field carrying the LLM's
	// per-call rationale.
	ReasonField = "_reason"

	// ArgsField names the per-tool args envelope. Kinds embed their
	// per-tool input schema under this key.
	ArgsField = "args"
)

// MissingOpCtxMessage is the canonical error string the dispatcher
// returns when a tool call lacks the operation_id / _reason envelope
// fields. The runner system prompt embeds this verbatim so the LLM
// recognises the failure mode if it reaches the model trace.
const MissingOpCtxMessage = "operation_id and _reason are required"

// WrapInputSchema wraps argsSchema in the operation_id + _reason
// envelope every kind that participates in the operation-audit
// protocol expects. argsSchema may be any valid JSON-Schema fragment
// (object, primitive, array, …); it is embedded under the "args" key
// verbatim. Optional extraProps are merged in as additional sibling
// properties (e.g. sandbox's "stdin"). Returns the marshaled wrapper
// schema.
//
// Replaces the inlined-template + struct-marshal patterns in
// pkg/agent/tool/{sandbox,mcp}.
func WrapInputSchema(argsSchema json.RawMessage, extraProps ...map[string]json.RawMessage) json.RawMessage {
	if len(argsSchema) == 0 {
		argsSchema = json.RawMessage(`{"type":"object"}`)
	}
	props := map[string]json.RawMessage{
		OperationIDField: json.RawMessage(`{"type":"string","description":"ID returned by new_operation. The call is rejected if the operation_id is unknown."}`),
		ReasonField:      json.RawMessage(`{"type":"string","description":"Why this specific call is needed for the operation. Recorded in the audit log."}`),
		ArgsField:        argsSchema,
	}
	for _, extra := range extraProps {
		for k, v := range extra {
			props[k] = v
		}
	}
	wrapper := struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}{
		Type:       "object",
		Properties: props,
		Required:   []string{OperationIDField, ReasonField, ArgsField},
	}
	out, err := json.Marshal(wrapper)
	if err != nil {
		// Defensively fall back to a minimal valid schema so the
		// runner can keep starting; the inputs above are all
		// well-formed in practice.
		return json.RawMessage(`{"type":"object","properties":{"operation_id":{"type":"string"},"_reason":{"type":"string"},"args":{"type":"object"}},"required":["operation_id","_reason","args"]}`)
	}
	return out
}

// ArgParseError returns the canonical IsError tool.Result for an arguments-JSON
// decode failure, in a form the LLM can retry from. hint is the JSON shape to
// retry with, e.g. `{"text": "the message body"}`. Authored once here so every
// Execute reports the contract identically:
//
//	"<toolName>: invalid arguments JSON: <err>. Expected <hint>."
func ArgParseError(toolName, hint string, err error) Result {
	return Result{
		Content: fmt.Sprintf("%s: invalid arguments JSON: %v. Expected %s.",
			toolName, err, hint),
		IsError: true,
		// Framework-generated retry guidance (the tool name, hint, and Go
		// unmarshal error), not tool/third-party output. Trusted is only
		// consulted for meta tools; every caller of ArgParseError is a meta
		// tool's Execute, so opting out of content-guard inspection here is
		// safe repo-wide.
		Trusted: true,
	}
}

// ParseArgs unmarshals raw into out and, on failure, returns a
// canonical IsError tool.Result via ArgParseError plus ok=false.
// Callers chain it as:
//
//	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"text":"…"}`); !ok {
//	    return res, nil
//	}
//
// Avoids the repeated `if err := json.Unmarshal(...); err != nil {
// return tool.Result{...}, nil }` block at every Execute entry.
func ParseArgs(raw json.RawMessage, out any, toolName, hint string) (Result, bool) {
	if err := json.Unmarshal(raw, out); err != nil {
		return ArgParseError(toolName, hint, err), false
	}
	return Result{}, true
}
