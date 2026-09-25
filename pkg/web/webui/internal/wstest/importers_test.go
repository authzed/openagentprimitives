package wstest_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// importPath is this package, as an importer would write it.
const importPath = "github.com/authzed/openagentprimitives/pkg/web/webui/internal/wstest"

// TestWstestIsImportedOnlyByTests keeps a test-only helper out of shipping
// code.
//
// wstest has to be a non-test package — four packages' tests import it, and a
// `_test.go` file is not importable across packages — which means the compiler
// will just as happily link it into a serving binary. Nothing here belongs
// there: Scale exists to make a deadline longer when the race detector is on,
// and a production timeout that silently triples under an instrumented build,
// or that an environment variable can stretch, is a bug wearing a helper's
// clothes.
//
// Go's own `internal/` rule already bounds the blast radius to pkg/web/webui;
// this narrows it the rest of the way, to tests.
//
// It walks the whole repo rather than webui alone so the failure message names
// the file wherever a future move puts it, and so a copy of the import path
// into another tree is caught rather than quietly allowed by a scan that was
// never pointed at it.
func TestWstestIsImportedOnlyByTests(t *testing.T) {
	root := repoRoot(t)

	var offenders []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			// A file this walk cannot parse is a file whose imports are
			// unknown, which is not the same as a file with none — say so
			// rather than counting it clean.
			return perr
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				return uerr
			}
			if p == importPath {
				rel, rerr := filepath.Rel(root, path)
				if rerr != nil {
					rel = path
				}
				offenders = append(offenders, rel)
			}
		}
		return nil
	}))

	assert.Empty(t, offenders,
		"wstest is a test-only helper: these non-test files import it, and would carry a "+
			"race-detector-aware, env-var-stretchable timeout into shipping code")
}

// repoRoot walks up from this package until it finds the go.mod, so the test
// keeps working if the package is ever moved deeper or shallower.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "walked to the filesystem root without finding go.mod")
		dir = parent
	}
}

// skipDir prunes trees that hold no first-party Go source: vendored and
// generated code cannot be a first-party importer, and .git/node_modules are
// simply large.
func skipDir(root, path, name string) bool {
	if path == root {
		return false
	}
	switch name {
	case "vendor", "node_modules", "dist", "testdata":
		return true
	}
	return strings.HasPrefix(name, ".")
}
