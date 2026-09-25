//go:build e2e

package e2e

import (
	"path/filepath"
	"runtime"
)

// TestdataDir returns the ABSOLUTE path of a shared e2e fixture directory.
//
// Tests historically passed a package-relative "testdata/<name>", which breaks
// the moment a test moves to a sibling package — `go test` runs each package
// with its OWN directory as the working directory. Copying the fixtures per
// package is not the answer either: they are genuinely shared (the
// agent-centerdot-companies bundle alone backs 11 test files), so per-package
// copies would drift apart silently.
//
// Resolving against this file's own location keeps exactly one copy of each
// fixture, reachable identically from every package. Same technique
// testenv.startShared uses to find the repo root.
func TestdataDir(name string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", name)
}
