package main

import (
	"os"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/gen/clidocs"
)

// TestGenerateCLIReference regenerates the site's CLI reference from the
// live oap command tree. It is the gated driver for pkg/gen/clidocs (NewRootCmd
// is in package main and can't be imported there). Runs only when
// OAP_GEN_CLI_DOCS names an output dir — `mage docs:cli` sets it; a normal
// `go test ./cmd/oap/` skips it.
func TestGenerateCLIReference(t *testing.T) {
	out := os.Getenv("OAP_GEN_CLI_DOCS")
	if out == "" {
		t.Skip("set OAP_GEN_CLI_DOCS=<dir> to (re)generate the CLI reference MDX")
	}
	if err := clidocs.Generate(NewRootCmd(), out); err != nil {
		t.Fatalf("clidocs.Generate: %v", err)
	}
	t.Logf("clidocs: wrote CLI reference to %s", out)
}
