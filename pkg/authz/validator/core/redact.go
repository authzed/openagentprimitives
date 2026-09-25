package core

import (
	"fmt"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

// This file holds the walk that makes Decision.Parsed's "redaction-safe view"
// promise true. It is SECURITY-SENSITIVE: it is the last thing that runs
// before a Decision leaves a validator, and nothing downstream can undo a
// value it lets through — by then the raw value is only present in the field
// that leaked it.

// RedactAnyMap recursively scrubs every leaf string in a map[string]any,
// descending through nested maps, slices, and every other shape RedactAny can
// walk. The map is scrubbed in place, so the caller keeps its map.
//
// Values only — map KEYS are not rewritten. Every map reaching this walk is
// keyed by a name declared in a toolkit or a spec (a flag's long name, a
// positional slot, an MCP argument field), never by a value, and rewriting
// keys would additionally let two distinct keys mask onto one and silently
// drop an entry. A sensitive value used as a map KEY is therefore outside this
// walk's contract; the Parsed docs on each validator say so.
func RedactAnyMap(m map[string]any, r *redact.Redactor) {
	for k, v := range m {
		m[k] = RedactAny(v, r)
	}
}

// RedactAny returns v with every registered sensitive value replaced by its
// redaction token, wherever inside v that value appears.
//
// FAIL-SAFE BY CONSTRUCTION: no branch returns a value unexamined. The rule
// every case obeys is
//
//	either the value comes back with its type intact and every string inside
//	it masked, or it comes back as its masked RENDERING.
//
// That rule closed a real leak, and a type switch would reopen it: a default
// arm returning "everything else" untouched is fail-OPEN, and it let a []string
// — exactly what a `stringList` flag and a variadic positional bind to — carry
// raw values straight out of a walk that had supposedly covered the field.
// Adding one case per leaked shape just reproduces the defect for the next one.
//
// So the general case dispatches on reflect.Kind, not on type: []string,
// []MyString, map[string]int, a struct and a pointer to one are all walked by
// SHAPE, and only genuinely uninspectable kinds (chan, func, complex) reach a
// default — which masks their rendering rather than emitting them.
//
// Types are preserved wherever the scrubbed value still fits its slot, because
// consumers read Parsed structurally (`oap tools toolspec explain` prints it,
// the MCP dispatch path marshals it into an audit record, the option-terminator
// regression test asserts a []string positional). The one case where a type
// cannot be kept is a value that IS a secret in a slot that can only hold
// non-strings — an int-valued map holding a registered number. There, the
// container comes back as its masked rendering: reshaping a field is a visible
// cost, handing back the secret is not.
//
// The walk assumes an ACYCLIC value. Every parsed view it sees is built from an
// argv or from decoded JSON, neither of which can express a cycle; a
// self-referential value would recurse until the stack gave out.
func RedactAny(v any, r *redact.Redactor) any {
	// Fast paths for the shapes that dominate both validators: CLI flags and
	// positionals, and JSON-decoded MCP arguments. Each is what the
	// shape-driven walk below would do for it, without the reflection.
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return r.RedactInString(x)
	case map[string]any:
		RedactAnyMap(x, r)
		return x
	case []any:
		for i := range x {
			x[i] = RedactAny(x[i], r)
		}
		return x
	case []string:
		for i := range x {
			x[i] = r.RedactInString(x[i])
		}
		return x
	}
	return redactByShape(reflect.ValueOf(v), r)
}

// redactByShape is RedactAny's general case: the kind-driven walk that runs
// for every type the fast path does not name. It is what makes the walk
// fail-safe — a type nobody anticipated is still walked by its shape.
func redactByShape(rv reflect.Value, r *redact.Redactor) any {
	if !rv.IsValid() {
		return nil
	}
	switch rv.Kind() {
	case reflect.String:
		// A named string type (`type Path string`) keeps its type: a
		// structured consumer decodes it back into the same field.
		out := reflect.New(rv.Type()).Elem()
		out.SetString(r.RedactInString(rv.String()))
		return out.Interface()

	case reflect.Pointer:
		if rv.IsNil() {
			return rv.Interface()
		}
		elemT := rv.Type().Elem()
		scrubbed := RedactAny(rv.Elem().Interface(), r)
		if !assignableTo(scrubbed, elemT) {
			// The pointee could only be scrubbed by rendering it. Return that
			// rendering rather than a pointer to the unscrubbed value.
			return scrubbed
		}
		// Written back through the pointer so the caller's pointer identity
		// survives — core.Finalize relies on it for Decision.Parsed.
		rv.Elem().Set(valueOf(scrubbed, elemT))
		return rv.Interface()

	case reflect.Slice, reflect.Array:
		return redactSequence(rv, r)

	case reflect.Map:
		return redactMap(rv, r)

	case reflect.Struct:
		return redactStruct(rv, r)

	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		// A scalar has no string to descend into, but its RENDERING can be a
		// registered secret: pkg/tools/redact records non-string JSON leaves
		// by their stringified form, so an MCP numeric argument can be a
		// registered value. Keep the typed value when masking changes nothing;
		// when it does, the value IS the secret and must not survive.
		s := fmt.Sprint(rv.Interface())
		if masked := r.RedactInString(s); masked != s {
			return masked
		}
		return rv.Interface()

	default:
		// Complex, Chan, Func, UnsafePointer — nothing to walk, and no
		// structured form worth keeping. Emit the masked rendering, never the
		// value: this is the branch that used to be `return v`.
		return r.RedactInString(fmt.Sprint(rv.Interface()))
	}
}

