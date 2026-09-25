//go:build !integration && !e2e

// pkg/authz/spicedb/relsource/imports/bundle_completeness_test.go
//
// Guards imports.go's hand-maintained blank-import list against silently
// falling out of sync with the tree. imports.go's own doc comment says the
// list "is derived from the tree ... not transcribed from a design doc" —
// but nothing enforced that claim until this test: a human derives it once,
// by grep, and nothing re-checks it on every future change. A sixth claim
// owner added tomorrow without a matching blank import here would leave
// every binary linking this bundle with a PARTIAL claim table while the
// sentinel is latched complete (relsource.MarkComplete) — CheckWrite and
// CheckDeleteFilter would silently ALLOW a write or delete on the new
// owner's claims from any other writer, exactly the original Critical this
// branch's relsource guard closed, restored by omission. See AGENTS.md's
// "Auditing" section on registry-vs-branching: this is the tree-walking
// guard test that rule calls for, one level up from the registry itself.
package imports_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relsourceImportPath is the import path this test recognizes as "the
// relsource package" regardless of which local name (almost always
// "relsource", but a file could alias it) a scanned file binds it to.
const relsourceImportPath = "github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"

// modulePrefix converts a repo-relative directory into its Go import path.
const modulePrefix = "github.com/authzed/openagentprimitives/"

// packageClaims accumulates, across every non-test .go file in one
// directory (== one Go package, for every directory this repo actually
// uses), the two facts that together decide whether that package is a real
// relsource claim owner:
//
//   - nonEmptySourceVars: every package-level var whose value is a
//     `relsource.Source{...}` composite literal carrying a non-empty Claims
//     field, keyed by var name.
//   - registeredIdents: every identifier passed as the sole argument to
//     `relsource.Register(...)` inside a `func init()` in this package.
//   - registeredInline: true if any such Register call's argument is
//     ITSELF a Claims-bearing relsource.Source composite literal, with no
//     intermediate var (no in-tree package does this today, but the guard
//     should not silently miss it if one starts to).
//
// A package is a claim owner only when a var satisfying the first bullet is
// also named by the second (or the third fires directly) — matching the
// brief's own description: "a relsource.Source composite literal with a
// non-empty Claims field, registered from an init()". Declaring an unused
// Claims-bearing var, or registering a claim-less Source, is not enough on
// its own.
type packageClaims struct {
	nonEmptySourceVars map[string]bool
	registeredIdents   map[string]bool
	registeredInline   bool
}

func newPackageClaims() *packageClaims {
	return &packageClaims{
		nonEmptySourceVars: map[string]bool{},
		registeredIdents:   map[string]bool{},
	}
}

func (p *packageClaims) isClaimOwner() bool {
	if p.registeredInline {
		return true
	}
	for id := range p.registeredIdents {
		if p.nonEmptySourceVars[id] {
			return true
		}
	}
	return false
}

// localRelsourceName returns the identifier a file binds relsourceImportPath
// to ("relsource" unless the file aliases the import), or "" if the file
// does not import it at all — in which case it cannot construct or
// reference a relsource.Source and scanFile skips it immediately.
func localRelsourceName(f *ast.File) string {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != relsourceImportPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "relsource"
	}
	return ""
}

// isRelsourceSourceType reports whether expr syntactically names
// <local>.Source, where local is this file's bound name for the relsource
// import (see localRelsourceName). Does not resolve aliases or defined
// types — the same acknowledged limit as pkg/authz/slotspec/guard_test.go's
// AST-only matching; see this test's own doc comment.
func isRelsourceSourceType(expr ast.Expr, local string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == local && sel.Sel != nil && sel.Sel.Name == "Source"
}

// claimsNonEmpty reports whether a relsource.Source composite literal's
// Claims field, if present, is a slice literal with at least one element. A
// Claims value that is NOT a literal (e.g. a variable or function call) is
// conservatively treated as non-empty: missing a real claim owner is the
// failure mode this test exists to prevent, so an unrecognized shape errs
// toward "flag it" rather than "assume it's fine".
func claimsNonEmpty(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Claims" {
			continue
		}
		claimsLit, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return true
		}
		return len(claimsLit.Elts) > 0
	}
	return false
}

