//go:build !integration && !e2e

package slotspec_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBoundEntitySpecIsConstructedInExactlyOnePlace is RULING P-4, enforced
// structurally rather than by review.
//
// A BoundEntitySpec used to be assembled by hand at three sites — the runner
// loop, the runner binary and authzd — each reading the same AgentClass slots
// and each responsible for remembering every field. `requires` is that slot's
// PRECONDITION set, and a site that forgets it hands pkg/authz a slot with no
// gate: the candidate binds, the permissioned call succeeds, and nothing
// anywhere goes red. The previous plan produced exactly that class of bug at
// three separate sites, which is why the shape is removed rather than
// documented.
//
// slotspec.FromSlots is now the only constructor. This test refuses a second
// one, so re-introducing the hazard is a compile-green change that fails here
// by name instead of shipping a silently ungated slot.
//
// It names internal/ explicitly: the repo's server binaries live under
// internal/cmd, so a scan rooted only at cmd/ would see the CLI alone and keep
// passing while guarding nothing on the two sites that matter most.
//
// Only COMPOSITE LITERALS are inspected. Declaring a variable, a parameter or a
// slice of BoundEntitySpec is how the type is consumed and is not a second
// construction site; `make([]authz.BoundEntitySpec, ...)` zeroes nothing into
// existence either. Test files are exempt: a test that pins one field's
// behaviour has to be able to write that field alone, and a test cannot ship an
// ungated slot to a cluster.
//
// # What this guard CANNOT catch, stated so nobody reads it as stronger than it is
//
// The walk matches on the NAME at the literal's type position, so any spelling
// that renames the type first evades it:
//
//   - an alias or defined type — `type spec = authz.BoundEntitySpec`, then
//     `spec{...}`. Closing this needs go/types (full type resolution across the
//     whole repo), not go/ast, which is a materially heavier test than the
//     hazard justifies today.
//   - construction by reflection, or by decoding into a zero value.
//
// A guard believed to be exhaustive is how the NEXT gap gets missed, and this
// one exists precisely so nobody has to remember the rule. So: it catches the
// shape a person actually writes by hand — a literal naming the type, directly
// or through a slice, array, pointer or map — and it does not catch a
// deliberate rename. If a future edit needs the stronger property, upgrade this
// to a go/types pass rather than trusting the list above to stay short.
func TestBoundEntitySpecIsConstructedInExactlyOnePlace(t *testing.T) {
	roots := []string{"../../../pkg", "../../../internal", "../../../cmd", "../../../test"}
	var offenders []string

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// This package IS the sanctioned constructor.
			if strings.Contains(filepath.ToSlash(path), "/pkg/authz/slotspec/") {
				return nil
			}
			file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return nil // unparseable (generated, build-tagged oddity); not this test's business
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !namesBoundEntitySpec(lit.Type) {
					return true
				}
				offenders = append(offenders, path)
				return true
			})
			return nil
		})
		require.NoError(t, err, "walking %s", root)
	}

	assert.Empty(t, offenders,
		"authz.BoundEntitySpec must be constructed only by slotspec.FromSlots; a hand-written literal is how a "+
			"slot's requires[] preconditions silently fail to reach the gate at one call site while working at the others")
}

// namesBoundEntitySpec reports whether a composite literal's type is
// BoundEntitySpec, qualified (authz.BoundEntitySpec) or not (inside pkg/authz
// itself), or a container whose element type is.
//
// The container arms are not decoration: an inner element literal may ELIDE its
// type entirely — `map[string]authz.BoundEntitySpec{"k": {…}}` and
// `[]authz.BoundEntitySpec{{…}}` both parse with a nil `Type` on the inner
// *ast.CompositeLit — so the outer type is the ONLY node naming what is being
// constructed. Without the *ast.MapType arm the map form was missed entirely,
// which is what a review of this guard found.
func namesBoundEntitySpec(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "BoundEntitySpec"
	case *ast.SelectorExpr:
		return t.Sel.Name == "BoundEntitySpec"
	case *ast.ArrayType:
		return namesBoundEntitySpec(t.Elt)
	case *ast.MapType:
		// Only the value: a map KEYED by BoundEntitySpec constructs none.
		return namesBoundEntitySpec(t.Value)
	case *ast.StarExpr:
		return namesBoundEntitySpec(t.X)
	default:
		return false
	}
}
