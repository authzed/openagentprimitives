//go:build !integration && !e2e

package authz_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSlotWritesRouteThroughTheGate refuses any production .go file that
// hand-builds a slot_grant_/slot_pin tuple, except the three sanctioned
// writers. Every production slot write must route through GrantSlots (the
// single-occupancy gate) or the pinner — otherwise a caller that assembled its
// own slot tuple and wrote it would bypass the pin entirely, binding a second
// instance to a slot the gate would have refused, with nothing going red.
//
// It flags two shapes, the two ways the gate gets bypassed by hand:
//
//   - a STRING LITERAL beginning with "slot_grant_" or "slot_pin" — a tuple
//     relation name written out rather than taken from the builder; and
//   - a call to the tuple builders authz.SlotGrantRelation / SlotPinRelation
//     (exact name, so DataSlotGrantRelation, isSlotGrantRelation and the
//     SlotGrantRelationName/Prefix read-side helpers are NOT flagged), which a
//     caller could feed straight to a WriteRelationships bypassing the gate.
//
// Sanctioned sites, and why each is the gate rather than a bypass of it:
//
//   - pkg/authz/slot_grant.go  — defines SlotGrantRelation and is where
//     GrantSlots/RevokeSlots build the grant tuples.
//   - pkg/authz/slot_pin.go    — defines SlotPinRelation and the pin relation
//     name.
//   - pkg/authz/spicedb/slot_pin.go — the SlotPinner implementation itself.
//     The brief's own contract says writes route through "GrantSlots/the
//     pinner"; this file IS the pinner (EnsurePin/MovePin build and write the
//     pin tuple), so it is a sanctioned writer, not a bypass of one.
//
// Walked over pkg/, internal/ AND cmd/ deliberately: the server binaries live
// under internal/cmd and the user-facing CLI under cmd/oap, so a pkg/-only scan
// would guard nothing on the sides that actually reach SpiceDB in production. A
// tree-walking guard rooted short of cmd/ keeps passing while silently guarding
// nothing there (see the ship-gate note in CLAUDE.md). test/ is out of scope
// (its SlotGrantRelationName use only derives schema-wait pairs, and a test
// cannot ship a bypass to a cluster).
//
// # What this guard cannot catch
//
// It matches on the literal and on the NAME at the call site, so a rename of
// the builder, an alias type, or a relation string assembled from fragments
// evades it — the same limitation pkg/authz/slotspec/guard_test.go states for
// its own walk. It catches the shape a person writes by hand, which is the one
// that has actually happened.
func TestSlotWritesRouteThroughTheGate(t *testing.T) {
	roots := []string{"../../pkg", "../../internal", "../../cmd"}
	sanctioned := []string{
		"/pkg/authz/slot_grant.go",
		"/pkg/authz/slot_pin.go",
		"/pkg/authz/spicedb/slot_pin.go",
	}
	isSanctioned := func(path string) bool {
		slash := filepath.ToSlash(path)
		for _, s := range sanctioned {
			if strings.HasSuffix(slash, s) {
				return true
			}
		}
		return false
	}

	var offenders []string
	sanctionedHits := map[string]bool{}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil // unparseable (generated, build-tagged oddity); not this test's business
			}
			var hit token.Pos = token.NoPos
			ast.Inspect(file, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						if v, uerr := strconv.Unquote(x.Value); uerr == nil && buildsSlotRelationString(v) {
							hit = x.Pos()
						}
					}
				case *ast.CallExpr:
					if callsSlotTupleBuilder(x.Fun) {
						hit = x.Pos()
					}
				}
				return hit == token.NoPos
			})
			if hit == token.NoPos {
				return nil
			}
			if isSanctioned(path) {
				sanctionedHits[filepath.ToSlash(path)] = true
				return nil
			}
			offenders = append(offenders, fset.Position(hit).String())
			return nil
		})
		require.NoError(t, err, "walking %s", root)
	}

	assert.Empty(t, offenders,
		"a slot_grant_/slot_pin tuple built outside GrantSlots or the pinner bypasses the single-occupancy "+
			"gate: route the write through authz.GrantSlots (or the SlotPinner) instead of assembling the tuple by hand")

	// A walk that stopped seeing the sanctioned writers would pass vacuously —
	// offenders would be empty because nothing matched at all. Require the
	// detector to have fired on the two builder-definition files it must always
	// find, so "no offenders" means "the gate holds", not "the scan broke".
	assert.True(t, sanctionedHitEndsWith(sanctionedHits, "/pkg/authz/slot_grant.go"),
		"the detector must fire on slot_grant.go; if it does not, the walk is broken and proves nothing")
	assert.True(t, sanctionedHitEndsWith(sanctionedHits, "/pkg/authz/slot_pin.go"),
		"the detector must fire on slot_pin.go; if it does not, the walk is broken and proves nothing")
}

// buildsSlotRelationString reports whether a string literal's value is a slot
// relation name written by hand.
func buildsSlotRelationString(v string) bool {
	return strings.HasPrefix(v, "slot_grant_") || strings.HasPrefix(v, "slot_pin")
}

// callsSlotTupleBuilder reports whether a call target is the SlotGrantRelation
// or SlotPinRelation tuple builder, by EXACT name (qualified or not) so the
// read-side helpers whose names merely share a prefix are left alone.
func callsSlotTupleBuilder(fun ast.Expr) bool {
	var name string
	switch f := fun.(type) {
	case *ast.Ident:
		name = f.Name
	case *ast.SelectorExpr:
		name = f.Sel.Name
	default:
		return false
	}
	return name == "SlotGrantRelation" || name == "SlotPinRelation"
}

func sanctionedHitEndsWith(hits map[string]bool, suffix string) bool {
	for p := range hits {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}
