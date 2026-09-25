package workshop

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// readRepoYAML reads relPath (relative to the repo root) and decodes it as
// YAML into into. rbac_browser_test.go and rbac_escalation_test.go both need
// to read a static manifest under config/ — role-builder.yaml and role.yaml
// respectively — to compare against a dynamically-built RBAC object; this is
// the one place that resolves the repo root and decodes.
func readRepoYAML(t *testing.T, relPath string, into any) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, relPath))
	require.NoError(t, err, "read %s", relPath)
	require.NoError(t, yaml.Unmarshal(data, into), "%s must decode as YAML", relPath)
}
