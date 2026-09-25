package provenance_test

import (
	"go/ast"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// allowedSigningPackages may construct a SigningMemory. Everything else is
// forbidden by construction.
//
//	channelsd / authzd / operator — component publishers with their own
//	                                registered keys; never a session's.
//	runner                        — IS the session; legitimately holds its seed.
//	e2e                           — the in-process harness stands in for all of
//	                                the above.
//
// webd and cmd/oap are ABSENT on purpose. Both used to read the per-session
// Ed25519 audit-signing seed out of <name>-memory-token and sign inbound turns
// as the session publisher. A browser-facing service (and a laptop CLI) must
// not hold a credential that can append to or forge the audit log — see
// pkg/memory/tokens/tokens.go:146-155. Inbound now goes through channelsd.
//
// If you are here because this test failed: do NOT add your package. Route the
// write through channelsd's .in.view_message request/reply instead.
var allowedSigningPackages = []string{
	"github.com/authzed/openagentprimitives/internal/cmd/authzd",
	"github.com/authzed/openagentprimitives/internal/cmd/channelsd",
	"github.com/authzed/openagentprimitives/internal/cmd/operator",
	"github.com/authzed/openagentprimitives/internal/cmd/runner",
	"github.com/authzed/openagentprimitives/test/e2e",
	"github.com/authzed/openagentprimitives/pkg/memory/provenance",
}

// callersOfNewSigningMemory returns the import paths of every non-test package
// in the module that names provenance.NewSigningMemory.
//
// It loads with the e2e+integration build tags so build-tagged files are
// scanned too — the e2e harness is exactly where an unnoticed re-introduction
// would hide from `go test ./...`.
func callersOfNewSigningMemory(t *testing.T) []string {
	t.Helper()
	// Parse-only mode — deliberately NOT NeedTypes/NeedTypesInfo. The walk below
	// is purely syntactic (it matches a selector named "NewSigningMemory"; it
	// never consults type info), so type-checking buys this tripwire nothing. It
	// costs a great deal, though: NeedTypes makes packages.Load compile the whole
	// dependency graph of every module package under -tags=e2e,integration, three
	// times (once per test function). On a cold build cache that overran the
	// 10-minute test timeout and wedged `mage test:unit` in CI. NeedCompiledGoFiles
	// is what gives NeedSyntax the file list to parse; without it Syntax comes back
	// empty and the tripwire passes vacuously (TestTripwireActuallyFires catches
	// exactly that).
	cfg := &packages.Config{
		Mode:       packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax,
		BuildFlags: []string{"-tags=e2e,integration"},
		Dir:        moduleRoot(t),
		Tests:      false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	require.NoError(t, err, "packages.Load must succeed")
	require.NotEmpty(t, pkgs)

	// A load failure (parse/type error in a scanned package) must not let the
	// tripwire pass vacuously: if packages.Load can't fully resolve the module,
	// we can't trust that the AST walk saw everything. Fail loudly instead of
	// silently reporting a partial (and falsely reassuring) caller set.
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, p.PkgPath+": "+e.Error())
		}
	})
	require.Empty(t, loadErrs, "packages.Load reported errors; tripwire cannot trust its result:\n%s", strings.Join(loadErrs, "\n"))

	seen := map[string]bool{}
	for _, p := range pkgs {
		if p.PkgPath == "github.com/authzed/openagentprimitives/pkg/memory/provenance" {
			continue // the definition site
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel != nil && sel.Sel.Name == "NewSigningMemory" {
					seen[p.PkgPath] = true
				}
				return true
			})
		}
	}
	var got []string
	for p := range seen {
		got = append(got, p)
	}
	sort.Strings(got)
	return got
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	require.NoError(t, err, "go list -m must resolve the module root")
	return strings.TrimSpace(string(out))
}

func TestOnlyTrustedPackagesConstructSigningMemory(t *testing.T) {
	allowed := map[string]bool{}
	for _, p := range allowedSigningPackages {
		allowed[p] = true
	}
	for _, p := range callersOfNewSigningMemory(t) {
		assert.True(t, allowed[p],
			"package %s constructs a SigningMemory but is not in allowedSigningPackages. "+
				"Do NOT add it — route the write through channelsd's .in.view_message request/reply. "+
				"See pkg/memory/tokens/tokens.go:146-155.", p)
	}
}

// TestTripwireActuallyFires guards the guard: a tripwire whose assertion never
// fires is worse than no tripwire, because it reads as coverage. We prove the
// detector sees a known-true caller (internal/cmd/runner legitimately signs).
func TestTripwireActuallyFires(t *testing.T) {
	assert.Contains(t, callersOfNewSigningMemory(t),
		"github.com/authzed/openagentprimitives/internal/cmd/runner",
		"the detector must find internal/cmd/runner, which legitimately constructs a SigningMemory; "+
			"if it does not, the AST walk is broken and this tripwire proves nothing")
}

// TestForbiddenPackagesDoNotSign is the point of the whole exercise, stated
// positively so a failure reads as the security regression it is.
func TestForbiddenPackagesDoNotSign(t *testing.T) {
	got := callersOfNewSigningMemory(t)
	for _, forbidden := range []string{
		"github.com/authzed/openagentprimitives/pkg/web/webui/chat",
		"github.com/authzed/openagentprimitives/cmd/oap",
	} {
		assert.NotContains(t, got, forbidden,
			"%s must never sign audit entries: it would be able to forge a session's "+
				"hash chain. Route inbound through channelsd.", forbidden)
	}
}
