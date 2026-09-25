package manifests_test

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// TestWorkspaceProvisionerSetupIsSandboxWritable guards that the RWX workspace
// the bundled provisioner creates is writable by the non-root sandbox uid.
//
// The sandbox/bundle containers run as uid 1000 (HardenedPodSecurityContext);
// the provisioner's helper pod creates the volume dir as root. The RWX volume
// is hostPath-backed (local-path-provisioner nodePathMap), so the
// pod's fsGroup is NOT applied — Kubernetes ignores fsGroup for hostPath — and
// the dir stays root-owned with the root group. A non-root uid that is not in
// the root group can therefore write only if the dir's "others" mode bit grants
// write: the setup script's `mkdir` mode must include world-write (0777).
//
// At 0775 the sandbox gets /workspace effectively read-only and every
// write-using tool call fails with "touch: Permission denied". The envtest
// suites cannot catch that (no kubelet, no real volume mount), so this
// manifest-level invariant is the guard.
func TestWorkspaceProvisionerSetupIsSandboxWritable(t *testing.T) {
	objs, err := manifests.Split(manifests.Install)
	require.NoError(t, err, "split install.yaml")

	var setup string
	for _, u := range objs {
		if u.GetKind() == "ConfigMap" && u.GetName() == "ap-workspace-provisioner-config" {
			s, found, nerr := unstructured.NestedString(u.Object, "data", "setup")
			require.NoError(t, nerr)
			require.True(t, found, "ap-workspace-provisioner-config must carry a setup script")
			setup = s
			break
		}
	}
	require.NotEmpty(t, setup, "ap-workspace-provisioner-config ConfigMap not found in install.yaml")

	m := regexp.MustCompile(`mkdir\s+-m\s+(\d+)`).FindStringSubmatch(setup)
	require.Len(t, m, 2, "setup script must create the workspace dir with an explicit `mkdir -m <mode>`; got:\n%s", setup)
	mode, err := strconv.ParseInt(m[1], 8, 32)
	require.NoError(t, err, "parse octal mode %q", m[1])

	// Root-owned dir + fsGroup ignored (hostPath) ⇒ the non-root sandbox uid can
	// write only through the "others" write bit (0o002).
	assert.NotZerof(t, mode&0o002,
		"workspace dir mode %#o leaves the non-root sandbox uid unable to write: the sandbox runs as uid 1000 and fsGroup is ignored for the hostPath-backed RWX volume, so the dir must be world-writable (0777)", mode)
}
