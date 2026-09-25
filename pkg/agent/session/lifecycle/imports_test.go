package lifecycle

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoIODeps proves the core is pure: no k8s/NATS/SpiceDB/memory/time imports.
func TestNoIODeps(t *testing.T) {
	banned := []string{"k8s.io", "sigs.k8s.io", "nats", "spicedb", "/memory", "\"time\""}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		af, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range af.Imports {
			for _, b := range banned {
				require.NotContains(t, imp.Path.Value, strings.Trim(b, "\""), "%s must not import %s", f, b)
			}
		}
	}
}
