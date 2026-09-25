package hooks

import (
	"encoding/json"
	"fmt"
)

// ExtractToolIDArg parses a tool's input JSON and returns the named field
// as a string. Tool calls go through an LLM-facing envelope of shape
// {operation_id, _reason, args:{...}}; the idArg names a field INSIDE args.
// Falls back to the top level for non-enveloped (flat) inputs.
// Returns "" (no error) when the field is missing or non-string; an error
// only when the JSON itself is malformed (the fail-closed signal).
func ExtractToolIDArg(inputJSON []byte, field string) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(inputJSON, &m); err != nil {
		return "", err
	}
	if args, ok := m["args"].(map[string]any); ok {
		if v, ok := args[field]; ok {
			if s, ok := v.(string); ok {
				return s, nil
			}
			return "", nil
		}
	}
	if v, ok := m[field]; ok {
		if s, ok := v.(string); ok {
			return s, nil
		}
	}
	return "", nil
}

// resolveToolArgAsExecuted returns the named argument AS THE DISPATCHER WILL
// HAND IT TO THE TOOL, and refuses a call that names it twice with different
// values.
//
// ExtractToolIDArg is deliberately NOT this. It prefers `args.<field>` whenever
// a map-valued `args` key exists at all, while the dispatcher unwraps the
// envelope only when `operation_id` is present (runner.unwrapToolArgs) and a
// meta tool unmarshals the TOP LEVEL straight into its own struct
// (tool.ParseArgs). All three readings agree on every well-formed call and
// diverge on one a model can compose for itself:
//
//	{"resource":"ledger:l-9","text":"…","args":{"resource":"dossier:d-1"}}
//
// A stray sibling `args` object. Resolved through ExtractToolIDArg a gate
// judges dossier:d-1 — a pool the session may already write to — while the
// tool writes to ledger:l-9, so the gate can be aimed at a destination other
// than the one being written to, from inside the very payload it polices.
//
// Two rules, both fail-closed. Unwrap exactly where the dispatcher does, so
// the gate and the tool read the same field. And treat the same name appearing
// at both levels with different values as UNRESOLVABLE rather than picking one:
// a call that names its destination twice is not one to guess about, and
// guessing correctly today only holds while both unwrap rules stay identical
// forever.
//
// Returns "" and no error when the field is absent or not a string — the
// caller decides what an unnamed destination means. An error means malformed
// JSON or a conflicting duplicate.
func resolveToolArgAsExecuted(inputJSON []byte, field string) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(inputJSON, &m); err != nil {
		return "", err
	}
	inner, hasInner := m["args"].(map[string]any)
	_, enveloped := m["operation_id"]

	fromTop, topOK := stringField(m, field)
	var fromInner string
	var innerOK bool
	if hasInner {
		fromInner, innerOK = stringField(inner, field)
	}
	if topOK && innerOK && fromTop != fromInner {
		return "", fmt.Errorf(
			"argument %q is named twice — %q at the top level, %q inside args — and the two readings disagree",
			field, fromTop, fromInner)
	}
	if enveloped && hasInner {
		return fromInner, nil
	}
	return fromTop, nil
}

// stringField returns m[field] and whether it was both present and a string.
func stringField(m map[string]any, field string) (string, bool) {
	v, ok := m[field]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// ExtractToolResultID parses a tool's result content as JSON and returns the
// named top-level field as a string. Used when the SpiceDB resource ID only
// appears in the response. Returns "" (no error) for empty content or a
// missing/non-string field; an error only for malformed JSON.
func ExtractToolResultID(content, field string) (string, error) {
	if content == "" {
		return "", nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return "", err
	}
	if v, ok := m[field]; ok {
		if s, ok := v.(string); ok {
			return s, nil
		}
	}
	return "", nil
}

// jsonArgs encodes an args map as JSON for scope.CheckScope. Returns {}
// for nil or empty maps; on marshal error (should not happen for plain
// map[string]any values), also returns {}.
func jsonArgs(args map[string]any) json.RawMessage {
	if len(args) == 0 {
		return json.RawMessage("{}")
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}
