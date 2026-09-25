package deplogs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	modulePath = "github.com/authzed/openagentprimitives"
	deplogsPkg = modulePath + "/pkg/platform/deplogs"
	// repoRoot is relative to this package's directory (pkg/platform/deplogs).
	repoRoot = "../../.."
)

// compilerPkgs are the SpiceDB schema-compiler packages that motivated this
// package: each logs a trace line per definition through zerolog's
// process-global logger, so anything that wraps one of them — a source
// package, or a binary that links one in — must silence it via deplogs.
// SpiceDB now uses schemadsl for both ordinary schemas and fragment imports.
var compilerPkgs = []string{
	"github.com/authzed/spicedb/pkg/schemadsl/compiler",
}

// importsOf returns the quoted import paths of a single Go file.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	require.NoError(t, err, "parse %s", path)

	out := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		out = append(out, strings.Trim(imp.Path.Value, `"`))
	}
	return out
}

// nonTestGoFilesByDir walks the repo's own source (pkg/, internal/, cmd/ and
// test/) and returns the non-test .go files grouped by directory — i.e. by
// package.
//
// internal/ holds the cluster-component binaries (internal/cmd/operator,
// runner, channelsd, webd, authzd, extractord, ...); cmd/ holds the oap CLI.
// Both roots are required: the fleet-wide trace noise this test exists to
// prevent came from the cluster components, so walking only cmd/ would leave
// the guard passing on the CLI alone while every server binary went unchecked.
//
// test/ holds the shared test harnesses (envtest, SpiceDB, oap fixtures), which
// are first-party source subject to the same rule: a harness that compiles a
// schema must silence the trace for every suite that links it.
func nonTestGoFilesByDir(t *testing.T) map[string][]string {
	t.Helper()
	byDir := map[string][]string{}
	for _, top := range []string{"pkg", "internal", "cmd", "test"} {
		root := filepath.Join(repoRoot, top)
		require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "testdata" || name == "dist" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			dir := filepath.Dir(path)
			byDir[dir] = append(byDir[dir], path)
			return nil
		}), "walk %s", root)
	}
	require.NotEmpty(t, byDir, "found no Go packages under %s — the repoRoot constant is wrong", repoRoot)
	return byDir
}

// TestSchemaCompilingPackagesImportDeplogs: any package that wraps SpiceDB's
// schema compiler must also import pkg/platform/deplogs, so the trace output is silenced
// for every binary AND every test binary that links it.
//
// This is the invariant that makes the fix structural rather than a line each
// consumer has to remember. Remembering is what failed: cmd/oap silenced the
// compiler in fdec63e9 and the other five binaries linking it never did, so the
// whole fleet wrote {"level":"trace",...} JSON into kubectl logs — and every
// `go test -v` over a schema-compiling package drowned in it.
func TestSchemaCompilingPackagesImportDeplogs(t *testing.T) {
	byDir := nonTestGoFilesByDir(t)

	imports := map[string]map[string]bool{}
	for dir, files := range byDir {
		imports[dir] = map[string]bool{}
		for _, f := range files {
			for _, imp := range importsOf(t, f) {
				imports[dir][imp] = true
			}
		}
	}

	// Each compiler package is checked — and asserted non-vacuous —
	// independently: a fragment-composing package that drops the deplogs
	// import must not go unnoticed just because some other, unrelated
	// package still imports the other compiler correctly.
	for _, compilerPkg := range compilerPkgs {
		var compilingDirs []string
		for dir := range byDir {
			if imports[dir][compilerPkg] {
				compilingDirs = append(compilingDirs, dir)
			}
		}

		require.NotEmpty(t, compilingDirs,
			"no package imports %s — this test has stopped testing anything", compilerPkg)

		for _, dir := range compilingDirs {
			assert.True(t, imports[dir][deplogsPkg],
				"%s wraps SpiceDB's schema compiler (%s) but does not import %s. Compiling a "+
					"schema there emits a trace line per definition through zerolog's "+
					"package-global logger; add a blank import of deplogs so linking this "+
					"package silences it, in binaries and in test binaries alike.", dir, compilerPkg, deplogsPkg)
		}
	}
}

