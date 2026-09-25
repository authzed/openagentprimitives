//go:build !integration && !e2e

package credkind_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credentialTypeLiterals are the enum values. A comparison against one of
// these, outside the credkind tree, is the branching this package exists to
// remove.
var credentialTypeLiterals = map[string]bool{
	`"static"`: true, `"oauth"`: true, `"federated"`: true, `"githubApp"`: true,
}

// falsePositiveSuffixes names specific files (matched by path suffix, not
// directory) that compare a DIFFERENT field — MCPServer.Spec.Auth.Type —
// against these same literal value names. That field is populated and
// consumed entirely outside credkind (see
// pkg/platform/identity/passthroughcatalog, whose AuthTypeOAuth/AuthTypeStatic
// constants are this same enum spelled without the literal — those sites
// don't trip this test at all). Excluding by file rather than by directory
// keeps the rest of pkg/platform/identityd under the guard: handlers_portal.go
// in the same package legitimately dispatches a REAL credential type through
// credkindregistry.Get and must stay covered.
var falsePositiveSuffixes = []string{
	"pkg/platform/identityd/handlers_link.go",
	"pkg/platform/identityd/suggested.go",
}

func isFalsePositiveFile(path string) bool {
	slash := filepath.ToSlash(path)
	for _, suf := range falsePositiveSuffixes {
		if strings.HasSuffix(slash, suf) {
			return true
		}
	}
	return false
}

// TestNoCredentialTypeSwitchesOutsideCredkind walks pkg/, internal/ and cmd/
// and fails on any comparison of a credential-type field against a
// credential-type literal — either a `==`/`!=` binary comparison, or a `case`
// clause of a switch whose tag expression selects a `.Type` field.
//
// It names internal/ explicitly: the repo's binaries moved under internal/cmd,
// so a scan rooted only at cmd/ now sees the CLI alone and would keep passing
// while guarding nothing on the server side.
//
// Two construction sites are exempt by design, not by exclusion list: a
// composite-literal `Type: "static"` (a *ast.KeyValueExpr, never inspected
// here) is how a credkind.Kind's own BuildCredential legitimately assembles
// the CRD value it hands back, and this test only ever looks at comparisons.
func TestNoCredentialTypeSwitchesOutsideCredkind(t *testing.T) {
	roots := []string{"../../../../pkg", "../../../../internal", "../../../../cmd"}
	var offenders []string

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			// The credkind tree is where these literals legitimately live.
			if strings.Contains(filepath.ToSlash(path), "/platform/identity/credkind/") {
				return nil
			}
			// MCPServer.Spec.Auth.Type is a DIFFERENT enum that shares value
			// names. It is out of scope for credkind and has its own TODO.
			if strings.Contains(filepath.ToSlash(path), "/identity/passthrough/") {
				return nil
			}
			// See falsePositiveSuffixes: two files here ALSO read
			// MCPServer.Spec.Auth.Type and compare it against "oauth", the same
			// different-enum false positive as passthrough/ above.
			if isFalsePositiveFile(path) {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}

			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil // unparseable files are not this test's problem
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.BinaryExpr:
					if node.Op != token.EQL && node.Op != token.NEQ {
						return true
					}
					for _, side := range []ast.Expr{node.X, node.Y} {
						lit, ok := side.(*ast.BasicLit)
						if ok && credentialTypeLiterals[lit.Value] {
							offenders = append(offenders,
								fmt.Sprintf("%s: comparison against %s", fset.Position(node.Pos()), lit.Value))
						}
					}
				case *ast.SwitchStmt:
					// Only a switch whose tag selects a `.Type` field is in scope —
					// a switch on some unrelated string happens to share these
					// values and is not this test's business.
					sel, ok := node.Tag.(*ast.SelectorExpr)
					if !ok || sel.Sel == nil || sel.Sel.Name != "Type" {
						return true
					}
					for _, stmt := range node.Body.List {
						cc, ok := stmt.(*ast.CaseClause)
						if !ok {
							continue
						}
						for _, v := range cc.List {
							lit, ok := v.(*ast.BasicLit)
							if ok && credentialTypeLiterals[lit.Value] {
								offenders = append(offenders,
									fmt.Sprintf("%s: case %s on a .Type switch", fset.Position(lit.Pos()), lit.Value))
							}
						}
					}
				}
				return true
			})
			return nil
		})
		require.NoError(t, err, "walking %s", root)
	}

	assert.Empty(t, offenders,
		"credential types must be dispatched through pkg/platform/identity/credkind/registry, "+
			"not compared against literals:\n%s", strings.Join(offenders, "\n"))
}

