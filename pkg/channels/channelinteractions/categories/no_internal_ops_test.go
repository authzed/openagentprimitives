package categories_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copyFieldNames are the struct fields whose contents a channel surface shows a
// person. Excerpt.Content and Details are deliberately absent: they carry
// untrusted or diagnostic text, every surface renders them inert, and naming an
// internal object in one is the point of having them.
var copyFieldNames = map[string]bool{
	"Lead": true, "Body": true, "NextStep": true,
	"Label": true, "Value": true, "PublicNoteBody": true,
}

// copyLiteralTypes are the composite-literal types that CARRY those fields —
// the notice/interaction payloads. This is how the scan finds its own targets:
// a file is a publisher because it builds one of these, not because a
// hand-maintained list names it. A file omitted from a list leaves no trace,
// so a list would silently narrow the guard's reach.
var copyLiteralTypes = map[string][]string{
	"github.com/authzed/openagentprimitives/pkg/channels/notice": {"Args"},
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents": {
		"InteractionRequestPayload",
		"InteractionAppliedPayload",
		"InteractionField",
		"InteractionExcerpt",
	},
}

// copyTypesByImportPath and copyTypesByPackageName index copyLiteralTypes for
// the two ways a literal can name its type: qualified through an import (which
// is resolved through the file's OWN import table, so an alias cannot dodge the
// scan) and unqualified inside the declaring package itself.
var copyTypesByImportPath, copyTypesByPackageName = func() (map[string]map[string]bool, map[string]map[string]bool) {
	byPath := map[string]map[string]bool{}
	byName := map[string]map[string]bool{}
	for importPath, types := range copyLiteralTypes {
		set := map[string]bool{}
		for _, ty := range types {
			set[ty] = true
		}
		byPath[importPath] = set
		byName[path.Base(importPath)] = set
	}
	return byPath, byName
}()

// internalOpsVocabulary is what must never reach a chat surface.
//
// Each entry is something only someone with cluster access could act on. A
// reader in Slack or the web chat is usually not that person, so a shell
// command is noise they cannot use, and an internal object name leaks
// implementation vocabulary onto a product surface. The session id alone is
// enough: whoever CAN investigate looks it up from that.
//
// This is an AUDIENCE rule, not a severity one. Log lines and error returns
// are for operators and should keep every one of these terms — which is why
// this guard only inspects user-copy fields.
var internalOpsVocabulary = []string{
	"kubectl",
	"agentsession#",
	"spicedb",
	"CRD",
	"reconcile",
	"configmap",
	"namespace/",
	"status.conditions",
}

// copyViolation is one banned term found in one user-facing copy field.
type copyViolation struct {
	File   string // repo-relative, forward slashes
	Line   int
	Field  string
	Text   string
	Banned string
}

func (v copyViolation) String() string {
	return v.File + ": " + v.Field + "=" + strconv.Quote(v.Text) + " contains " + strconv.Quote(v.Banned)
}

// scanUserCopy returns every file under root/pkg and root/cmd that composes
// user-facing copy, and every internal-operations term found in one of their
// copy fields.
//
// Publishers are DISCOVERED, not listed: any file building a notice/interaction
// literal is in scope the moment it is written, in either directory tree, under
// any import alias.
//
// It reads the AST rather than lines of text, which buys three things: comments
// are structurally invisible (never expressions, so an operator-facing comment
// naming kubectl cannot register), EVERY string literal in a field's value is
// inspected rather than only one sitting directly after the field name (so
// `"a " + "b"` continuations and fmt.Sprintf format strings are covered), and a
// type is matched through the file's own import table rather than by spelling.
//
// It does NOT follow values into variables or helper functions: `Body: body` is
// invisible to it. Keeping copy inline at the literal is what keeps it
// checkable.
//
// Both halves come from one walk so the suite can assert what was SCANNED as
// well as what was found — an empty violation list means nothing unless the
// publisher list is the one you expected.
func scanUserCopy(t *testing.T, root string) (publishers []string, violations []copyViolation) {
	t.Helper()
	for _, tree := range []string{"pkg", "internal", "cmd"} {
		dir := filepath.Join(root, tree)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue // a fixture root need not have every tree
		}
		require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			require.NoError(t, rerr)
			rel = filepath.ToSlash(rel)

			isPublisher, found := scanGoFile(t, p, rel)
			if isPublisher {
				publishers = append(publishers, rel)
			}
			violations = append(violations, found...)
			return nil
		}))
	}
	return publishers, violations
}

