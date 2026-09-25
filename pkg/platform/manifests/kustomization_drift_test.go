package manifests_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestKustomizationCoversEveryCRDFile catches a CRD file that exists on disk
// but is missing from config/crds/kustomization.yaml's resources: list. Such a
// CRD never reaches install.yaml, so its controller's informer waits ~2 minutes
// for a resource the cluster does not know about and the manager crash-loops on
// "failed to wait for caches to sync."
//
// TestInstallYAMLMatchesKustomize cannot catch that: when kustomization.yaml
// itself is wrong, install.yaml matches it just fine and both lack the CRD.
//
// This test enforces a stricter invariant by going directly to disk:
//
//  1. Every *.yaml file in config/crds/ (other than kustomization.yaml)
//     MUST appear in kustomization.yaml's resources: list.
//  2. Every entry in kustomization.yaml's resources: list MUST exist
//     as a file on disk (catches dangling references after a rename
//     or delete).
func TestKustomizationCoversEveryCRDFile(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "config", "crds")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read dir %s", dir)

	yamlFiles := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") || name == "kustomization.yaml" {
			continue
		}
		yamlFiles[name] = true
	}

	kPath := filepath.Join(dir, "kustomization.yaml")
	kBytes, err := os.ReadFile(kPath)
	require.NoError(t, err, "read %s", kPath)

	var k struct {
		Resources []string `json:"resources"`
	}
	// sigs.k8s.io/yaml unmarshals via JSON tags.
	require.NoError(t, yaml.Unmarshal(kBytes, &k), "unmarshal kustomization.yaml")

	listed := map[string]bool{}
	for _, r := range k.Resources {
		listed[r] = true
	}

	// (1) Every CRD file on disk must be referenced by kustomization.yaml.
	for f := range yamlFiles {
		assert.True(t, listed[f],
			"config/crds/%s exists on disk but is not listed in "+
				"kustomization.yaml — install.yaml will not include this CRD "+
				"and any controller watching the resource will time out "+
				"waiting for cache sync at startup", f)
	}

	// (2) Every referenced resource must exist on disk.
	for r := range listed {
		assert.True(t, yamlFiles[r],
			"kustomization.yaml references %q but no such file exists "+
				"in config/crds/ — kustomize build will fail", r)
	}
}
