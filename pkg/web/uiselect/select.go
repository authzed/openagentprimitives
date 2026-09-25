// Package uiselect is the agent-UI binding selector language: the total,
// non-executing path expression a uicomponents.Binding uses to say WHICH part
// of a resolved source value fills a component prop.
//
// A binding's value lands on a prop verbatim, but a real endpoint answers with
// an envelope: ap:table.rows needs []map[string]any and a CRM search returns
// {"results":[…],"total":…,"paging":…} with each row's fields under
// ".properties". "results[].properties" is that gap.
//
// The language is deliberately two productions wide: no filters, wildcards,
// indices, recursive descent, functions, or arithmetic — nothing that could
// execute, allocate unboundedly, or backtrack. Parse is a single-pass scanner
// (not a regexp) and Apply recurses once per parsed segment, of which Parse
// admits at most MaxSegments.
//
// Grammar:
//
//	selector := segment ( "." segment )*
//	segment  := key [ "[]" ]
//	key      := one or more characters, none of them '.', '[' or ']'
//
// Keys match JSON object keys EXACTLY (JSON is case sensitive). There is no
// escape form, so a JSON key containing '.', '[' or ']' is unreachable —
// accepted, because every additional form is one this package, the validator
// and the error taxonomy must carry forever.
package uiselect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// MaxSelectorLen bounds the raw selector string, MaxSegments the parsed segment
// count. Checked in Parse, never in Apply: an over-large selector is rejected in
// the AUTHOR's build rather than on a viewer's page, and Apply's recursion depth
// is bounded before any data is touched.
const (
	MaxSelectorLen = 256
	MaxSegments    = 16
)

// maxFoundKeys caps how many object keys a MissError reports: the keys are the
// error's whole diagnostic value ("it is `results`, not `items`"), but an
// unbounded list lets an upstream document write an unbounded log line.
const maxFoundKeys = 24

// Sentinel errors. Every error this package returns wraps exactly one of
// these, so a caller can classify without string matching.
var (
	// ErrSyntax — the selector is not well formed.
	ErrSyntax = errors.New("uiselect: selector is not well formed")
	// ErrTooLong — the selector string exceeds MaxSelectorLen.
	ErrTooLong = errors.New("uiselect: selector is too long")
	// ErrTooManySegments — the selector has more than MaxSegments segments.
	ErrTooManySegments = errors.New("uiselect: selector has too many segments")
	// ErrNoMatch — a key the selector names is absent from the object at
	// that point. This is the error that MUST NOT become an empty result.
	ErrNoMatch = errors.New("uiselect: selector matched nothing")
	// ErrNotObject — the selector tried to read a key from something that is
	// not a JSON object.
	ErrNotObject = errors.New("uiselect: selector expected an object")
	// ErrNotArray — a segment ended in "[]" but the value there is not a
	// JSON array.
	ErrNotArray = errors.New("uiselect: selector expected an array")
	// ErrValueNotJSON — the resolved value is not valid JSON, so no selector
	// can be applied to it.
	ErrValueNotJSON = errors.New("uiselect: value is not valid JSON")
)

// Segment is one step of a parsed selector.
type Segment struct {
	// Key is the JSON object key this step reads. Never empty: Parse rejects
	// an empty key rather than reading it as "the current value", since
	// "a..b" is a typo, and a typo that silently means no-op is the
	// silent-wrong class this package exists to avoid.
	Key string
	// Iterate is true when the segment was written with a trailing "[]".
	// The value at Key must then be an array, and the REMAINING segments are
	// applied to each element in turn.
	Iterate bool
}

// String renders a segment as the author would have written it, e.g.
// "results[]" or "total". Used to populate MissError.Segment.
func (s Segment) String() string {
	if s.Iterate {
		return s.Key + "[]"
	}
	return s.Key
}

// Selector is a PARSED selector. Apply is a method on this type and takes no
// string, so applying an unparsed selector is structurally impossible: the
// author-time gate (uicomponents.validateBindings) and the runtime application
// cannot disagree about what is well formed.
type Selector struct {
	segments []Segment
	raw      string
}

// Empty reports whether this selector selects the whole value. The zero
// Selector is empty, which is what a binding with no `select` parses to.
func (s Selector) Empty() bool { return len(s.segments) == 0 }

// String returns the selector as the author wrote it.
func (s Selector) String() string { return s.raw }