// scanGoFile parses one file and reports whether it builds user-facing copy,
// plus every banned term in it.
func scanGoFile(t *testing.T, absPath, rel string) (isPublisher bool, violations []copyViolation) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, absPath, nil, 0)
	require.NoError(t, err, "parse %s", rel)

	// localName → import path, so a literal's type is resolved through what
	// THIS file imported rather than through how it happens to be spelled.
	imports := map[string]string{}
	for _, spec := range f.Imports {
		p, uerr := strconv.Unquote(spec.Path.Value)
		if uerr != nil {
			continue
		}
		name := path.Base(p)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = p
	}
	selfPkg := f.Name.Name

	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || !isCopyType(imports, selfPkg, cl.Type) {
			return true
		}
		isPublisher = true
		for _, lit := range copyFieldHolders(cl) {
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || !copyFieldNames[key.Name] {
					continue
				}
				ast.Inspect(kv.Value, func(v ast.Node) bool {
					bl, ok := v.(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						return true
					}
					text, uerr := strconv.Unquote(bl.Value)
					if uerr != nil {
						return true
					}
					for _, banned := range internalOpsVocabulary {
						if strings.Contains(strings.ToLower(text), strings.ToLower(banned)) {
							violations = append(violations, copyViolation{
								File: rel, Line: fset.Position(bl.Pos()).Line,
								Field: key.Name, Text: text, Banned: banned,
							})
						}
					}
					return true
				})
			}
		}
		return true
	})
	return isPublisher, violations
}

// isCopyType reports whether a composite-literal type expression names one of
// the payload types (or a slice/pointer of one).
func isCopyType(imports map[string]string, selfPkg string, e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.StarExpr:
		return isCopyType(imports, selfPkg, t.X)
	case *ast.ArrayType:
		return isCopyType(imports, selfPkg, t.Elt)
	case *ast.Ident:
		// Unqualified: only meaningful inside the package that declares it.
		return copyTypesByPackageName[selfPkg][t.Name]
	case *ast.SelectorExpr:
		id, ok := t.X.(*ast.Ident)
		if !ok {
			return false
		}
		return copyTypesByImportPath[imports[id.Name]][t.Sel.Name]
	}
	return false
}

// copyFieldHolders returns the literals whose ELEMENTS are copy fields.
//
// For `notice.Args{…}` that is the literal itself. For
// `[]channelevents.InteractionField{{Label: …}}` the outer literal's elements
// are themselves literals with their type elided, and those are what carry the
// fields. Elements that name their type explicitly are skipped here because
// ast.Inspect reaches them on their own and would otherwise report twice.
func copyFieldHolders(cl *ast.CompositeLit) []*ast.CompositeLit {
	if _, isSlice := cl.Type.(*ast.ArrayType); !isSlice {
		return []*ast.CompositeLit{cl}
	}
	var out []*ast.CompositeLit
	for _, el := range cl.Elts {
		if u, ok := el.(*ast.UnaryExpr); ok {
			el = u.X
		}
		if inner, ok := el.(*ast.CompositeLit); ok && inner.Type == nil {
			out = append(out, inner)
		}
	}
	return out
}

// A message posted into a chat thread must be readable by the person who was
// talking to the agent. This walks the real source of every notice publisher
// and fails on internal-operations vocabulary in a user-copy field.
//
// It is a source scan rather than a rendered-output assertion because the copy
// is per-call: the conformance suite drives categories with generic fixture
// text and would never see what a publisher actually writes.
func TestNoticeCopyCarriesNoInternalOpsVocabulary(t *testing.T) {
	publishers, violations := scanUserCopy(t, repoRoot(t))
	require.NotEmpty(t, publishers, "the scan found no publishers at all; it is asserting nothing")
	for _, v := range violations {
		assert.Fail(t, "operator vocabulary in user-facing copy",
			"%s:%d puts operator vocabulary %q in user-facing copy %q — "+
				"name the session and let whoever can investigate look it up",
			v.File, v.Line, v.Banned, v.Text)
	}
}

