package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearIdentityEnv blanks every identity input AND the probe flag, so each
// case states exactly which inputs it sets.
func clearIdentityEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		probeModeEnv,
		"OPERATOR_MEMORY_URL", "token",
		"WORKSHOP_NAMESPACE", "WORKSHOP_SESSION_NAMESPACE",
		"WORKSHOP_SESSION_NAME", "WORKSHOP_ID",
	} {
		t.Setenv(k, "")
	}
}

// TestBuildServer_ProbeFlag_DeclarationOnly: declaration-only is entered ONLY
// when the admission probe explicitly asks for it (AP_PROBE_MODE), because
// only buildProbePod sets that env. This is what lets the probe pod — which
// has no Workshop identity and never will — serve tools/list to pass the
// reachability check.
func TestBuildServer_ProbeFlag_DeclarationOnly(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv(probeModeEnv, "1")

	srv, declOnly, err := buildServer()
	require.NoError(t, err, "the admission probe must serve declarations without an identity")
	require.NotNil(t, srv)
	assert.True(t, declOnly, "AP_PROBE_MODE must select declaration-only")
}

// TestBuildServer_NoIdentityNoProbeFlag_FailsClosed is the fix for the live
// race on oap-desktop 2026-09-11: a SESSION sidecar boots identity-less during
// the ~60s its Workshop provisions. It looks input-identical to the probe
// (only MCP_PORT), but it is NOT a probe — its identity is COMING. It must
// fail-closed at boot (crash) so the runner's reachability boot-probe WAITS,
// rather than serve declaration-only and let the runner run its turns against
// dead tools before the sidecar is rebuilt with its token. Only the explicit
// probe flag — never mere input-absence — enters declaration-only.
func TestBuildServer_NoIdentityNoProbeFlag_FailsClosed(t *testing.T) {
	clearIdentityEnv(t)

	_, _, err := buildServer()
	require.Error(t, err, "an identity-less sidecar that is NOT the probe must fail closed, not degrade to declaration-only")
	assert.Contains(t, err.Error(), "workshop SA token",
		"the error must name the missing identity so the runner boot-probe and the operator log both see the real cause")
}

// TestBuildServer_PartialIdentity_StillFailsClosed: a partial identity is a
// mis-wired sidecar and must fail closed regardless of the probe flag being
// absent — same fail-closed posture, unchanged.
func TestBuildServer_PartialIdentity_StillFailsClosed(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("OPERATOR_MEMORY_URL", "http://operator:8082")

	_, _, err := buildServer()
	require.Error(t, err, "one identity input present + the rest absent = misconfiguration")
	assert.Contains(t, err.Error(), "workshop SA token")
}

// TestWorkshopRESTConfig_FailsClosedNamingMissingPiece pins the fix for the
// live crash on oap-desktop 2026-09-11: the client was built with
// rest.InClusterConfig, which reads the token AND ca.crt from the default
// automount path — absent here (automount off), so it failed even with the
// workshop token present at its own mount. workshopRESTConfig reads only the
// projected mount + in-pod env, and fails closed naming exactly what is
// missing. With no token file present, the token is the first named gap.
func TestWorkshopRESTConfig_FailsClosedNamingMissingPiece(t *testing.T) {
	// The token path is absolute and cannot exist on a test box, which is the
	// point — this is exactly the state a mis-projected pod is in.
	_, err := workshopRESTConfig()
	require.Error(t, err, "no projected token/CA on a test box → fail closed")
	assert.Contains(t, err.Error(), "workshop SA token",
		"the error must name the missing identity piece, not a generic dial failure")
}
