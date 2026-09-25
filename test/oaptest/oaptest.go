// Package oaptest provides a shared, valid .oap bundle fixture for tests.
//
// Tests must NOT read anything under examples/ (examples are user-facing demos,
// not fixtures — editing one to improve the demo must not break unrelated
// tests; see AGENTS.md). Any test that needs an .oap source folder on disk uses
// WriteBundle instead.
package oaptest

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

//go:embed bundle dependency_bundle
var bundleFS embed.FS

const (
	DependencyRootName          = "fixture-coordinator"
	DependencyChildName         = "fixture-translator"
	DependencyPhysicalChildName = DependencyRootName + "-" + DependencyChildName
)

// WriteBundle materializes the fixture .oap source folder into a fresh temp dir
// (cleaned up by the testing framework) and returns its path. The bundle uses
// neutral fixture names: one AgentClass "demo-class" (version 1.2.0), from
// which the manifest's identity fields are inherited (so agent.name is
// "demo-class" too) — nothing here references a real example.
func WriteBundle(t *testing.T) string {
	t.Helper()
	return writeFixture(t, "bundle")
}

// WriteDependencyBundle materializes a complete parent/child OAP graph. Both
// nodes are independently installable, and all names are test-only fixtures.
func WriteDependencyBundle(t *testing.T) string {
	t.Helper()
	return writeFixture(t, "dependency_bundle")
}

func writeFixture(t *testing.T, sourceRoot string) string {
	t.Helper()
	root := t.TempDir()
	err := fs.WalkDir(bundleFS, sourceRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceRoot, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := bundleFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatalf("oaptest: materialize %s: %v", sourceRoot, err)
	}
	return root
}