// Segments returns a copy of the parsed segments, for callers that need to
// describe a selector (an error message, a test). A copy, so a caller cannot
// mutate a parsed Selector's meaning after it has been validated.
func (s Selector) Segments() []Segment {
	if len(s.segments) == 0 {
		return nil
	}
	out := make([]Segment, len(s.segments))
	copy(out, s.segments)
	return out
}

// Parse scans sel into a Selector. The EMPTY string parses to the empty
// Selector with a nil error: "no selector" is legal and common, and making
// callers special-case it would put the same `if sel != ""` branch at every
// call site, where a forgotten one silently skips validation.
func Parse(sel string) (Selector, error) {
	if sel == "" {
		return Selector{}, nil
	}
	// The length bound is checked before any scanning, so a pathological
	// string is cheap to reject regardless of what it contains.
	if len(sel) > MaxSelectorLen {
		return Selector{}, fmt.Errorf("uiselect: selector of %d bytes exceeds MaxSelectorLen (%d): %w", len(sel), MaxSelectorLen, ErrTooLong)
	}

	var segments []Segment
	pos := 0
	n := len(sel)
	for {
		// A key is one or more characters that are none of '.', '[', ']'.
		start := pos
		for pos < n && sel[pos] != '.' && sel[pos] != '[' && sel[pos] != ']' {
			pos++
		}
		key := sel[start:pos]
		if key == "" {
			return Selector{}, fmt.Errorf("uiselect: empty key at byte %d in %q: %w", start, sel, ErrSyntax)
		}

		iterate := false
		if pos < n && sel[pos] == '[' {
			// The only legal bracket form is "[]" — no index, no wildcard, no
			// filter expression. Anything else between the brackets, or an
			// unterminated '[', is ErrSyntax.
			if pos+1 >= n || sel[pos+1] != ']' {
				return Selector{}, fmt.Errorf("uiselect: malformed \"[]\" at byte %d in %q: %w", pos, sel, ErrSyntax)
			}
			pos += 2
			iterate = true
		}

		// A ']' reaching here has no matching '[' before it — the key scan
		// above stops at ']' just as it stops at '[', so a lone ']' (or one
		// left over after an already-consumed "[]") lands here.
		if pos < n && sel[pos] == ']' {
			return Selector{}, fmt.Errorf("uiselect: unmatched ']' at byte %d in %q: %w", pos, sel, ErrSyntax)
		}

		segments = append(segments, Segment{Key: key, Iterate: iterate})
		// Checked during the scan, not after it, so a deep selector is
		// rejected before the rest of the string is walked.
		if len(segments) > MaxSegments {
			return Selector{}, fmt.Errorf("uiselect: selector has more than %d segments: %w", MaxSegments, ErrTooManySegments)
		}

		if pos == n {
			break
		}
		if sel[pos] != '.' {
			// The key scan only ever stops at '.', '[', or ']'; '[' and ']'
			// are both handled above, so reaching here means "[]" was
			// followed by something other than '.' or end-of-string (e.g.
			// "a[]b").
			return Selector{}, fmt.Errorf("uiselect: unexpected character %q at byte %d in %q: %w", sel[pos], pos, sel, ErrSyntax)
		}
		pos++ // consume '.'
		if pos == n {
			return Selector{}, fmt.Errorf("uiselect: trailing '.' in %q: %w", sel, ErrSyntax)
		}
	}

	return Selector{segments: segments, raw: sel}, nil
}

// MissError is the structured form of a selector that did not match. It is
// operator-facing and NEVER rendered to a browser: it names an upstream API's
// internal field layout, which a viewer cannot act on.
//
// Found carries the object KEYS present at the failing segment — keys only,
// never values, which are upstream data and must never reach a log line.
type MissError struct {
	Selector string   // the selector as written
	Index    int      // 0-based position of the failing segment
	Segment  string   // that segment as written, e.g. "results[]"
	Element  int      // -1 outside an iteration; else the failing element's index
	Kind     string   // what was actually there: object|array|string|number|bool|null
	Found    []string // sorted object keys present there, capped at maxFoundKeys
	Err      error    // ErrNoMatch, ErrNotObject, or ErrNotArray
}

func (e *MissError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "uiselect: selector %q failed at segment %d (%q)", e.Selector, e.Index, e.Segment)
	if e.Element >= 0 {
		fmt.Fprintf(&b, " (element %d)", e.Element)
	}
	fmt.Fprintf(&b, ": %v, found kind %s", e.Err, e.Kind)
	if len(e.Found) > 0 {
		fmt.Fprintf(&b, ", keys present: %s", strings.Join(e.Found, ", "))
	}
	return b.String()
}

