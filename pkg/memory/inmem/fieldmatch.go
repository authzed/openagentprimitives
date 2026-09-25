package inmem

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// matchesFieldEquals reports whether content satisfies every predicate in
// filters, reproducing what the SQL backends do rather than approximating it.
//
// Fidelity matters here because the unit suite runs on inmem: a backend that
// dropped FieldEquals would match everything a wrong FieldFilter.Path names,
// while postgres/sqlite match nothing, so no unit test could observe the
// mistake. Two accessors shipped broken on both SQL backends that way.
//
// The semantics mirror postgres's `content->>'key' <op> $1`:
//   - the key is looked up at the TOP LEVEL, with a dot part of the key name
//     rather than a nesting separator — both SQL backends resolve "a.b" to a
//     key literally named "a.b", so no backend reaches nested content;
//   - both sides compare as TEXT, so ordering is lexicographic, not numeric;
//     sqlite's builder casts explicitly to reproduce the same quirk;
//   - an absent key, a JSON null, or non-object content yields SQL NULL: every
//     comparison false, IsNil true, NotNil false.
func matchesFieldEquals(content []byte, filters []memory.FieldFilter) bool {
	if len(filters) == 0 {
		return true
	}
	obj := decodeContentObject(content)
	for _, ff := range filters {
		got, present := extractText(obj, ff.Path)
		op := memory.NormalizeFieldOp(ff.Op)
		if op == memory.FieldOpIsNil {
			if present {
				return false
			}
			continue
		}
		if op == memory.FieldOpNotNil {
			if !present {
				return false
			}
			continue
		}
		if !present {
			// NULL <op> anything is NULL, which WHERE treats as not-true.
			return false
		}
		want := fmt.Sprint(ff.Value)
		var ok bool
		switch op {
		case memory.FieldOpEq:
			ok = got == want
		case memory.FieldOpLt:
			ok = got < want
		case memory.FieldOpLte:
			ok = got <= want
		case memory.FieldOpGt:
			ok = got > want
		case memory.FieldOpGte:
			ok = got >= want
		default:
			// An op this matcher does not render must not silently pass the
			// entry through: that would widen the answer, which is the failure
			// mode the DroppedPredicates channel exists to make visible, and
			// this matcher has no way to report it. Exclude instead — the
			// caller sees an empty result, not a wrong one.
			ok = false
		}
		if !ok {
			return false
		}
	}
	return true
}

// decodeContentObject decodes content as a JSON object, keeping numbers as
// json.Number so their literal text survives — postgres's ->> returns the
// stored JSON text, which round-tripping through float64 would not preserve.
// Returns nil for anything that is not a JSON object; every lookup against nil
// is then absent, matching what content->>'k' yields for a non-object.
func decodeContentObject(content []byte) map[string]json.RawMessage {
	if len(content) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return nil
	}
	return obj
}

// extractText returns the text form of obj[path] and whether it is non-NULL,
// matching postgres's jsonb ->> text operator: a JSON string yields its
// unquoted value, every other type yields its JSON text.
func extractText(obj map[string]json.RawMessage, path string) (string, bool) {
	raw, ok := obj[path]
	if !ok {
		return "", false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", false
		}
		return s, true
	}
	return string(trimmed), true
}
