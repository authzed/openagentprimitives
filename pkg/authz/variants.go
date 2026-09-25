package authz

import (
	"fmt"
)

// ResolveVariant evaluates each variant's When against args in order.
// Returns the first matching variant's Permission, a "matched" flag,
// and any CEL compile/eval error. When no variant matches, returns
// the zero Permission and matched=false (caller falls back to the
// per-tool Permission).
func ResolveVariant(variants []PermissionVariant, args map[string]any) (Permission, bool, error) {
	for i, v := range variants {
		prg, err := CompileBool(v.When)
		if err != nil {
			return Permission{}, false, fmt.Errorf("permissionVariants[%d].when: %w", i, err)
		}
		ok, err := EvalBool(prg, args)
		if err != nil {
			return Permission{}, false, fmt.Errorf("permissionVariants[%d].when eval: %w", i, err)
		}
		if ok {
			return v.Check, true, nil
		}
	}
	return Permission{}, false, nil
}