func (e *MissError) Unwrap() error { return e.Err }

// Apply returns the part of value this selector names.
//
// The empty Selector returns value UNCHANGED and un-round-tripped — the same
// bytes, not a re-encoding, so a binding with no `select` does not have its
// object keys reordered under it.
//
// Semantics, each one a decision:
//
//   - Descending a key on a non-object is ErrNotObject; a key absent from an
//     object is ErrNoMatch. Neither is ever an empty result.
//   - A MATCHED path holding an empty array or object SUCCEEDS and returns
//     it. "Not there" and "there and empty" are different facts, and a table
//     with zero rows is ordinary; conflating them either way is the defect.
//   - A terminal null is a match and is returned as null: the key existed.
//     Descending INTO a null is ErrNotObject with Kind "null".
//   - "results[]" requires an array and returns one — that assertion is its
//     only difference from "results", and it makes a source that switches
//     from a list to an object fail loudly instead of handing an object to
//     ap:table.rows.
//   - Under iteration, EVERY element must satisfy the remaining segments. A
//     failing element is a MissError carrying its index, not a dropped row;
//     skipping it would produce a table short by an invisible amount.
//   - Several "[]" segments nest and do NOT flatten: "a[].b[]" yields an
//     array of arrays. Implicit flattening would be a second, invisible rule
//     about how many levels a value has.
//
// Numbers are decoded with json.Decoder.UseNumber so a large integer ID
// re-encodes as the digits it arrived as; without it every number becomes a
// float64 and 12345678901234567890 comes back as 1.2345678901234567e+19 — a
// silent corruption of exactly the field a table is most likely to select.
func (s Selector) Apply(value json.RawMessage) (json.RawMessage, error) {
	if s.Empty() {
		return value, nil
	}

	var v any
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("uiselect: %w: %v", ErrValueNotJSON, err)
	}

	out, err := apply(v, s.segments, s.raw, 0, -1)
	if err != nil {
		return nil, err
	}

	// Built entirely from values decoded above, so this can only fail on an
	// unrepresentable value — impossible here, since numbers are json.Number,
	// not float64.
	result, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("uiselect: re-encoding selected value: %w", err)
	}
	return result, nil
}

// apply walks one step of a parsed selector against a decoded JSON value. segs
// is always non-empty on entry: Apply short-circuits the empty selector and
// every recursive call checks len(rest) first. index and element exist only to
// populate MissError — the segment's position in the selector, and the
// enclosing iteration's element index (-1 outside one).
func apply(v any, segs []Segment, raw string, index int, element int) (any, error) {
	seg := segs[0]
	rest := segs[1:]

	obj, ok := v.(map[string]any)
	if !ok {
		return nil, missError(raw, index, seg, element, kindOf(v), nil, ErrNotObject)
	}
	child, present := obj[seg.Key]
	if !present {
		return nil, missError(raw, index, seg, element, "object", foundKeys(obj), ErrNoMatch)
	}

	if seg.Iterate {
		arr, ok := child.([]any)
		if !ok {
			return nil, missError(raw, index, seg, element, kindOf(child), nil, ErrNotArray)
		}
		if len(rest) == 0 {
			return arr, nil
		}
		// A sequential loop, not further recursion into the array itself —
		// depth stays bounded by len(segs), never by the array's length.
		out := make([]any, len(arr))
		for i, elem := range arr {
			res, err := apply(elem, rest, raw, index+1, i)
			if err != nil {
				return nil, err
			}
			out[i] = res
		}
		return out, nil
	}

	if len(rest) == 0 {
		return child, nil
	}
	return apply(child, rest, raw, index+1, element)
}

// missError builds a *MissError, wrapping sentinel so errors.Is classifies
// it without string matching.
func missError(raw string, index int, seg Segment, element int, kind string, found []string, sentinel error) error {
	return &MissError{
		Selector: raw,
		Index:    index,
		Segment:  seg.String(),
		Element:  element,
		Kind:     kind,
		Found:    found,
		Err:      sentinel,
	}
}

// kindOf names the JSON kind of a decoded value, for MissError.Kind.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "bool"
	default:
		// Unreachable for a value produced by json.Decoder.UseNumber, which
		// only ever yields the six kinds above.
		return fmt.Sprintf("%T", v)
	}
}

// foundKeys returns m's keys, sorted so a MissError's text is deterministic
// rather than an accident of Go's map iteration, and capped at maxFoundKeys.
func foundKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxFoundKeys {
		keys = keys[:maxFoundKeys]
	}
	return keys
}
