//go:build mage
// +build mage

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/magefile/mage/sh"

	"github.com/authzed/openagentprimitives/pkg/gen/crddocs"
)

// cliDocsDir is where the generated reference MDX lands, alongside the
// hand-written guides the site renders.
const cliDocsDir = "site/content/docs"

// crdSchemaDir holds the controller-gen CRD YAMLs the CRD reference is built from.
const crdSchemaDir = "config/crds"

// Crd regenerates the site's CRD reference (one MDX page per CustomResource
// kind) from the CRD schemas under config/crds. Unlike Cli it runs in-process — it
// reads YAML, so it needs no seam into package main. Run `mage gen:api` first if
// the CRD schemas are stale relative to the *_types.go.
func (Docs) Crd() error {
	if err := crddocs.Generate(crdSchemaDir, cliDocsDir); err != nil {
		return err
	}
	fmt.Printf("crddocs: wrote CRD reference to %s\n", cliDocsDir)
	return nil
}

// Cli regenerates the site's CLI reference (one MDX page per `oap`
// command family) from the live cobra tree. The walk lives in pkg/gen/clidocs;
// it is driven here by the gated TestGenerateCLIReference in cmd/oap, because
// NewRootCmd is package main and can only be reached from within that package.
func (Docs) Cli() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	// `go test` runs with cwd = the package dir, so the generator must be handed
	// an ABSOLUTE output path, not one relative to this (repo-root) invocation.
	out := filepath.Join(wd, cliDocsDir)
	env := map[string]string{"OAP_GEN_CLI_DOCS": out}
	if err := sh.RunWith(env, "go", "test", "-run", "TestGenerateCLIReference", "-count=1", "./cmd/oap/"); err != nil {
		return err
	}
	fmt.Printf("clidocs: wrote CLI reference to %s\n", out)
	return nil
}
