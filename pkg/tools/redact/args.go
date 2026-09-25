package redact

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Shared MCP arg-redaction helpers, used by the MCP spec validator
// (pkg/tools/mcp/validator) and the agent's MCP tool dispatch
// (pkg/agent/tool/mcp). SECURITY-SENSITIVE: it scrubs declared-sensitive
// argument values out of the audit / Decision path, so its observable
// behavior must stay byte-identical across changes.
//
// Two replacement semantics are deliberately distinct:
//
//   - RedactValueAtPath (validator): replaces the WHOLE matched value with one
//     token, after registering every scalar leaf for free-text scrubbing, and
//     keys that token on the value's canonical string — scalar form for
//     scalars, compact JSON for composites — so no byte survives.
//   - RedactLeafAtPath (agent/tool/mcp): replaces only the addressed leaf,
//     keyed on that leaf's canonical string.
//
// Both key on canonicalString, which is what makes the emitted
// ID→Descriptor map faithful: two distinct values can never collide on one
// token. Both share one array-index-aware path walker and the Stringify /
// DeepCopyJSONMap primitives, and both fail CLOSED on a malformed declared
// path — see resolveForRedaction.

// Stringify renders a scalar leaf as the raw string the Redactor records
// for free-text scrubbing. Strings pass through; other JSON scalars use a
// minimal %v-equivalent form so the Redactor's value→token cache dedupes
// identical leaves across paths. Values with no such form — maps, slices,
// nil, and scalar kinds not enumerated here — return "". Callers must not
// treat that "" as a value: route every declared-sensitive value through
// canonicalString, which falls back to JSON so nothing lands unrepresented.
func Stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

// DeepCopyJSONMap returns an independent copy of a JSON-shaped
// map[string]any, recursively copying nested maps and slices. Callers
// redact (mutate) the copy in place while keeping the caller's original
// args intact for any later use (e.g. CEL evaluation).
func DeepCopyJSONMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = DeepCopyJSONValue(v)
	}
	return out
}

// DeepCopyJSONValue deep-copies an arbitrary JSON-shaped value (maps and
// slices recursively; scalars by value).
func DeepCopyJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return DeepCopyJSONMap(t)
	case []any:
		cp := make([]any, len(t))
		for i := range t {
			cp[i] = DeepCopyJSONValue(t[i])
		}
		return cp
	default:
		return v
	}
}

// canonicalString renders any sensitive value as a single non-empty
// string to register and use as its replacement token's keyed value.
//
// Returns "" only when there is genuinely nothing to protect: nil, the
// empty string, or a value encoding/json itself cannot render (which
// therefore cannot serialize into the emitted args either). Every other
// value gets a form, because "" is how a caller decides to LEAVE A VALUE
// IN PLACE with no descriptor emitted — so any shape that falls through to
// "" is a shape that serializes in cleartext while the audit record stays
// silent about it. That is why the JSON fallback covers all remaining
// values and not just composites: a declared-sensitive int32 or
// json.Number is as much a secret as a declared-sensitive string, and
// Stringify does not enumerate either.
//
// Strings short-circuit ahead of the fallback so the empty string keeps
// its "nothing to protect" meaning rather than canonicalizing to the
// two-byte JSON literal `""`.
func canonicalString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if s := Stringify(v); s != "" {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return ""
}

// pathSeg is one step of a parsed dotted path. If index ≥ 0 the segment
// addresses an array element; otherwise key names a map child.
type pathSeg struct {
	key   string
	index int
}

// parsePath splits "args.tags[2].name" into segments. Bracketed integer
// indices are treated as array steps. Dotted-only paths ("a.b.c") yield
// segments all carrying index = -1.
//
// A bracket that is unterminated ("tags[2") or whose contents are not an
// integer ("tags[x]") is an authoring error in the declaring spec, and is
// returned as one. The segments that parsed cleanly BEFORE the bad bracket
// are returned alongside the error rather than discarded: they address a
// strict ancestor of the value the author meant to protect, which is what
// lets resolveForRedaction fail closed instead of skipping.
func parsePath(p string) ([]pathSeg, error) {
	parts := strings.Split(p, ".")
	out := make([]pathSeg, 0, len(parts))
	for _, part := range parts {
		// "tags[2]" → emit "tags" then index 2.
		for {
			lb := strings.IndexByte(part, '[')
			if lb < 0 {
				if part != "" {
					out = append(out, pathSeg{key: part, index: -1})
				}
				break
			}
			if lb > 0 {
				out = append(out, pathSeg{key: part[:lb], index: -1})
			}
			rb := strings.IndexByte(part, ']')
			if rb <= lb {
				return out, fmt.Errorf("declared sensitive path %q is malformed: unterminated array index at %q", p, part[lb:])
			}
			idx, err := strconv.Atoi(part[lb+1 : rb])
			if err != nil {
				return out, fmt.Errorf("declared sensitive path %q is malformed: array index %q is not an integer", p, part[lb+1:rb])
			}
			out = append(out, pathSeg{key: "", index: idx})
			part = part[rb+1:]
			if strings.HasPrefix(part, ".") {
				part = part[1:]
			}
			if part == "" {
				break
			}
		}
	}
	return out, nil
}