// redactSequence walks a slice or array elementwise. Slices are scrubbed in
// place (their elements are addressable); an array is copied, since the value
// this walk holds is not.
func redactSequence(rv reflect.Value, r *redact.Redactor) any {
	elemT := rv.Type().Elem()

	// A byte slice (json.RawMessage, a captured stdout) is text far more often
	// than it is a list of numbers. Walking it element by element would cost a
	// full redactor scan per byte and would mask individual bytes whose
	// rendering collides with a short registered value, shredding the slice
	// into an unreadable rendering. Treat it as the string it is.
	if rv.Kind() == reflect.Slice && elemT.Kind() == reflect.Uint8 {
		out := reflect.New(rv.Type()).Elem()
		out.SetBytes([]byte(r.RedactInString(string(rv.Bytes()))))
		return out.Interface()
	}

	scrubbed := make([]any, rv.Len())
	for i := range scrubbed {
		scrubbed[i] = RedactAny(rv.Index(i).Interface(), r)
		if !assignableTo(scrubbed[i], elemT) {
			return r.RedactInString(fmt.Sprint(rv.Interface()))
		}
	}
	out := rv
	if rv.Kind() == reflect.Array {
		out = reflect.New(rv.Type()).Elem()
	}
	for i, v := range scrubbed {
		out.Index(i).Set(valueOf(v, elemT))
	}
	return out.Interface()
}

// redactMap walks a map's values, in place. Keys are left alone — see
// RedactAnyMap for why. The scrubbed values are collected before any is
// written back, so the map is not mutated while it is being ranged over.
func redactMap(rv reflect.Value, r *redact.Redactor) any {
	valT := rv.Type().Elem()
	keys := rv.MapKeys()
	scrubbed := make([]any, len(keys))
	for i, k := range keys {
		scrubbed[i] = RedactAny(rv.MapIndex(k).Interface(), r)
		if !assignableTo(scrubbed[i], valT) {
			return r.RedactInString(fmt.Sprint(rv.Interface()))
		}
	}
	for i, k := range keys {
		rv.SetMapIndex(k, valueOf(scrubbed[i], valT))
	}
	return rv.Interface()
}

// redactStruct walks a struct field by field, returning a scrubbed value of
// the same type.
//
// A struct with an unexported field is NOT walked: reflect cannot read it, so
// the walk cannot show it carries no secret, and cannot write a scrubbed value
// back into it either. fmt can read it, so such a value comes back as its
// masked rendering — every field included.
func redactStruct(rv reflect.Value, r *redact.Redactor) any {
	t := rv.Type()
	out := reflect.New(t).Elem()
	out.Set(rv)
	for i := 0; i < t.NumField(); i++ {
		if !t.Field(i).IsExported() {
			return r.RedactInString(fmt.Sprint(rv.Interface()))
		}
		f := out.Field(i)
		scrubbed := RedactAny(f.Interface(), r)
		if !assignableTo(scrubbed, f.Type()) {
			return r.RedactInString(fmt.Sprint(rv.Interface()))
		}
		f.Set(valueOf(scrubbed, f.Type()))
	}
	return out.Interface()
}

// assignableTo reports whether a scrubbed value can be written back into a
// slot of type t. It is what decides between "same shape, scrubbed" and "the
// masked rendering": a scalar that WAS a registered secret comes back as a
// string, which an int-typed slot cannot hold.
func assignableTo(v any, t reflect.Type) bool {
	if v == nil {
		switch t.Kind() {
		case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice,
			reflect.Chan, reflect.Func, reflect.UnsafePointer:
			return true
		}
		return false
	}
	return reflect.TypeOf(v).AssignableTo(t)
}

// valueOf converts a scrubbed value into a reflect.Value assignable to t.
// A nil needs the typed zero: reflect.ValueOf(nil) is invalid and cannot be
// assigned to anything.
func valueOf(v any, t reflect.Type) reflect.Value {
	if v == nil {
		return reflect.Zero(t)
	}
	return reflect.ValueOf(v)
}
