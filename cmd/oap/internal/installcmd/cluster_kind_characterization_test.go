// CHARACTERIZATION TESTS. These pin the behavior of the install-shape decisions
// as they are TODAY, before the cluster-kind refactor migrates them onto
// cloud.InstallProfile. They call each function at its CURRENT signature.
//
// Tasks 8, 9, and 11 rewrite these calls to the new signatures while keeping
// every expected value byte-for-byte identical — that rewrite is the proof that
// the refactor preserved behavior.
//
// AN EXPECTED VALUE HERE MUST NEVER BE EDITED TO MATCH NEW BEHAVIOR. If one of
// these goes red during a migration, the migration is wrong.
package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/imagemode"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// allKinds is every registered kind, so a case matrix cannot silently omit one.
func allKinds() []string {
	return []string{cloud.KeyLocal, cloud.KeyDefault, cloud.KeyGKE, cloud.KeyEKS, cloud.KeyAKS}
}

// site 1: SpiceDB datastore mode. Memory iff localMode.
func TestCharacterizationSpiceDBModeFollowsLocalMode(t *testing.T) {
	assert.Equal(t, SpiceDBMemory, spicedbModeFor(cloud.DatastoreMemory))
	assert.Equal(t, SpiceDBPostgres, spicedbModeFor(cloud.DatastorePostgres))
}

// site 1b: the first-boot rule — only the ephemeral engine may seed its own
// schema at startup.
func TestCharacterizationOnlyMemoryModeBootstrapsAtStartup(t *testing.T) {
	assert.True(t, spicedbModeFor(cloud.DatastoreMemory).bootstrapsSchemaAtStartup())
	assert.False(t, spicedbModeFor(cloud.DatastorePostgres).bootstrapsSchemaAtStartup())
}

// profileFor reproduces, for this test's matrix ONLY, today's RunInstall
// combination of a per-invocation localMode override and the detected kind's
// own profile: localMode always wins (the developer profile) regardless of
// what the kind itself would otherwise answer. This is exactly the shape of
// today's `!localMode && c.IsManaged()` guard, expressed as a profile pick
// instead of a second bool parameter threaded through validateWebdRouting.
// It is deliberately NOT reused by production code: RunInstall no longer
// takes a localMode parameter at all (the caller passes the resolved
// Strategy directly — cloud.MustFor(cloud.KeyLocal) when local behavior is
// wanted), so nothing outside this characterization matrix needs to combine
// the two independently.
func profileFor(strat cloud.Strategy, localMode bool) cloud.InstallProfile {
	if localMode {
		return cloud.DevProfile
	}
	return strat.InstallProfile()
}

// site 6: the external-hostname guard, over the full localMode × kind matrix.
// Today's rule is `!localMode && c.IsManaged()`, so an install with no hostname
// and no opt-out is refused on exactly the managed clouds, and only when not
// local.
func TestCharacterizationHostnameGuardOverLocalModeAndKind(t *testing.T) {
	for _, key := range allKinds() {
		strat := cloud.MustFor(key)
		managed := strat.IsManaged()
		for _, localMode := range []bool{true, false} {
			wantErr := !localMode && managed
			name := key
			if localMode {
				name += " +localMode"
			}
			if wantErr {
				name += ": no hostname refused"
			} else {
				name += ": no hostname allowed"
			}
			t.Run(name, func(t *testing.T) {
				err := validateWebdRouting(profileFor(strat, localMode), "", "", false, false)
				if wantErr {
					require.Error(t, err)
					assert.Contains(t, err.Error(), "--hostname-suffix")
					return
				}
				assert.NoError(t, err)
			})
		}
	}
}

// site 6b: --manual-webd-routing is an unconditional opt-out on every kind.
func TestCharacterizationManualRoutingOptsOutOnEveryKind(t *testing.T) {
	for _, key := range allKinds() {
		t.Run(key+": manual routing accepted", func(t *testing.T) {
			assert.NoError(t, validateWebdRouting(profileFor(cloud.MustFor(key), false), "", "", false, true))
		})
	}
}

// site 7: image mode, over the full matrix. Today: an explicit registry wins;
// then localMode or a locally-loadable kube-context; then an unresolvable
// context on a non-managed cloud; else needsRemote.
func TestCharacterizationImageModeOverLocalModeAndKind(t *testing.T) {
	cases := []struct {
		name       string
		reg        string
		localMode  bool
		kctx       string
		kind       string
		wantReg    string
		wantLocal  bool
		wantRemote bool
	}{
		{"explicit registry wins over everything", "myreg.io/ap", false, "remote-ctx", cloud.KeyGKE, "myreg.io/ap", false, false},
		{"explicit registry wins even under localMode", "myreg.io/ap", true, "kind-ap", cloud.KeyLocal, "myreg.io/ap", false, false},
		{"localMode on a managed cloud still forces local images", "", true, "remote-ctx", cloud.KeyEKS, "", true, false},
		{"kind- context is locally loadable regardless of kind", "", false, "kind-ap", cloud.KeyGKE, "", true, false},
		{"unresolvable context on a non-managed cloud is treated as local", "", false, "", cloud.KeyDefault, "", true, false},
		{"managed cloud, remote context, no registry: needsRemote", "", false, "eks-ctx", cloud.KeyEKS, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, useLocal, needsRemote, err := imagemode.Resolve(tc.reg, profileFor(cloud.MustFor(tc.kind), tc.localMode), tc.kctx)
			require.NoError(t, err)
			assert.Equal(t, tc.wantReg, reg)
			assert.Equal(t, tc.wantLocal, useLocal)
			assert.Equal(t, tc.wantRemote, needsRemote)
		})
	}
}

// sites 9+10: the interactive `oap init` steps. shouldPromptIdp/
// shouldPromptMonitoring take the resolved cloud.InstallProfile rather than a
// raw localMode bool (cloud.DevProfile behaves as the old localMode=true,
// cloud.ProductionProfile as localMode=false); every `want` below matches the
// pre-migration behavior for the equivalent bool, characterizing that the
// migration to InstallProfile changed nothing observable.
func TestCharacterizationInteractivePromptsOverLocalMode(t *testing.T) {
	cases := []struct {
		name        string
		profile     cloud.InstallProfile
		optOut      bool
		skipInstall bool
		isTTY       bool
		want        bool
	}{
		{"remote + TTY + no opt-out: prompts", cloud.ProductionProfile, false, false, true, true},
		{"local profile: never prompts, even on a TTY", cloud.DevProfile, false, false, true, false},
		{"remote but not a TTY: no prompt", cloud.ProductionProfile, false, false, false, false},
		{"remote, TTY, explicit opt-out: no prompt", cloud.ProductionProfile, true, false, true, false},
		{"remote, TTY, --skip-install: no prompt", cloud.ProductionProfile, false, true, true, false},
	}
	for _, tc := range cases {
		t.Run("idp: "+tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldPromptIdp(tc.profile, tc.optOut, tc.skipInstall, tc.isTTY))
		})
		t.Run("monitoring: "+tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldPromptMonitoring(tc.profile, tc.optOut, tc.skipInstall, tc.isTTY))
		})
	}
}