// leafRef points at the slot a resolved path addresses: either a key in
// a map or an index in a slice. Exactly one of (mapParent, sliceParent)
// is non-nil. get reads the current value at the slot; set writes it.
type leafRef struct {
	mapParent   map[string]any
	key         string
	sliceParent []any
	index       int
}

func (l leafRef) get() any {
	if l.mapParent != nil {
		return l.mapParent[l.key]
	}
	return l.sliceParent[l.index]
}

func (l leafRef) set(v any) {
	if l.mapParent != nil {
		l.mapParent[l.key] = v
		return
	}
	l.sliceParent[l.index] = v
}

// resolveLeaf walks the parsed segments from root m and returns the slot
// the final segment addresses. ok=false when any non-final segment is
// missing or shape-mismatched, when the final container/segment shapes
// disagree, or when the final map key is absent. This is the shared,
// array-index-aware walker; dotted-only paths fall out as the special
// case where every segment has index = -1.
func resolveLeaf(m map[string]any, segs []pathSeg) (leafRef, bool) {
	if len(segs) == 0 {
		return leafRef{}, false
	}
	cur := any(m)
	for i := 0; i < len(segs)-1; i++ {
		seg := segs[i]
		switch v := cur.(type) {
		case map[string]any:
			if seg.index >= 0 {
				return leafRef{}, false // shape mismatch
			}
			child, ok := v[seg.key]
			if !ok {
				return leafRef{}, false
			}
			cur = child
		case []any:
			if seg.index < 0 || seg.index >= len(v) {
				return leafRef{}, false
			}
			cur = v[seg.index]
		default:
			return leafRef{}, false
		}
	}
	last := segs[len(segs)-1]
	switch v := cur.(type) {
	case map[string]any:
		if last.index >= 0 {
			return leafRef{}, false
		}
		if _, ok := v[last.key]; !ok {
			return leafRef{}, false
		}
		return leafRef{mapParent: v, key: last.key}, true
	case []any:
		if last.index < 0 || last.index >= len(v) {
			return leafRef{}, false
		}
		return leafRef{sliceParent: v, index: last.index}, true
	}
	return leafRef{}, false
}

// argDescriptor is the descriptor both callers attach to a redacted MCP
// argument. label is the original path string so the emitted
// ID→descriptor map retains call-site context.
func argDescriptor(label string) Descriptor {
	return Descriptor{Description: "mcp arg", Kind: "arg", Name: label}
}

// malformedPathDescriptor is argDescriptor for the fail-closed branch. Name
// still carries the declared path, because that is what traces the finding
// back to the offending spec — but what actually got redacted is the
// enclosing value, not the leaf that path names. The Description says so, so
// the emitted ID→Descriptor map never asserts a resolution that did not
// happen: a reader who finds a whole object collapsed into one token can see
// that the spec, not the value, is what went wrong.
func malformedPathDescriptor(label string) Descriptor {
	return Descriptor{
		Description: "mcp arg (declared path is malformed; the enclosing value was redacted whole)",
		Kind:        "arg",
		Name:        label,
	}
}

// resolveForRedaction parses path, resolves it against m, and returns the slot
// to rewrite together with the descriptor to record for it.
//
// On a malformed path it fails CLOSED rather than skipping. parsePath hands
// back the segments that parsed cleanly before the bad bracket, and those
// address a strict ANCESTOR of the value the author meant to protect — so
// redacting what they resolve to still covers the secret. The direction is
// over-redaction, never under-redaction. Skipping instead would leave a value
// the spec declared sensitive sitting in cleartext in the emitted args, which
// trades a wrong-but-safe audit record for an actual leak.
//
// parseErr is returned rather than swallowed. This package knows the path and
// the parse fault; only the caller knows the session and tool an operator
// needs in order to find the spec that declared it.
func resolveForRedaction(m map[string]any, path, label string) (leafRef, Descriptor, bool, error) {
	segs, parseErr := parsePath(path)
	desc := argDescriptor(label)
	if parseErr != nil {
		desc = malformedPathDescriptor(label)
	}
	ref, ok := resolveLeaf(m, segs)
	return ref, desc, ok, parseErr
}

