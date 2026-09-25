//go:build !integration && !e2e

package credkind_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// credResolvingImports name a package whose presence in a binary's transitive
// import graph means "this binary resolves credentials at runtime" — either
// directly (credresolve) or by dispatching through the registry itself.
var credResolvingImports = []string{
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve",
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry",
}

// kindRegisteringImports name every package whose init() registers at least
// one credkind.Kind. A binary that resolves credentials but imports none of
// these panics-by-error at the first registry.Get call: "unknown credential
// type", far from its cause.
//
// This is not hypothetical: internal/cmd/webd linked credresolve with zero
// kinds registered, so every browser credential-update request failed at
// runtime while go build, go vet, and the whole unit suite stayed green —
// go vet compiles a package's imports but never runs an init() to observe
// what it does or doesn't register.
var kindRegisteringImports = []string{
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports",
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/static",
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/oauth",
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/federated",
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp",
}

// TestEveryCredentialResolvingBinaryRegistersKinds loads every main package
// under cmd/ and internal/cmd/ and, for each one whose transitive import
// graph resolves credentials, asserts the same graph also registers at least
// one credkind.Kind.
//
// It skips rather than fails when the toolchain can't answer the question at
// all (no `go` on PATH, or packages.Load itself erroring for an
// environmental reason) — the point is to catch a real wiring gap, not to
// flake the suite on a sandboxed test runner.
func TestEveryCredentialResolvingBinaryRegistersKinds(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; cannot load the package graph")
	}

	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedImports | packages.NeedDeps,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg,
		"github.com/authzed/openagentprimitives/cmd/...",
		"github.com/authzed/openagentprimitives/internal/cmd/...",
	)
	if err != nil {
		t.Skipf("packages.Load failed (toolchain/module resolution unavailable in this environment): %v", err)
	}
	require.NotEmpty(t, pkgs, "expected at least one package under ./cmd/... and ./internal/cmd/...")

	// A load failure (parse/type error, or a package that genuinely can't be
	// resolved) must not let this guard pass vacuously: if packages.Load can't
	// fully resolve the module, the import graph it built cannot be trusted.
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, p.PkgPath+": "+e.Error())
		}
	})
	if len(loadErrs) > 0 {
		t.Skipf("packages.Load reported errors; cannot trust the import graph in this environment:\n%s",
			strings.Join(loadErrs, "\n"))
	}

	var mains []*packages.Package
	for _, p := range pkgs {
		if p.Name == "main" {
			mains = append(mains, p)
		}
	}
	require.NotEmpty(t, mains, "expected at least one main package under ./cmd/... and ./internal/cmd/...")

	for _, m := range mains {
		m := m
		t.Run(m.PkgPath, func(t *testing.T) {
			closure := transitiveImportPaths(m)

			resolvesCreds := false
			for _, want := range credResolvingImports {
				if closure[want] {
					resolvesCreds = true
					break
				}
			}
			if !resolvesCreds {
				return // this binary never touches credential resolution
			}

			registersKinds := false
			for _, want := range kindRegisteringImports {
				if closure[want] {
					registersKinds = true
					break
				}
			}
			assert.True(t, registersKinds,
				"%s transitively imports a credential-resolving package but registers no "+
					"credkind.Kind (missing a blank import of credkind/imports, or one of "+
					"credkind/static, credkind/oauth, credkind/federated, credkind/githubapp); registry.Get would fail "+
					"every call at runtime with \"unknown credential type\"", m.PkgPath)
		})
	}
}

// transitiveImportPaths walks p's import graph (populated by NeedDeps) and
// returns the set of every package path reachable from it, including p
// itself.
func transitiveImportPaths(p *packages.Package) map[string]bool {
	seen := map[string]bool{}
	var walk func(*packages.Package)
	walk = func(p *packages.Package) {
		if seen[p.PkgPath] {
			return
		}
		seen[p.PkgPath] = true
		for _, imp := range p.Imports {
			walk(imp)
		}
	}
	walk(p)
	return seen
}

// TestBinaryWiringGuardActuallyFires guards the guard: a check whose
// assertion never fires is worse than no check, because it reads as
// coverage. internal/cmd/runner legitimately resolves credentials AND
// blank-imports credkind/imports, so it must appear on both sides of the
// detector — proving credResolvingImports and kindRegisteringImports both
// match real import paths in the current module, not stale ones.
func TestBinaryWiringGuardActuallyFires(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; cannot load the package graph")
	}

	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedImports | packages.NeedDeps,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "github.com/authzed/openagentprimitives/internal/cmd/runner")
	if err != nil {
		t.Skipf("packages.Load failed: %v", err)
	}
	require.Len(t, pkgs, 1)
	require.Empty(t, pkgs[0].Errors, "internal/cmd/runner must load cleanly")

	closure := transitiveImportPaths(pkgs[0])

	foundResolver := false
	for _, want := range credResolvingImports {
		if closure[want] {
			foundResolver = true
		}
	}
	assert.True(t, foundResolver,
		"internal/cmd/runner is expected to transitively import a credential-resolving "+
			"package; if it does not, credResolvingImports is stale and the guard proves nothing")

	foundRegistration := false
	for _, want := range kindRegisteringImports {
		if closure[want] {
			foundRegistration = true
		}
	}
	assert.True(t, foundRegistration,
		"internal/cmd/runner is expected to transitively import a kind-registering package; "+
			"if it does not, kindRegisteringImports is stale and the guard proves nothing")
}