// mainFuncFile returns the file in dir that declares func main, and its parsed
// AST. Build constraints are ignored deliberately: a binary whose main is
// behind a build tag still has to silence dependency logs.
func mainFuncFile(t *testing.T, dir string) (string, *ast.FuncDecl) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read %s", dir)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
		require.NoError(t, err, "parse %s", path)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == "main" {
				return path, fn
			}
		}
	}
	return "", nil
}

// callsDeplogsSilence reports whether fn's body contains a deplogs.Silence()
// call.
func callsDeplogsSilence(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Silence" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "deplogs" {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestBinariesLinkingSchemaCompilerSilenceInMain: every binary that can reach
// SpiceDB's schema compiler calls deplogs.Silence() in main().
//
// The blank imports asserted above already silence these binaries via package
// init. The explicit call is the contract that does not depend on which
// packages a binary happens to link — a future dependency logging to zerolog's
// global would otherwise reintroduce the noise in a binary nobody re-audited.
//
// The set of binaries is derived from the link graph, not transcribed: a new
// binary that reaches the compiler is held to the same rule the day it is
// added.
func TestBinariesLinkingSchemaCompilerSilenceInMain(t *testing.T) {
	// Both roots: the cluster components are main packages under ./internal/cmd,
	// the CLI is ./cmd/oap. Listing only one of them would leave `checked`
	// non-zero — the NotZero guard below would still pass — while silently
	// dropping the other half of the fleet from the assertion.
	cmd := exec.Command("go", "list",
		"-f", `{{if eq .Name "main"}}{{.Dir}}	{{join .Deps ","}}{{end}}`, "./internal/...", "./cmd/...")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go list ./internal/... ./cmd/... failed: %s", string(out))

	type mainPkg struct {
		dir  string
		deps []string
	}
	var mains []mainPkg
	var mainsUnderInternal, mainsUnderCmd int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dir, deps, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		// Match on the enclosing directory, not a substring of the whole path:
		// cmd/oap has main packages nested under its own internal/ tree, and
		// counting those as cluster components would keep mainsUnderInternal
		// non-zero even if internal/cmd disappeared entirely. The internal/cmd
		// case must be tested first — it also ends in "/cmd".
		parent := filepath.Dir(dir)
		switch {
		case strings.HasSuffix(parent, filepath.Join("internal", "cmd")):
			mainsUnderInternal++
		case strings.HasSuffix(parent, string(filepath.Separator)+"cmd"):
			mainsUnderCmd++
		}
		mains = append(mains, mainPkg{dir: dir, deps: strings.Split(deps, ",")})
	}

	// Scan-root health, asserted separately from the invariant: a per-compiler
	// `checked` count alone cannot distinguish "no binary links this compiler"
	// from "the listing lost a whole root". Both roots must still yield main
	// packages, or a future move leaves this test green while guarding half of
	// what it claims to.
	require.NotZero(t, mainsUnderInternal,
		"go list found no main package under ./internal/ — the cluster-component binaries "+
			"(internal/cmd/operator, runner, channelsd, ...) have moved or the pattern is wrong; "+
			"this test would silently stop guarding them")
	require.NotZero(t, mainsUnderCmd,
		"go list found no main package directly under a cmd/ directory — the oap CLI has moved "+
			"or the pattern is wrong; this test would silently stop guarding it")

	// Each compiler package is checked — and asserted non-vacuous —
	// independently, same reasoning as the source-level check above: a binary
	// linking only the composable compiler must not hide behind some other
	// binary that happens to link the original one correctly.
	for _, compilerPkg := range compilerPkgs {
		var checked int
		for _, m := range mains {
			if !slices.Contains(m.deps, compilerPkg) {
				continue
			}
			checked++

			path, fn := mainFuncFile(t, m.dir)
			require.NotNil(t, fn, "no func main found in %s", m.dir)
			assert.True(t, callsDeplogsSilence(fn),
				"%s links %s but its main() never calls deplogs.Silence(). Compiling a "+
					"SpiceDB schema will write a trace line per definition to this "+
					"binary's stderr — the user's terminal, or kubectl logs.", path, compilerPkg)
		}

		require.NotZero(t, checked,
			"no binary links %s — this test has stopped testing anything", compilerPkg)
	}
}
