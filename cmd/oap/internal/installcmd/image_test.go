package installcmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
)

// `oap image load` against a cluster it cannot side-load into must say so. It
// returned nil — success — after printing a note, so scripts saw an exit 0 for
// an image that never reached the cluster.
func TestLoadImageIntoClusterRemoteContextIsError(t *testing.T) {
	g := &apcmd.Globals{Context: "gke_my-proj_us-east1_prod"}
	err := loadImageIntoCluster(context.Background(), io.Discard, g, "demo-image:dev", "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "gke_my-proj_us-east1_prod")
	assert.ErrorContains(t, err, "--image-registry")
}

// docker-desktop genuinely needs no load: still a success.
func TestLoadImageIntoClusterDockerDesktopIsNoOp(t *testing.T) {
	g := &apcmd.Globals{Context: "docker-desktop"}
	assert.NoError(t, loadImageIntoCluster(context.Background(), io.Discard, g, "demo-image:dev", ""))
}

// The VM path is selected by imageload's VMLoad disposition, not by a second
// context-name comparison living in this file. Proving it still routes to the
// SSH path also proves it never falls through to exec'ing plan.Argv, which
// VMLoad leaves nil: the kubeconfig lookup that only the VM branch performs is
// what fails here, and a nil-Argv exec would panic instead.
func TestLoadImageIntoClusterAPDesktopTakesTheVMPath(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	// A valid kubeconfig that simply has no oap-desktop context, so the VM
	// branch's GuestIPFromKubeconfig is the thing that reports the problem.
	require.NoError(t, os.WriteFile(kubeconfig, []byte(
		"apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"), 0o600))

	g := &apcmd.Globals{Context: imageload.APContextName, Kubeconfig: kubeconfig}
	err := loadImageIntoCluster(context.Background(), io.Discard, g, "demo-image:dev", "")
	require.Error(t, err)
	assert.ErrorContains(t, err, imageload.APContextName, "the VM branch must be the one that reported")
	assert.NotContains(t, err.Error(), "unhandled image-load disposition")
}
