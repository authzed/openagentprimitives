//go:build integration

package install_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// installOwner is the AgentClass on whose behalf clusterdeps are adopted in
// every case below.
var installOwner = types.NamespacedName{Namespace: "team-a", Name: "fake-agent"}

// fakeToolkit returns a minimal, structurally-valid SpiceboxToolkit CR (the
// only cluster-scoped v1alpha1 kind carrying a status.pin baseline today) —
// every array-typed required field is an explicit empty slice, not nil: a nil
// Go slice marshals to JSON `null`, and the real apiserver's structural
// schema (type: array, no `nullable: true`) rejects that. Mirrors the
// fixture helper in pkg/platform/oap/source/cluster_integration_test.go.
func fakeToolkit(name string) *v1alpha1.SpiceboxToolkit {
	return &v1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SpiceboxToolkitSpec{
			Name:            name,
			ToolkitRevision: "v1",
			Target:          v1alpha1.ToolkitTarget{Binary: "/usr/bin/fake-tool"},
			Parser:          v1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env:             v1alpha1.ToolkitEnv{Allowed: []v1alpha1.ToolkitEnvVar{}},
			Subcommands: []v1alpha1.ToolkitSubcommand{{
				Path: []string{"run"},
				Effects: v1alpha1.ToolkitEffects{
					Reads:      []string{},
					Writes:     []string{},
					Network:    v1alpha1.ToolkitNetworkEffect{Destinations: []string{}},
					Filesystem: v1alpha1.ToolkitFsEffect{Paths: []string{}},
					Creds:      v1alpha1.ToolkitCredsEffect{Required: []string{}, Writes: []string{}},
				},
			}},
		},
	}
}

func TestEnsureClusterDeps_Absent_NotBundled_WarnsNotErrors(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	deps := []oap.RequiredClusterDep{
		{Kind: "SpiceboxToolkit", Name: "ghost-toolkit"},
	}

	// Not carried by the bundle (bundled=nil): absent is not a hard error, but
	// it must be surfaced as a warning — never silently tolerated.
	warnings, err := install.EnsureClusterDeps(ctx, env.Client, deps, installOwner, nil)
	require.NoError(t, err, "an absent cluster dep must not fail install — it may be provisioned separately")
	require.Len(t, warnings, 1, "an absent, non-bundled required dep must produce exactly one warning")
	assert.Contains(t, warnings[0], "ghost-toolkit", "the warning must name the missing dep")
}

func TestEnsureClusterDeps_Absent_Bundled_NoWarning(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	deps := []oap.RequiredClusterDep{
		{Kind: "SpiceboxToolkit", Name: "carried-toolkit"},
	}

	// Carried by the bundle: the install's own CR apply will create it, so an
	// absent-now dep is neither an error nor a warning.
	bundled := map[string]bool{"SpiceboxToolkit/carried-toolkit": true}
	warnings, err := install.EnsureClusterDeps(ctx, env.Client, deps, installOwner, bundled)
	require.NoError(t, err)
	assert.Empty(t, warnings, "a dep the bundle itself creates must not warn even when absent now")
}

func TestEnsureClusterDeps_PreExistingCompatible_IsAdoptedNotMutated(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tk := fakeToolkit("shared-toolkit")
	require.NoError(t, env.Client.Create(ctx, tk), "create pre-existing SpiceboxToolkit")

	deps := []oap.RequiredClusterDep{
		// No pin asserted: any recorded (or absent) cluster pin is compatible.
		{Kind: "SpiceboxToolkit", Name: "shared-toolkit"},
	}

	warnings, err := install.EnsureClusterDeps(ctx, env.Client, deps, installOwner, nil)
	require.NoError(t, err)
	assert.Empty(t, warnings, "a present, adopted dep must not warn")

	var got v1alpha1.SpiceboxToolkit
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: "shared-toolkit"}, &got))
	assert.Contains(t, got.Labels, adoptguard.AdoptedLabel, "adopted label set")
	assert.Equal(t, "team-a/fake-agent", got.Annotations["agentprimitives.authzed.com/owner-OapInstall-fake-agent"])
	// Adoption is metadata-only: spec/status untouched.
	assert.Equal(t, "shared-toolkit", got.Spec.Name, "spec must be untouched by adoption")
	assert.Nil(t, got.Status.Pin, "status must be untouched by adoption")
}

func TestEnsureClusterDeps_PreExistingCompatible_MatchingPin_IsAdopted(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tk := fakeToolkit("pinned-toolkit")
	require.NoError(t, env.Client.Create(ctx, tk))
	tk.Status.Pin = &v1alpha1.PinRecord{Kind: "cli", Strength: "frozen", Digest: "sha256:abc123"}
	require.NoError(t, env.Client.Status().Update(ctx, tk), "record a matching baseline pin")

	deps := []oap.RequiredClusterDep{
		{Kind: "SpiceboxToolkit", Name: "pinned-toolkit", Pin: &oap.Pin{Digest: "sha256:abc123"}},
	}

	warnings, err := install.EnsureClusterDeps(ctx, env.Client, deps, installOwner, nil)
	require.NoError(t, err)
	assert.Empty(t, warnings)

	var got v1alpha1.SpiceboxToolkit
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: "pinned-toolkit"}, &got))
	assert.Contains(t, got.Labels, adoptguard.AdoptedLabel, "adopted label set")
	assert.Equal(t, "sha256:abc123", got.Status.Pin.Digest, "status pin untouched by adoption")
}

func TestEnsureClusterDeps_PreExistingIncompatiblePin_HardConflict_NotMutated(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tk := fakeToolkit("drifted-toolkit")
	require.NoError(t, env.Client.Create(ctx, tk))
	tk.Status.Pin = &v1alpha1.PinRecord{Kind: "cli", Strength: "frozen", Digest: "sha256:cluster-has-this"}
	require.NoError(t, env.Client.Status().Update(ctx, tk), "record a baseline pin that will disagree with the manifest")

	deps := []oap.RequiredClusterDep{
		{Kind: "SpiceboxToolkit", Name: "drifted-toolkit", Pin: &oap.Pin{Digest: "sha256:manifest-wants-this"}},
	}

	_, err := install.EnsureClusterDeps(ctx, env.Client, deps, installOwner, nil)
	require.Error(t, err, "an incompatible pin must be a hard conflict, never silently overwritten")
	assert.Contains(t, err.Error(), "sha256:manifest-wants-this")
	assert.Contains(t, err.Error(), "sha256:cluster-has-this")
	assert.Contains(t, err.Error(), "pin mismatch")

	var got v1alpha1.SpiceboxToolkit
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: "drifted-toolkit"}, &got))
	assert.NotContains(t, got.Labels, adoptguard.AdoptedLabel, "a conflicting dep must NOT be adopted")
	assert.Equal(t, "sha256:cluster-has-this", got.Status.Pin.Digest, "cluster's recorded pin must be untouched")
}
