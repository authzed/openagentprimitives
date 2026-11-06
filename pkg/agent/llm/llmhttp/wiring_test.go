package llmhttp_test

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

// sdkPackages are the LLM SDKs whose NewClient builds a client on the SDK's
// default, timeout-less HTTP client.
var sdkPackages = map[string]string{
	"github.com/anthropics/anthropic-sdk-go": "anthropic",
	"github.com/openai/openai-go/v3":         "openai",
}

// allowedConstructors are the single places each SDK client is built; both
// attach llmhttp.Client().
var allowedConstructors = map[string]bool{
	"pkg/agent/llm/anthropic/anthropic.go": true,
	"pkg/agent/llm/openaicompat/client.go": true,
}

// TestNoBareSDKClientConstruction guards against a runner blocking indefinitely
// on a silent provider stream: every non-test SDK client must come from
// anthropic.NewSDKClient or openaicompat.NewClient, so none can fall back to the
// SDK default client with no timeouts. Scans every root the ship gate runs.
func TestNoBareSDKClientConstruction(t *testing.T) {
	repoRoot, err := filepath.Abs("../../../..")
	require.NoError(t, err)

	var offenders []string
	for _, root := range []string{"pkg", "internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(repoRoot, p)
			rel = filepath.ToSlash(rel)
			if allowedConstructors[rel] {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			locals := map[string]bool{}
			for _, imp := range f.Imports {
				ip, _ := strconv.Unquote(imp.Path.Value)
				def, ok := sdkPackages[ip]
				if !ok {
					continue
				}
				name := def
				if imp.Name != nil {
					name = imp.Name.Name
				}
				locals[name] = true
			}
			if len(locals) == 0 {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewClient" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && locals[id.Name] {
					offenders = append(offenders, rel)
				}
				return true
			})
			return nil
		})
		require.NoError(t, err, "walk %s", root)
	}
	assert.Empty(t, offenders,
		"build LLM SDK clients via anthropic.NewSDKClient / openaicompat.NewClient so they get llmhttp's bounded transport")
}