// scanFile records this file's contribution to pc: any package-level
// Claims-bearing relsource.Source var, and any relsource.Register(...) call
// inside a func init(), by argument shape (bare identifier or inline
// composite literal).
func scanFile(f *ast.File, pc *packageClaims) {
	local := localRelsourceName(f)
	if local == "" {
		return
	}

	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, val := range vs.Values {
				lit, ok := val.(*ast.CompositeLit)
				if !ok || !isRelsourceSourceType(lit.Type, local) {
					continue
				}
				if claimsNonEmpty(lit) {
					pc.nonEmptySourceVars[vs.Names[i].Name] = true
				}
			}
		}
	}

	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name == nil || fd.Name.Name != "init" || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok || x.Name != local || sel.Sel == nil || sel.Sel.Name != "Register" {
				return true
			}
			if len(call.Args) != 1 {
				return true
			}
			switch arg := call.Args[0].(type) {
			case *ast.Ident:
				pc.registeredIdents[arg.Name] = true
			case *ast.CompositeLit:
				if isRelsourceSourceType(arg.Type, local) && claimsNonEmpty(arg) {
					pc.registeredInline = true
				}
			}
			return true
		})
	}
}

// blankImportedPaths parses imports.go (this package's own bundle file, in
// the same directory as this test) and returns every import path it
// blank-imports (`_ "path"`).
func blankImportedPaths(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "imports.go", nil, parser.ParseComments)
	require.NoError(t, err, "parse imports.go")
	out := map[string]bool{}
	for _, imp := range f.Imports {
		if imp.Name == nil || imp.Name.Name != "_" {
			continue
		}
		out[strings.Trim(imp.Path.Value, `"`)] = true
	}
	return out
}

// TestBundleCoversEveryClaimOwner walks pkg/, internal/ and cmd/ — the same
// three roots AGENTS.md requires of every tree-walking guard test, so a
// claim owner living only in a server binary under internal/cmd is not
// silently unguarded — parses every non-test .go file, and finds every
// package that both (a) declares a relsource.Source var with a non-empty
// Claims field and (b) registers it from its own init(). It then asserts
// every such package's import path is blank-imported by imports.go.
//
// Deliberately excludes _test.go files: a test fixture that registers a
// throwaway Source with claims (e.g. the sentinel package's
// "everythingowner", or a bronze/steel-thread test double) is not a
// production claim owner and must not force a blank import into every
// binary linking this bundle.
//
// # What this guard cannot catch, stated so nobody reads it as stronger than it is
//
// Like pkg/authz/slotspec/guard_test.go's TestBoundEntitySpecIsConstructed
// InExactlyOnePlace, this matches on the NAME at the type position via
// go/ast, not full type resolution: a claim var built through a type alias,
// a helper function, or reflection would evade it. It also requires the
// familiar shape — a package-level `var X = relsource.Source{...}` (or an
// inline literal) passed DIRECTLY to `relsource.Register` inside `func
// init()` — the shape every claim owner in this tree uses today. A future
// owner that wraps Register behind its own helper function would need this
// guard extended, not silently trusted to already be covered.
func TestBundleCoversEveryClaimOwner(t *testing.T) {
	// repoRelPrefix is what every root below shares: this test file lives 5
	// directories under the repo root (imports/relsource/spicedb/authz/pkg),
	// so "../../../../../" walks back up to it, and stripping that literal
	// prefix from any walked path recovers the repo-relative directory
	// (e.g. "pkg/memory/pttagmint") regardless of which of the three roots
	// produced it.
	const repoRelPrefix = "../../../../../"
	roots := []string{repoRelPrefix + "pkg", repoRelPrefix + "internal", repoRelPrefix + "cmd"}
	byDir := map[string]*packageClaims{}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil // unreadable files are not this test's problem
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, src, 0)
			if perr != nil {
				return nil // unparseable (generated, build-tagged oddity); not this test's business
			}
			dir := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(path)), repoRelPrefix)
			pc, ok := byDir[dir]
			if !ok {
				pc = newPackageClaims()
				byDir[dir] = pc
			}
			scanFile(f, pc)
			return nil
		})
		require.NoError(t, err, "walking %s", root)
	}

	bundled := blankImportedPaths(t)

	var missing []string
	for dir, pc := range byDir {
		if !pc.isClaimOwner() {
			continue
		}
		imp := modulePrefix + dir
		if !bundled[imp] {
			missing = append(missing, imp)
		}
	}
	sort.Strings(missing)

	assert.Empty(t, missing,
		"relsource claim owner(s) not blank-imported by pkg/authz/spicedb/relsource/imports/imports.go — "+
			"add a blank import for each to that file, or CheckWrite/CheckDeleteFilter will silently ALLOW "+
			"another writer to touch their claims in every binary that links this bundle:\n%s",
		strings.Join(missing, "\n"))
}