// credentialDispatchFields are the discriminated-union arms of the
// credential-type enum, spelled as struct field selectors rather than string
// literals:
//
//	switch {
//	case cred.Static != nil:    …
//	case cred.OAuth != nil:     …
//	case cred.Federated != nil: …
//	}
//
// That is a type switch on the SAME enum TestNoCredentialTypeSwitchesOutsideCredkind
// polices, spelled with no string literal — invisible to that test's
// literal-matching AST walk. This is Guard 6.
var credentialDispatchFields = map[string]bool{
	"Static": true, "OAuth": true, "Federated": true, "GitHubApp": true,
}

// guard6ExcludedSuffixes names files that legitimately test two or more
// dispatch fields against nil outside credkind/, matched by exact path
// suffix (never by directory, which would blind the guard to the rest of
// the file's package).
// Empty on purpose. The single entry it used to carry —
// identityrefresh/identity.go's ReferencesSecret, excused as "oauth-only BY
// DESIGN" — was the exclusion covering up the exact defect the guard exists to
// catch: it asserted that "refreshable" and "type=oauth" are the same thing,
// which holds only while oauth is the sole NeedsRefresh kind. It now dispatches
// through the registry and needs no exemption.
//
// Add an entry only for a site that genuinely must name two blocks and cannot
// route through the registry, and say why in a way a second kind would not
// invalidate.
var guard6ExcludedSuffixes = []string{}

func isGuard6ExcludedFile(path string) bool {
	slash := filepath.ToSlash(path)
	for _, suf := range guard6ExcludedSuffixes {
		if strings.HasSuffix(slash, suf) {
			return true
		}
	}
	return false
}

// isGeneratedSource reports the standard `// Code generated … DO NOT EDIT.`
// marker controller-gen stamps on zz_generated.deepcopy.go. DeepCopyInto
// legitimately nil-checks every one of a credential's typed blocks in
// sequence to decide whether to deep-copy each one — not the discriminated-
// union DISPATCH this guard exists to catch — and the file is regenerated by
// `mage gen:api`, never hand-edited, so nothing there is a switch anyone
// chose to write.
func isGeneratedSource(src []byte) bool {
	return strings.Contains(string(src), "Code generated") && strings.Contains(string(src), "DO NOT EDIT")
}

// credFieldHit is one `X.Field != nil` (or `== nil`) leaf comparison, where
// Field is one of credentialDispatchFields.
type credFieldHit struct {
	base  string // syntactic rendering of X, for grouping same-variable hits
	field string
	pos   token.Pos
}

// unwrapParen strips redundant parens so `(cred.Static != nil)` matches the
// same as the bare comparison.
func unwrapParen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// nilFieldHit reports whether expr is `X.Field != nil` / `X.Field == nil`
// (either operand order) for one of the four dispatch fields.
func nilFieldHit(expr ast.Expr) (credFieldHit, bool) {
	bin, ok := unwrapParen(expr).(*ast.BinaryExpr)
	if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
		return credFieldHit{}, false
	}
	var sel *ast.SelectorExpr
	sawNil := false
	for _, side := range []ast.Expr{bin.X, bin.Y} {
		side = unwrapParen(side)
		if id, ok := side.(*ast.Ident); ok && id.Name == "nil" {
			sawNil = true
			continue
		}
		if se, ok := side.(*ast.SelectorExpr); ok {
			sel = se
		}
	}
	if !sawNil || sel == nil || sel.Sel == nil || !credentialDispatchFields[sel.Sel.Name] {
		return credFieldHit{}, false
	}
	return credFieldHit{base: types.ExprString(sel.X), field: sel.Sel.Name, pos: bin.Pos()}, true
}

// collectFromExpr recurses through &&/|| to find every leaf nil-field
// comparison in a boolean expression tree (an if's Cond, or one switch case
// value).
func collectFromExpr(expr ast.Expr) []credFieldHit {
	expr = unwrapParen(expr)
	if h, ok := nilFieldHit(expr); ok {
		return []credFieldHit{h}
	}
	if bin, ok := expr.(*ast.BinaryExpr); ok && (bin.Op == token.LAND || bin.Op == token.LOR) {
		return append(collectFromExpr(bin.X), collectFromExpr(bin.Y)...)
	}
	return nil
}

// collectFromIf walks an if / else-if chain — one *ast.IfStmt whose Else may
// itself be another *ast.IfStmt — collecting every leaf nil-field comparison
// from every link's condition. It does not descend into the bodies; those
// are ordinary *ast.BlockStmt nodes the outer walk visits on their own.
func collectFromIf(stmt *ast.IfStmt) []credFieldHit {
	var out []credFieldHit
	for stmt != nil {
		out = append(out, collectFromExpr(stmt.Cond)...)
		next, ok := stmt.Else.(*ast.IfStmt)
		if !ok {
			break
		}
		stmt = next
	}
	return out
}

