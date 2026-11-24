package llmhttp_test

import (
	"go/ast"
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

// sdkPackages are the LLM SDKs whose NewClient builds a client on the SDK's
// default, timeout-less HTTP client.
var sdkPackages = map[string]string{
	"github.com/anthropics/anthropic-sdk-go": "anthropic",
	"github.com/openai/openai-go/v3":         "openai",
}

// sdkOptionPackages are those SDKs' request-option packages. Their
// WithHTTPClient replaces whatever client the SDK client was built on, so a
// caller passing one drops llmhttp's bounds just as surely as a bare NewClient.
var sdkOptionPackages = map[string]string{
	"github.com/anthropics/anthropic-sdk-go/option": "option",
	"github.com/openai/openai-go/v3/option":         "option",
}

// allowedConstructors are the single places each SDK client is built; both
// attach llmhttp.Client().
var allowedConstructors = map[string]bool{
	"pkg/agent/llm/anthropic/sdkclient/sdkclient.go": true,
	"pkg/agent/llm/openaicompat/client.go":           true,
}

// guardedRoots is every tree the ship gate runs (mage test:unit).
var guardedRoots = []string{"pkg", "internal", "cmd", "test"}

// localNames maps the identifiers a file uses for the packages in want,
// honoring import aliases.
func localNames(f *ast.File, want map[string]string) map[string]bool {
	locals := map[string]bool{}
	for _, imp := range f.Imports {
		ip, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		def, ok := want[ip]
		if !ok {
			continue
		}
		if imp.Name != nil {
			def = imp.Name.Name
		}
		locals[def] = true
	}
	return locals
}

// unboundedSDKClientUses walks the non-test Go files under repoRoot/<root> and
// returns one "<file>: <pkg>.<func>" entry for every way a file outside
// allowed can end up with an SDK client that is not on llmhttp's transport:
// a bare <sdk>.NewClient, or an <sdk option>.WithHTTPClient override.
func unboundedSDKClientUses(repoRoot string, roots []string, allowed map[string]bool) ([]string, error) {
	var offenders []string
	for _, root := range roots {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if allowed[rel] {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			refused := map[string]map[string]bool{
				"NewClient":      localNames(f, sdkPackages),
				"WithHTTPClient": localNames(f, sdkOptionPackages),
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && refused[sel.Sel.Name][id.Name] {
					offenders = append(offenders, rel+": "+id.Name+"."+sel.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return offenders, nil
}

// TestNoBareSDKClientConstruction guards against a runner blocking indefinitely
// on a silent provider stream: every non-test SDK client must come from
// sdkclient.New or openaicompat.NewClient, and no caller may hand either an
// HTTP-client override, so none can end up on the SDK default client with no
// timeouts. Scans every root the ship gate runs.
func TestNoBareSDKClientConstruction(t *testing.T) {
	repoRoot, err := filepath.Abs("../../../..")
	require.NoError(t, err)

	offenders, err := unboundedSDKClientUses(repoRoot, guardedRoots, allowedConstructors)
	require.NoError(t, err, "walk the guarded roots")
	assert.Empty(t, offenders,
		"build LLM SDK clients via sdkclient.New / openaicompat.NewClient, with no option.WithHTTPClient override, so they get llmhttp's bounded transport")
}

// TestUnboundedSDKClientUses_ReportsEachBypass proves the scanner itself: a
// guard that reports nothing is indistinguishable from a guard that cannot see.
func TestUnboundedSDKClientUses_ReportsEachBypass(t *testing.T) {
	cases := []struct {
		name string
		file string
		src  string
		want []string
	}{
		{
			name: "bare anthropic NewClient under an import alias: reported",
			file: "pkg/a/a.go",
			src: `package a

import sdk "github.com/anthropics/anthropic-sdk-go"

var C = sdk.NewClient()
`,
			want: []string{"pkg/a/a.go: sdk.NewClient"},
		},
		{
			name: "bare openai NewClient under its default name: reported",
			file: "internal/b/b.go",
			src: `package b

import "github.com/openai/openai-go/v3"

var C = openai.NewClient()
`,
			want: []string{"internal/b/b.go: openai.NewClient"},
		},
		{
			name: "SDK option.WithHTTPClient override under test/: reported",
			file: "test/c/c.go",
			src: `package c

import (
	"net/http"

	"github.com/anthropics/anthropic-sdk-go/option"
)

var O = option.WithHTTPClient(http.DefaultClient)
`,
			want: []string{"test/c/c.go: option.WithHTTPClient"},
		},
		{
			name: "allowlisted constructor file: not reported",
			file: "pkg/allowed/client.go",
			src: `package allowed

import (
	oa "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

var C = oa.NewClient(option.WithHTTPClient(nil))
`,
		},
		{
			name: "another package's NewClient and WithHTTPClient: not reported",
			file: "cmd/d/d.go",
			src: `package d

import "example.com/other"

var C = other.NewClient(other.WithHTTPClient(nil))
`,
		},
		{
			name: "_test.go file: not scanned, tests stub the transport",
			file: "pkg/e/e_test.go",
			src: `package e

import sdk "github.com/anthropics/anthropic-sdk-go"

var C = sdk.NewClient()
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, r := range guardedRoots {
				require.NoError(t, os.MkdirAll(filepath.Join(root, r), 0o755))
			}
			path := filepath.Join(root, filepath.FromSlash(tc.file))
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(tc.src), 0o600))

			got, err := unboundedSDKClientUses(root, guardedRoots,
				map[string]bool{"pkg/allowed/client.go": true})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