// The guard is worthless over a subset it does not name. Every binary that
// publishes a notice must be in scope, and the cluster components under
// internal/cmd/ are the ones a pkg/-rooted scan would silently drop.
func TestInternalOpsScan_ReachesCmdPublishers(t *testing.T) {
	publishers, _ := scanUserCopy(t, repoRoot(t))

	var underCmd []string
	for _, p := range publishers {
		if strings.HasPrefix(p, "internal/cmd/") {
			underCmd = append(underCmd, p)
		}
	}
	assert.NotEmpty(t, underCmd,
		"no internal/cmd/ publisher is in scope, so a notice published from a binary "+
			"(internal/cmd/runner/sidecar_refresh.go, internal/cmd/channelsd/status_watchdog.go) is never checked")
}

// plantedPublisher is a cmd/ notice publisher that breaks the rule four ways:
// in a continuation literal, in a format string, in a nested field value, and
// (legitimately) in a comment, which must NOT be reported.
//
// It is a fixture rather than a real file because a scan is only proven by a
// violation it catches. "Walks more files" and "fails on a violation in them"
// are different claims, and only the second is the one this guard makes.
const plantedPublisher = `package main

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// Operators run kubectl against the configmap; saying so HERE is fine.
func planted() *notice.Notice {
	return notice.New("planted", notice.Args{
		Lead: "Something went wrong",
		Body: "Nothing to see in this clause. " +
			"Then ask someone to run kubectl get pods.",
		NextStep: fmt.Sprintf("Check the configmap for %s.", "detail"),
		Fields: []channelevents.InteractionField{
			{Label: "Where", Value: "namespace/name"},
		},
	})
}
`

// cleanPublisher publishes copy that breaks nothing, so a scan that reports it
// is over-reporting rather than working.
const cleanPublisher = `package pipeline

import "github.com/authzed/openagentprimitives/pkg/channels/notice"

var _ = notice.Args{
	Lead:     "That didn't work",
	Body:     "Nobody could reach the thing you asked for.",
	NextStep: "Try sending it again.",
}
`

// A guard that walks files without failing on what is in them is decoration.
// This plants a violating cmd/ publisher in a throwaway tree and requires the
// scan to find both the file and every violation in it — including the ones a
// line-oriented scan would miss (a concatenation continuation, a Sprintf format
// string) and excluding the comment, which is operator-facing and allowed to
// name internals.
func TestInternalOpsScan_BitesOnAPlantedCmdViolation(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	write("go.mod", "module fixture\n")
	write("internal/cmd/runner/planted.go", plantedPublisher)
	write("pkg/channels/channelsd/pipeline/clean.go", cleanPublisher)

	publishers, violations := scanUserCopy(t, root)

	assert.Contains(t, publishers, "internal/cmd/runner/planted.go",
		"a cmd/ file that builds notice copy must be discovered")
	assert.Contains(t, publishers, "pkg/channels/channelsd/pipeline/clean.go")

	found := map[string]bool{}
	for _, v := range violations {
		found[v.Banned] = true
		assert.Equal(t, "internal/cmd/runner/planted.go", v.File,
			"the clean publisher must not be reported: %s", v)
	}
	assert.True(t, found["kubectl"],
		"a banned term in a CONTINUATION literal must be caught, not just the first clause")
	assert.True(t, found["configmap"],
		"a banned term in a Sprintf FORMAT STRING reaches the reader like any other copy")
	assert.True(t, found["namespace/"],
		"a banned term in a nested InteractionField value is still user-facing copy")
}

// repoRoot resolves the repository root from this test's working directory, so
// the scan does not depend on where `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	// .../pkg/channels/channelinteractions/categories → the repo root
	root := filepath.Join(wd, "..", "..", "..", "..")
	_, err = os.Stat(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "expected a repo root (with go.mod) at %s", root)
	return root
}
