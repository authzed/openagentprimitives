// Package celconv converts cel-go evaluation results into plain Go values
// without asserting on cel-go's internal representations.
//
// It exists because the same assertion bug has now been written twice, in two
// packages that evaluate CEL for unrelated reasons (pkg/authz/relwrites and
// pkg/agent/tool/mcp/labelextract). Keeping the conversion — and the reason it
// is not a type assertion — in one importable place is what stops a third copy
// from re-learning it.
//
// It lives under pkg/x rather than in either caller because the conversion is
// pure cel-go plumbing that belongs to neither authorization nor MCP label
// extraction, and because labelextract is otherwise free of internal
// dependencies: importing relwrites for it would pull pkg/memory, pkg/authz and
// pkg/platform/identity into a leaf package for the sake of twelve lines.
package celconv

import (
	"fmt"
	"reflect"

	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// List converts a CEL list value to a Go slice, accepting every representation
// cel-go produces.
//
// A raw v.Value().([]any) assertion is NOT sufficient: a plain field reference
// (result.results) yields a Go []any, but a list MACRO — filter, map — builds a
// new list whose Value() is []ref.Val. Asserting only the first meant a forEach
// silently supported nothing but a bare field reference, and the documented way
// to skip records missing a property ("filter") aborted every call with
// "expected list, got []ref.Val".
//
// ConvertToNative is cel-go's own conversion and handles both, plus any future
// internal representation, so this cannot drift again.
//
// Note that ConvertToNative does NOT recurse: the returned slice's ELEMENTS are
// still in cel-go's internal form when they are themselves lists or maps, and
// they marshal to JSON as {"Adapter":{}} — every key and value silently gone,
// with no error to notice. A caller that needs plain values all the way down
// must recurse itself, re-wrapping each level with the type adapter
// (types.DefaultTypeAdapter.NativeToValue) so every step dispatches on cel-go's
// traits rather than on a concrete internal type, never on []ref.Val directly.
func List(v ref.Val) ([]any, error) {
	if lister, ok := v.(traits.Lister); ok {
		native, err := lister.ConvertToNative(reflect.TypeOf([]any{}))
		if err != nil {
			return nil, fmt.Errorf("converting list: %w", err)
		}
		out, ok := native.([]any)
		if !ok {
			return nil, fmt.Errorf("converting list: got %T", native)
		}
		return out, nil
	}
	// Not a list at all — e.g. a forEach pointed at a scalar.
	return nil, fmt.Errorf("expected list, got %T", v.Value())
}