// redactedErr and unresolvedErr wrap a parse fault with what the redactor did
// about it, so a caller's log line says both. A nil parseErr means the path was
// well-formed: a path that simply does not match the args is a no-op by
// contract, not an error.
func redactedErr(parseErr error) error {
	if parseErr == nil {
		return nil
	}
	return fmt.Errorf("%w; failed closed by redacting the enclosing value whole instead", parseErr)
}

// unresolvedErr covers the one malformed-path outcome that is NOT fail-closed:
// the surviving prefix did not resolve either, so no redaction was applied at
// all. Nothing addressable was left to over-redact, but the declaration went
// unhonored and the caller must be able to say so.
func unresolvedErr(parseErr error) error {
	if parseErr == nil {
		return nil
	}
	return fmt.Errorf("%w; nothing resolved, so NO redaction was applied for it", parseErr)
}

// RedactLeafAtPath resolves path against m (array-index-aware) and
// replaces ONLY the addressed leaf with a Redactor token keyed on the
// leaf's canonical string. A well-formed path that does not match the args
// is a no-op; a leaf with nothing to protect (nil, the empty string) is left
// in place. This is the agent/tool/mcp dispatch semantics: a single leaf in,
// a single leaf out.
//
// Returns a non-nil error when path itself is malformed — see
// resolveForRedaction for why that still redacts. Callers MUST log it with
// their own session/tool context; a malformed declaration is an authoring bug
// that is otherwise invisible, because its only symptom is an over-redacted
// value that looks deliberate.
func RedactLeafAtPath(m map[string]any, path string, r *Redactor, label string) error {
	ref, desc, ok, parseErr := resolveForRedaction(m, path, label)
	if !ok {
		return unresolvedErr(parseErr)
	}
	// canonicalString, not Stringify: every composite stringifies to "", so a
	// Stringify key would collide all of them onto one cache entry and hand
	// two different secret objects the same token — scrubbed, but with an
	// emitted map claiming the second redaction was the first.
	if cs := canonicalString(ref.get()); cs != "" {
		ref.set(r.Redact(cs, desc))
	}
	return redactedErr(parseErr)
}

// RedactValueAtPath resolves path against m (array-index-aware) and, for
// the matched value, (1) registers every scalar leaf so any literal
// occurrence is scrubbed from free-form text, and (2) replaces the WHOLE
// value with a single token keyed on its canonical string so NO byte of
// a declared-sensitive value survives serialization. A nil / empty
// canonical form means there is nothing to protect — the value is left
// in place. A well-formed path that does not match the args is a no-op.
// This is the MCP spec validator's whole-value semantics.
//
// Returns a non-nil error when path itself is malformed, on the same
// fail-closed-and-report contract as RedactLeafAtPath.
func RedactValueAtPath(m map[string]any, path string, r *Redactor, label string) error {
	ref, desc, ok, parseErr := resolveForRedaction(m, path, label)
	if !ok {
		return unresolvedErr(parseErr)
	}
	v := ref.get()
	// Prime every scalar leaf for free-text redaction (catches the same
	// secret reappearing in a CEL message or trace detail).
	registerLeaves(r, v, desc)
	// Replace the whole value with one token so NO byte of it survives
	// serialization. An empty canonical form means there is no value to
	// protect (nil / empty string) — leave it.
	if cs := canonicalString(v); cs != "" {
		ref.set(r.Redact(cs, desc))
	}
	return redactedErr(parseErr)
}

// registerLeaves recurses v and registers every scalar leaf's canonical
// string form with the redactor, descending maps and slices. Composite
// nodes themselves are not registered here — their leaves are; the whole
// composite is separately registered via its canonical JSON form by the
// caller.
//
// canonicalString, not Stringify: a leaf of a kind Stringify does not
// enumerate would otherwise register as "", which RegisterSensitive drops,
// silently leaving that leaf unscrubbed in every Reason / Trace / FailedOn
// string the Decision emits.
func registerLeaves(r *Redactor, v any, d Descriptor) {
	switch t := v.(type) {
	case map[string]any:
		for _, child := range t {
			registerLeaves(r, child, d)
		}
	case []any:
		for _, child := range t {
			registerLeaves(r, child, d)
		}
	default:
		r.RegisterSensitive(canonicalString(v), d)
	}
}
