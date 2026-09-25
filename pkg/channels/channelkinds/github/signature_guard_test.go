package github_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file guards the two properties of Verify that NO behavioral test can
// reach, because breaking either leaves the whole suite green:
//
//   - Swapping hmac.Equal for bytes.Equal (or `==`) changes only TIMING. The
//     rejections and acceptances stay byte-identical, so every signature test
//     still passes while the response latency becomes a byte-at-a-time oracle
//     for forging the MAC.
//   - Reading the legacy X-Hub-Signature header is caught today by
//     TestVerify_TheLegacySHA1HeaderIsNeverHonored — but only for the exact
//     fallback shape that test drives. A read added for logging, for a
//     metric, or "temporarily" during a migration would not trip it, and is
//     one edit away from becoming a fallback.
//
// Neither is caught by tooling either: this repo has no golangci config, so
// gosec is not running over it. That leaves review, and review does not scale
// across a year of edits. Hence a structural guard.
//
// It is an AST scan over THIS PACKAGE ONLY, deliberately not a repo-wide
// string sweep — a grep for "bytes.Equal" across pkg/ would fire on every
// legitimate byte comparison in the tree and be turned off within a month.
// The scope is the package that owns the authentication boundary.

// guardScanDir is the directory scanned, relative to this package's own
// directory, which is where `go test` runs.
const guardScanDir = "."

// verifyFuncName is the method whose body property A is asserted over.
const verifyFuncName = "Verify"

// nonConstantTimeComparisons are comparison helpers that leak timing. None has
// a legitimate use in this package; if one ever does, it belongs behind a
// named helper in the package that owns that concern, not spelled inline here
// where the next reader cannot tell it apart from a MAC comparison.
//
// subtle.ConstantTimeCompare is absent on purpose: it WOULD be correct, but
// this guard deliberately pins ONE spelling of the comparison so there is
// exactly one thing to review. Adding a second accepted form means a reviewer
// must now check which one a given call site used.
var nonConstantTimeComparisons = map[string]bool{
	"bytes.Equal":       true,
	"bytes.Compare":     true,
	"strings.EqualFold": true,
	"strings.Compare":   true,
	"reflect.DeepEqual": true,
}

// digestIdents are the identifiers inside Verify that hold, or produce, MAC
// material. A `==`/`!=` comparison touching any of them is a timing leak.
//
// THIS SCAN IS DEFENSE IN DEPTH, NOT THE PRIMARY CONTROL. It is a list of
// NAMES, and names drift: a refactor that renames `sum` to something outside
// this list weakens A3 silently, with nothing to say so. The load-bearing
// half of this guard is A2 — "Verify calls hmac.Equal" — which is
// name-independent and trips on any swap or deletion whatever replaced it.
// A3 exists to make the failure message specific about WHICH comparison went
// wrong, and to catch a stray comparison that sits alongside a still-present
// hmac.Equal.
//
// So: if this guard ever needs simplifying, delete A3, never A2. A guard
// whose weaker half looks like its strong half is how the strong half gets
// deleted by someone tidying up.
var digestIdents = map[string]bool{
	"sum":      true,
	"mac":      true,
	"offered":  true,
	"expected": true,
	"digest":   true,
	"secret":   true,
}

// legacySignatureHeader must never appear as a string literal. Matched
// EXACTLY, never by prefix: the legitimate "X-Hub-Signature-256" contains this
// value as a prefix, so a contains-check would fire on the correct constant
// and force an exclusion that would then hide the real thing.
const legacySignatureHeader = "X-Hub-Signature"

// TestVerifyUsesConstantTimeComparison is property A: inside Verify, the MAC
// is compared with hmac.Equal and with nothing else.
func TestVerifyUsesConstantTimeComparison(t *testing.T) {
	files := parsePackageSources(t)

	var (
		offenders    []string
		foundVerify  bool
		hmacEqualPos string
	)

	for _, pf := range files {
		// A1: no timing-leaking comparison helper anywhere in the package.
		ast.Inspect(pf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := types.ExprString(call.Fun)
			if nonConstantTimeComparisons[name] {
				offenders = append(offenders, fmt.Sprintf(
					"%s: %s is not constant time — the MAC comparison must use hmac.Equal",
					pf.fset.Position(call.Pos()), name))
			}
			return true
		})

		// A2 and A3 are scoped to Verify's body.
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil || fn.Name.Name != verifyFuncName || fn.Body == nil {
				continue
			}
			foundVerify = true

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					// A2: hmac.Equal must actually be called here.
					if types.ExprString(node.Fun) == "hmac.Equal" {
						hmacEqualPos = pf.fset.Position(node.Pos()).String()
					}
				case *ast.BinaryExpr:
					// A3: no ==/!= may touch MAC material.
					if node.Op != token.EQL && node.Op != token.NEQ {
						return true
					}
					for _, side := range []ast.Expr{node.X, node.Y} {
						if ident, ok := digestOperand(side); ok {
							offenders = append(offenders, fmt.Sprintf(
								"%s: %s compared with %s — %q is MAC material and must go through hmac.Equal",
								pf.fset.Position(node.Pos()), types.ExprString(side), node.Op, ident))
						}
					}
				}
				return true
			})
		}
	}

	// A guard that silently checks nothing reads as passing forever. Both of
	// these fail loudly if the package moves or the method is renamed.
	require.True(t, foundVerify,
		"no func named %q found under %s — this guard's assumptions are stale and it is checking nothing",
		verifyFuncName, guardScanDir)
	assert.NotEmpty(t, hmacEqualPos,
		"Verify does not call hmac.Equal.\n\n"+
			"The MAC comparison MUST be constant time. `==`, bytes.Equal and strings.EqualFold all return\n"+
			"the same answers as hmac.Equal, so every signature test in this package stays green while the\n"+
			"response latency becomes a byte-at-a-time oracle for forging the MAC. Nothing else catches\n"+
			"this: it is not behaviorally observable, and gosec is not running (no golangci config exists\n"+
			"in this repo).")

	assert.Empty(t, offenders, "non-constant-time comparison in the webhook authentication path:\n%s",
		strings.Join(offenders, "\n"))
}

