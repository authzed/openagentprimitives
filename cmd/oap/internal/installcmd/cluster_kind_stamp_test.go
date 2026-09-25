package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// TestShouldPromptStepsFollowTheProfile documents shouldPromptIdp/
// shouldPromptMonitoring's new cloud.InstallProfile-based signature. It
// deliberately overlaps with cluster_kind_characterization_test.go's
// TestCharacterizationInteractivePromptsOverLocalMode: that test proves the
// migration from the old localMode-bool call preserved every expected value;
// this one documents the new signature directly, including the noFlag/
// skipInstall axes.
func TestShouldPromptStepsFollowTheProfile(t *testing.T) {
	cases := []struct {
		name        string
		profile     cloud.InstallProfile
		noFlag      bool
		skipInstall bool
		isTTY       bool
		want        bool
	}{
		{name: "production + TTY + no opt-out: prompts", profile: cloud.ProductionProfile, isTTY: true, want: true},
		{name: "local profile: never prompts, even on a TTY", profile: cloud.DevProfile, isTTY: true, want: false},
		{name: "production but not a TTY: no prompt", profile: cloud.ProductionProfile, isTTY: false, want: false},
		{name: "production, TTY, explicit opt-out flag: no prompt", profile: cloud.ProductionProfile, isTTY: true, noFlag: true, want: false},
		{name: "production, TTY, --skip-install: no prompt", profile: cloud.ProductionProfile, isTTY: true, skipInstall: true, want: false},
	}
	for _, tc := range cases {
		t.Run("idp: "+tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldPromptIdp(tc.profile, tc.noFlag, tc.skipInstall, tc.isTTY))
		})
		t.Run("monitoring: "+tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldPromptMonitoring(tc.profile, tc.noFlag, tc.skipInstall, tc.isTTY))
		})
	}
}

// TestClusterKindIsStampedOnOperatorAndWebd exercises stampClusterKind over
// representative kinds (local, default, gke), reusing the operatorDoc/
// deploymentDoc/deploymentEnvValue helpers install_develop_test.go already
// built for setOperatorEnv/setDeploymentEnv rather than adding near-duplicate
// doc builders.
func TestClusterKindIsStampedOnOperatorAndWebd(t *testing.T) {
	for _, key := range []string{cloud.KeyLocal, cloud.KeyDefault, cloud.KeyGKE} {
		t.Run(key, func(t *testing.T) {
			op := operatorDoc()
			wb := deploymentDoc(webdDeploymentName, webdContainerName)
			docs := []*unstructured.Unstructured{op, wb}

			require.NoError(t, stampClusterKind(docs, key))

			assert.Equal(t, key, deploymentEnvValue(t, op, clusterKindEnvName))
			assert.Equal(t, key, deploymentEnvValue(t, wb, clusterKindEnvName))
		})
	}
}

// TestClusterKindStampIsIdempotent proves the SSA contract: a byte-identical
// re-install must re-apply byte-identical env, so the stamp stays a pure
// function of the resolved kind with no wall-clock or counter leaking in.
// Mirrors install_develop_test.go's TestSetOperatorMemoryBackend "same value:
// doc is unchanged (SSA no-op)" subtest.
func TestClusterKindStampIsIdempotent(t *testing.T) {
	op := operatorDoc()
	wb := deploymentDoc(webdDeploymentName, webdContainerName)
	docs := []*unstructured.Unstructured{op, wb}

	require.NoError(t, stampClusterKind(docs, cloud.KeyGKE))
	beforeOp := op.DeepCopy()
	beforeWb := wb.DeepCopy()

	require.NoError(t, stampClusterKind(docs, cloud.KeyGKE))
	assert.Equal(t, beforeOp.Object, op.Object, "a second stamp must leave the operator doc byte-identical (SSA no-op)")
	assert.Equal(t, beforeWb.Object, wb.Object, "a second stamp must leave the webd doc byte-identical (SSA no-op)")
	assert.Equal(t, cloud.KeyGKE, deploymentEnvValue(t, op, clusterKindEnvName))
	assert.Equal(t, 1, countEnvOccurrences(t, op, clusterKindEnvName),
		"a second stamp must overwrite, not append a duplicate env entry")
}

