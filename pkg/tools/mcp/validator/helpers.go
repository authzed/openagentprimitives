package validator

import (
	"encoding/json"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// findTool looks up a tool in the spec's allowlist by exact name.
func findTool(sp *mcpspec.Spec, name string) (*mcpspec.Tool, bool) {
	if sp == nil {
		return nil, false
	}
	for i := range sp.Tools {
		if sp.Tools[i].Name == name {
			return &sp.Tools[i], true
		}
	}
	return nil, false
}

// ContainsEnum returns true when a SEP-1913 enum field (which may be
// a string OR a JSON array of strings — "possibles" semantics from
// tools/list) contains target. Empty / null / structurally-wrong raw
// returns false (fail-open at the helper; the caller decides what
// empty means).
func ContainsEnum(raw json.RawMessage, target string) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == target
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, v := range arr {
			if v == target {
				return true
			}
		}
	}
	return false
}

// ContainsEnumField reads a named property out of an object encoded
// as raw JSON and feeds it to ContainsEnum. Both an empty raw and a
// raw that isn't a JSON object return false.
//
// This convenience wrapper collapses "absent" and "unparseable" into a
// single false — fine for callers that only care about a positive
// match. Callers enforcing an opt-in DENY axis must NOT use this:
// silently treating a corrupt (possibly attacker-controlled) metadata
// blob as "no match" fails OPEN. Use ContainsEnumFieldChecked there and
// fail closed on parseOK == false.
func ContainsEnumField(raw json.RawMessage, field, target string) bool {
	match, _ := ContainsEnumFieldChecked(raw, field, target)
	return match
}

// ContainsEnumFieldChecked is the parse-error-aware form of
// ContainsEnumField. It distinguishes:
//
//   - (false, true)  — the blob parsed as a JSON object; the field is
//     absent or present-but-no-match (legitimate "no match").
//   - (true,  true)  — the blob parsed and the field contains target.
//   - (false, false) — the blob is non-empty but does NOT parse as a
//     JSON object (truncated, a bare string/array, attacker-corrupt).
//     The caller cannot conclude "no match"; on a deny axis it must
//     fail CLOSED.
//
// An empty raw is treated as a clean no-match (false, true): SEP-1913
// metadata is optional, so absence is not an error.
func ContainsEnumFieldChecked(raw json.RawMessage, field, target string) (match, parseOK bool) {
	if len(raw) == 0 {
		return false, true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, false
	}
	return ContainsEnum(obj[field], target), true
}
