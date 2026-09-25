//go:build !integration && !e2e

package relwrites_test

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

// TestSlotBoundCheckerIsWiredWhereverTheRelWriterIs refuses a dispatch path
// that gets a relwrites.Writer but no SlotBoundChecker.
//
// There are TWO wiring sites for the agent's tools, not one:
// internal/cmd/runner/main.go and test/e2e/inprocess_runner_factory.go, each
// with its own Loop. Wiring only the runner is the exact miss this feature's
// history is made of — the harness would then run every scenario with a nil
// checker, so a bundle that should prove the gate refuses would instead prove
// only that Run's unwired fallback fires, and a bundle that should prove the
// gate ALLOWS would fail for a reason nobody would connect to wiring.
//
// The guard is structural because the alternative is remembering. It is also
// deliberately NOT build-tagged to e2e: test/e2e's own files carry
// `//go:build e2e`, so a test that only compiles under that tag cannot be the
// thing that catches a harness wiring gap in the unit suite where people
// actually look. go/parser reads the source regardless of build tags, which is
// what lets this unit test see the e2e harness at all.
//
// Granularity is the enclosing BLOCK, not the enclosing function: main.go
// wires the sandbox tools and the MCP tools from two loops inside one
// function, and a function-level check would let one site satisfy the guard
// for both. Both real sites are a `if st, ok := t.(*T); ok { … }` body whose
// statements are the setter calls, so block-level is exactly per-site.
//
// # What this guard cannot catch
//
//   - **A path that wires NEITHER setter is not a site at all, and one exists
//     in the tree today.** The walk keys on SetRelWriter, so a tool kind that
//     never receives a writer is invisible here, forever and silently.
//     `pkg/agent/tool/sidecartoolbox` is that path: Synthesize wraps every
//     *MCPTool it builds in *originTool (synthesize.go), which forwards Execute
//     and exposes no setters, so the runner's `t.(*mcpdispatch.MCPTool)`
//     assertion never matches and no relWriter is ever set. Every
//     writesRelationships block on a SidecarToolbox is therefore inert —
//     Exclusive included — which is fail-closed and pre-dates this gate, but it
//     means this guard says nothing about that path. What the guard DOES buy
//     there: whoever fixes it and wires a writer will trip this test unless
//     they wire a checker too.
//   - A checker wired from a nil source. NewSlotBoundChecker(nil, …) returns
//     a nil checker by construction, and the call is still present here. That
//     case is covered where it belongs: Run refuses the block loudly, and
//     TestNewSlotBoundChecker_NilListerYieldsANilChecker pins it.
//   - A setter reached through an interface or a helper under another name.
//     The walk matches the SELECTOR, so a rename evades it — the same
//     limitation pkg/authz/slotspec/guard_test.go states for its own walk.
func TestSlotBoundCheckerIsWiredWhereverTheRelWriterIs(t *testing.T) {
	const (
		writerSetter  = "SetRelWriter"
		checkerSetter = "SetSlotBoundChecker"
	)
	roots := []string{"../../../pkg", "../../../internal", "../../../cmd", "../../../test"}

	var offenders []string
	var sites int

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
			ast.Inspect(file, func(n ast.Node) bool {
				block, ok := n.(*ast.BlockStmt)
				if !ok {
					return true
				}
				called := directCalls(block)
				pos, wires := called[writerSetter]
				if !wires {
					return true
				}
				sites++
				if _, checks := called[checkerSetter]; !checks {
					offenders = append(offenders, fset.Position(pos).String())
				}
				return true
			})
			return nil
		})
		require.NoError(t, err, "walking %s", root)
	}

	assert.GreaterOrEqual(t, sites, 4,
		"expected at least the runner's two wiring sites and the e2e harness's two; "+
			"finding fewer means the walk stopped seeing them, not that they are correct")
	assert.Empty(t, offenders,
		"every site that wires a relwrites.Writer must also wire a SlotBoundChecker: without one, "+
			"a block declaring requireSlotBound is refused as unwired rather than gated")
}

// directCalls maps each method name called as a statement DIRECTLY in block
// (not in a nested block) to the position of its first call.
//
// Direct-only on purpose: ast.Inspect visits every nested BlockStmt in turn,
// so a nested body is checked as its own site. Descending into children here
// would let an enclosing block borrow a checker call that only runs under some
// inner condition.
func directCalls(block *ast.BlockStmt) map[string]token.Pos {
	out := map[string]token.Pos{}
	for _, stmt := range block.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if _, seen := out[sel.Sel.Name]; !seen {
			out[sel.Sel.Name] = sel.Sel.NamePos
		}
	}
	return out
}