// TestNoLegacySHA1SignatureHeaderIsRead is property B: the legacy SHA-1
// header is not read, not logged, and not importable-adjacent.
//
// GitHub sends X-Hub-Signature (HMAC-SHA1) on every delivery alongside the
// SHA-256 one. Honoring it — even as a fallback when the strong header is
// absent — lets an attacker downgrade this channel to a broken MAC simply by
// omitting a header. The safe posture is that this package has no reason to
// name SHA-1 at all.
func TestNoLegacySHA1SignatureHeaderIsRead(t *testing.T) {
	files := parsePackageSources(t)
	var offenders []string

	for _, pf := range files {
		for _, imp := range pf.file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if path == "crypto/sha1" {
				offenders = append(offenders, fmt.Sprintf(
					"%s: imports crypto/sha1 — this package has no reason to compute a SHA-1 MAC",
					pf.fset.Position(imp.Pos())))
			}
		}

		ast.Inspect(pf.file, func(n ast.Node) bool {
			// Import paths are string literals too and are handled above.
			if _, ok := n.(*ast.ImportSpec); ok {
				return false
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if val == legacySignatureHeader {
				offenders = append(offenders, fmt.Sprintf(
					"%s: names the legacy header %q", pf.fset.Position(lit.Pos()), val))
			}
			if strings.Contains(strings.ToLower(val), "sha1") {
				offenders = append(offenders, fmt.Sprintf(
					"%s: string literal %q names SHA-1", pf.fset.Position(lit.Pos()), val))
			}
			return true
		})
	}

	assert.Empty(t, offenders,
		"the legacy SHA-1 signature header must be ignored ENTIRELY, not read:\n%s\n\n"+
			"Only X-Hub-Signature-256 authenticates a delivery. A read added for logging or a metric is\n"+
			"one edit away from becoming a fallback, and a fallback lets an attacker downgrade this\n"+
			"channel to a broken MAC by omitting the strong header.",
		strings.Join(offenders, "\n"))
}

// --- scanning plumbing ---------------------------------------------------

// parsedFile pairs an AST with the FileSet that can position it.
type parsedFile struct {
	fset *token.FileSet
	file *ast.File
}

// parsePackageSources parses every non-test .go file in this package. It
// fails the test if it parsed none — a guard whose scan root went stale
// silently checks nothing, which is worse than no guard because it reads as
// passing forever (the lesson pkg/agent/runner's MIME guard records).
func parsePackageSources(t *testing.T) []parsedFile {
	t.Helper()

	var out []parsedFile
	fset := token.NewFileSet()

	err := filepath.WalkDir(guardScanDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		out = append(out, parsedFile{fset: fset, file: f})
		return nil
	})
	require.NoError(t, err, "walking %s for Go sources", guardScanDir)
	require.NotEmpty(t, out,
		"parsed no non-test Go files under %s — this guard's scan root is stale and it is checking nothing",
		guardScanDir)
	return out
}

// digestOperand reports whether one side of a comparison touches MAC
// material, and names the identifier that made it so.
//
// A `len(x)` call is exempt: `len(secret) == 0` is a length check, not a
// content comparison, and reveals nothing about the bytes. Every other shape
// — the bare identifier, a field of it, a slice of it, a conversion of it, or
// a `.Sum(...)` call — counts.
func digestOperand(e ast.Expr) (string, bool) {
	if call, ok := e.(*ast.CallExpr); ok {
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "len" {
			return "", false
		}
	}
	var hit string
	ast.Inspect(e, func(n ast.Node) bool {
		if hit != "" {
			return false
		}
		switch node := n.(type) {
		case *ast.SelectorExpr:
			// mac.Sum(nil) and any other hash finalization.
			if node.Sel != nil && node.Sel.Name == "Sum" {
				hit = "Sum"
				return false
			}
		case *ast.Ident:
			if digestIdents[node.Name] {
				hit = node.Name
				return false
			}
		}
		return true
	})
	return hit, hit != ""
}