// TestClusterKindStampReachesTheRealBundleDeployments proves stampClusterKind
// actually lands on the operator and webd Deployments as RunInstall produces
// them, not just on the hand-built fixtures TestClusterKindIsStampedOnOperatorAndWebd
// uses. Those fixtures assume operatorDeploymentName/webdDeploymentName are
// correct and that both docs are in the "base" doc set stampClusterKind is
// called against — two assumptions RunInstall itself only holds by
// construction, not by anything the compiler checks.
//
// setDeploymentEnv (install_develop.go) returns nil — success — when the
// named Deployment simply isn't present in the doc set handed to it. That is
// deliberate for its other callers (not every mutation applies to every
// Deployment), but it means a rename of spicebox-webd/spicebox-operator in
// config/, or a install-tier label change that moves either Deployment from
// the "base" region into wsDocs at install.go's
// manifests.FilterByInstallTier(docs, installTierLabelKey,
// installTierWorkspaceProvisioner) call, would make stampClusterKind on
// baseDocs a SILENT no-op — and both binaries would then crash-loop with
// "invalid --cluster-kind/AP_CLUSTER_KIND" on every subsequent install,
// because neither Deployment ever gets the env var. This test runs the exact
// same manifests.Substitute -> manifests.Split -> manifests.FilterByInstallTier
// pipeline RunInstall does, over the REAL embedded manifests.Install bytes, so
// either failure mode above turns this test red instead of surfacing only at
// deploy time.
func TestClusterKindStampReachesTheRealBundleDeployments(t *testing.T) {
	rendered, err := manifests.Substitute(manifests.Install, manifests.Tags{})
	require.NoError(t, err)
	docs, err := manifests.Split(rendered)
	require.NoError(t, err)

	// Mirror RunInstall's own split: the workspace-provisioner tier is set
	// aside first, and stampClusterKind is called on what's left ("base").
	_, baseDocs := manifests.FilterByInstallTier(docs, installTierLabelKey, installTierWorkspaceProvisioner)

	require.NoError(t, stampClusterKind(baseDocs, cloud.KeyGKE))

	op := mustFindDeployment(t, baseDocs, operatorDeploymentName)
	wb := mustFindDeployment(t, baseDocs, webdDeploymentName)
	assert.Equal(t, cloud.KeyGKE, deploymentEnvValue(t, op, clusterKindEnvName),
		"the real operator Deployment must come out of the base split stamped")
	assert.Equal(t, cloud.KeyGKE, deploymentEnvValue(t, wb, clusterKindEnvName),
		"the real webd Deployment must come out of the base split stamped")
}

// mustFindDeployment locates the Deployment named `name` in docs, failing the
// test immediately (not returning a zero value a caller could silently pass
// through) when it is absent — the exact condition
// TestClusterKindStampReachesTheRealBundleDeployments exists to catch.
func mustFindDeployment(t *testing.T, docs []*unstructured.Unstructured, name string) *unstructured.Unstructured {
	t.Helper()
	for _, d := range docs {
		if d.GetKind() == "Deployment" && d.GetName() == name {
			return d
		}
	}
	require.FailNowf(t, "deployment not found", "no Deployment named %q in the given doc set", name)
	return nil
}

// countEnvOccurrences returns how many env entries named `name` appear on d's
// first container — used to prove a repeated stampClusterKind call overwrites
// in place rather than appending a duplicate.
func countEnvOccurrences(t *testing.T, d *unstructured.Unstructured, name string) int {
	t.Helper()
	containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found, "containers must be present")
	require.NotEmpty(t, containers)
	c, ok := containers[0].(map[string]any)
	require.True(t, ok, "container[0] must be a map")
	env, ok := c["env"].([]any)
	if !ok {
		return 0
	}
	count := 0
	for _, e := range env {
		m := e.(map[string]any)
		if m["name"] == name {
			count++
		}
	}
	return count
}
