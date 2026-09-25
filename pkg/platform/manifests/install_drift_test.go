package manifests_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// TestInstallYAMLMatchesKustomize verifies the embedded install.yaml is in
// sync with the source kustomize tree under config/. The bundle that
// oap install / oap init applies comes from this embedded byte slice; if the
// source has drifted ahead (a new controller's RBAC marker, a CRD edit,
// a new resource in the kustomization), the operator deploys with a
// stale ClusterRole and crash-loops on cache sync.
//
// Skipped when kubectl isn't on PATH (the regenerator we're checking
// against). Run `mage manifests` to fix a failure.
func TestInstallYAMLMatchesKustomize(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not on PATH; cannot regenerate manifest for drift check")
	}

	root := repoRoot(t)
	cmd := exec.Command("kubectl", "kustomize", filepath.Join(root, "config"))
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("kubectl kustomize failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Fatalf("kubectl kustomize failed: %v", err)
	}

	if bytes.Equal(out, manifests.Install) {
		return
	}

	t.Fatalf(`pkg/platform/manifests/install.yaml is out of sync with config/.

Run:
    mage manifests

then commit the regenerated install.yaml. This file is what oap install /
oap init apply; staleness silently ships missing CRDs or RBAC at deploy
time and the operator crash-loops with "failed to wait for caches to sync."

(rendered size: %d bytes; embedded size: %d bytes)`, len(out), len(manifests.Install))
}

// repoRoot returns the absolute path of the repository root by walking up
// from the test source file until it finds a directory containing go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) failed")
	dir := filepath.Dir(file)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "could not locate repo root from %s", file)
		dir = parent
	}
	t.Fatalf("repo root walk too deep from %s", file)
	return ""
}