// collectFromSwitch walks a switch's case VALUES. A tagless switch (`switch {
// case cred.Static != nil: }`) is the shape this guard targets; a tagged
// switch's case values compare against the tag instead, a different shape
// (TestNoCredentialTypeSwitchesOutsideCredkind's territory) that the
// nil-comparison detector above simply never matches, so no special-casing
// is needed here.
func collectFromSwitch(sw *ast.SwitchStmt) []credFieldHit {
	var out []credFieldHit
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, v := range cc.List {
			out = append(out, collectFromExpr(v)...)
		}
	}
	return out
}

// fieldsInStmt returns the nil-field comparisons ONE statement directly
// contains — an if/else-if chain, or a switch's case values.
func fieldsInStmt(stmt ast.Stmt) []credFieldHit {
	switch s := stmt.(type) {
	case *ast.IfStmt:
		return collectFromIf(s)
	case *ast.SwitchStmt:
		return collectFromSwitch(s)
	}
	return nil
}

// scanStmtList walks one flat list of SIBLING statements — a block's body,
// or a switch case's body (CaseClause.Body is a bare []ast.Stmt, never
// itself a *ast.BlockStmt, so the generic BlockStmt walk below never treats
// it as one scope on its own) — and reports the point where a SECOND
// distinct dispatch field of the same base expression is seen.
//
// This catches all three shapes the discriminated-union bug takes: one
// switch whose cases test different fields, one if whose condition
// OR-combines two field tests, and a RUN OF SIBLING ifs each testing one
// field — the shape collectCredentialSecretNames had before this guard
// (agentsession/controller.go: one `if cred.Static != nil …`, then a
// separate `if cred.OAuth != nil …`, neither if alone naming two fields).
func scanStmtList(stmts []ast.Stmt, report func(pos token.Pos, base, newField, existingField string)) {
	seen := map[string]map[string]token.Pos{}
	for _, stmt := range stmts {
		for _, h := range fieldsInStmt(stmt) {
			if seen[h.base] == nil {
				seen[h.base] = map[string]token.Pos{}
			}
			if _, ok := seen[h.base][h.field]; ok {
				continue
			}
			if len(seen[h.base]) >= 1 {
				var existing string
				for f := range seen[h.base] {
					existing = f
					break
				}
				report(h.pos, h.base, h.field, existing)
			}
			seen[h.base][h.field] = h.pos
		}
	}
}

// TestNoMultiFieldCredentialDispatchOutsideCredkind is Guard 6: it walks
// pkg/, internal/ and cmd/ and fails on any if/switch that tests two or more
// of .Static/.OAuth/.Federated/.GitHubApp against nil on the same base
// expression — the discriminated-union dispatch that
// TestNoCredentialTypeSwitchesOutsideCredkind cannot see (see that test's
// doc comment). Route through credkindregistry.SecretNameFor, or
// registry.Get + Kind.SecretRef when the caller also needs the Key, instead.
func TestNoMultiFieldCredentialDispatchOutsideCredkind(t *testing.T) {
	roots := []string{"../../../../pkg", "../../../../internal", "../../../../cmd"}
	var offenders []string

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.Contains(filepath.ToSlash(path), "/platform/identity/credkind/") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if isGuard6ExcludedFile(path) {
				return nil
			}

			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil // unreadable files are not this test's problem
			}
			if isGeneratedSource(src) {
				return nil
			}

			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, src, 0)
			if perr != nil {
				return nil // unparseable files are not this test's problem
			}

			report := func(pos token.Pos, base, newField, existingField string) {
				offenders = append(offenders, fmt.Sprintf(
					"%s: dispatches on both .%s and .%s of %q — route through credkindregistry.SecretNameFor "+
						"(or registry.Get + Kind.SecretRef) instead",
					fset.Position(pos), existingField, newField, base))
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.BlockStmt:
					scanStmtList(node.List, report)
				case *ast.CaseClause:
					scanStmtList(node.Body, report)
				}
				return true
			})
			return nil
		})
		require.NoError(t, err, "walking %s", root)
	}

	assert.Empty(t, offenders,
		"credential-type dispatch on two or more of .Static/.OAuth/.Federated/.GitHubApp must route through "+
			"pkg/platform/identity/credkind/registry, not a hand-rolled nil-check switch — the same enum "+
			"dispatch TestNoCredentialTypeSwitchesOutsideCredkind catches via string literals, spelled "+
			"without one:\n%s", strings.Join(offenders, "\n"))
}
