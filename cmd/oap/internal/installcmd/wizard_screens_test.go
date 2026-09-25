package installcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// routingScreensByKey names the three routing screens so a test reads by intent
// rather than by slice position. It fixes p to cloud.ManagedProfile — which
// RequiresExternalHostname() — so the trusted screen is always present at
// index 0 regardless of d, keeping the existing tests' "trusted, sandbox, acme"
// indexing valid; the profile-gating behavior itself has its own tests below.
func routingScreensByKey(d DetectedSettings) (trusted, sandbox, acme tui.Screen) {
	scr := newRoutingScreens(cloud.ManagedProfile, WebdRoutingOpts{}, d)
	return scr[0], scr[1], scr[2]
}

func TestRoutingScreens_SeededKeysShortCircuit(t *testing.T) {
	ctx := context.Background()
	trusted, sandbox, acme := routingScreensByKey(DetectedSettings{})

	st := tui.NewState()
	st.Set(flagTrustedHostname, "webd.example.com")
	st.Set(flagSandboxHostname, "sandbox.example.com")
	st.Set(flagACMEEmail, "ops@example.com")

	for _, scr := range []tui.Screen{trusted, sandbox, acme} {
		g, err := scr.Prepare(ctx, st)
		require.NoError(t, err, "a seeded key must not error")
		assert.Nil(t, g, "a seeded key must short-circuit (nil group), not prompt")
		require.NoError(t, scr.Apply(ctx, st))
	}

	assert.Equal(t, "webd.example.com", st.Get(flagTrustedHostname))
	assert.Equal(t, "sandbox.example.com", st.Get(flagSandboxHostname))
	assert.Equal(t, "ops@example.com", st.Get(flagACMEEmail))
}

func TestRoutingScreens_DefaultsPrefillFromDetected(t *testing.T) {
	ctx := context.Background()
	d := DetectedSettings{
		TrustedHostname: "webd.detected.com",
		SandboxHostname: "sandbox.detected.com",
		ACMEEmail:       "detected@example.com",
	}
	trusted, sandbox, acme := routingScreensByKey(d)

	// A fresh run: each screen presents (non-nil group) and, on Apply with no
	// typed input, records its detected default — the bare-Enter "keep it" path.
	st := tui.NewState()
	g, err := trusted.Prepare(ctx, st)
	require.NoError(t, err)
	require.NotNil(t, g, "an unseeded trusted-hostname question presents a group")
	require.NoError(t, trusted.Apply(ctx, st))
	assert.Equal(t, "webd.detected.com", st.Get(flagTrustedHostname), "detected default folds back on a bare Enter")

	// Sandbox + email now run because a trusted origin exists in State.
	g, err = sandbox.Prepare(ctx, st)
	require.NoError(t, err)
	require.NotNil(t, g)
	require.NoError(t, sandbox.Apply(ctx, st))
	assert.Equal(t, "sandbox.detected.com", st.Get(flagSandboxHostname))

	g, err = acme.Prepare(ctx, st)
	require.NoError(t, err)
	require.NotNil(t, g)
	require.NoError(t, acme.Apply(ctx, st))
	assert.Equal(t, "detected@example.com", st.Get(flagACMEEmail))
}

func TestRoutingScreens_SandboxAndEmailSkipWithoutTrustedOrigin(t *testing.T) {
	ctx := context.Background()
	_, sandbox, acme := routingScreensByKey(DetectedSettings{})

	st := tui.NewState() // no trusted hostname → nothing to be a second origin to
	for name, scr := range map[string]tui.Screen{"sandbox": sandbox, "acme email": acme} {
		_, err := scr.Prepare(ctx, st)
		assert.Truef(t, errors.Is(err, tui.ErrSkip), "%s must Skip when there is no trusted origin", name)
	}
}

func TestRoutingScreens_EmailSkipsWhenTLSIssuerNamed(t *testing.T) {
	ctx := context.Background()
	_, _, acme := routingScreensByKey(DetectedSettings{})

	st := tui.NewState()
	st.Set(flagTrustedHostname, "webd.example.com")
	st.Set(flagTLSIssuer, "my-own-issuer") // operator brought their own issuer

	_, err := acme.Prepare(ctx, st)
	assert.True(t, errors.Is(err, tui.ErrSkip),
		"the ACME email must not be asked when --tls-issuer named an issuer")
}

func TestProceedScreen_SeededTrueShortCircuits(t *testing.T) {
	ctx := context.Background()
	scr := newProceedScreen()

	st := tui.NewState()
	st.SetBool(keyProceed, true)

	g, err := scr.Prepare(ctx, st)
	require.NoError(t, err)
	assert.Nil(t, g, "a seeded proceed answer must short-circuit (nil group)")
	require.NoError(t, scr.Apply(ctx, st))
	assert.True(t, proceedConfirmed(st), "seeded true proceeds")
}

func TestProceedScreen_NoAnswerBlocksProceed(t *testing.T) {
	ctx := context.Background()
	scr := newProceedScreen()

	st := tui.NewState()
	st.SetBool(keyProceed, false) // the user said no

	g, err := scr.Prepare(ctx, st)
	require.NoError(t, err)
	assert.Nil(t, g, "a seeded answer short-circuits regardless of value")
	require.NoError(t, scr.Apply(ctx, st))
	assert.False(t, proceedConfirmed(st), "an explicit no must not proceed")
}

func TestProceedConfirmed_AbsentKeyDefaultsToProceed(t *testing.T) {
	// Default:true — a run that never reached the screen proceeds, matching
	// confirmPlan's "silence proceeds".
	assert.True(t, proceedConfirmed(tui.NewState()))
}

func TestProceedGuidance_NamesHTTPSOriginWhenPresent(t *testing.T) {
	withHost := tui.NewState()
	withHost.Set(flagTrustedHostname, "webd.example.com")
	g := proceedGuidance(withHost)
	assert.Contains(t, g, "https://webd.example.com", "the summary names the HTTPS origin")
	assert.Contains(t, g, "~3–7 minutes", "an HTTPS install carries the load-balancer time estimate")

	noHost := proceedGuidance(tui.NewState())
	assert.NotContains(t, noHost, "https://", "no trusted origin → no HTTPS line")
	assert.Contains(t, noHost, "~2–5 minutes", "a hostname-less install carries the shorter estimate")
}

// TestRoutingScreens_ManualWebdRoutingReturnsNoScreens is M4's fix 1: an
// operator who passed --manual-webd-routing must never be asked for a
// hostname the wizard would then fold into WebdRoutingOpts and only THEN have
// validateWebdRouting refuse ("--trusted-hostname and --manual-webd-routing
// are mutually exclusive") — after the build phase already ran. Mirrors
// webdRoutingInputs' own early return (routing_ask.go:58-60).
func TestRoutingScreens_ManualWebdRoutingReturnsNoScreens(t *testing.T) {
	scr := newRoutingScreens(cloud.ManagedProfile, WebdRoutingOpts{manualWebdRouting: true}, DetectedSettings{})
	assert.Empty(t, scr, "manual-webd-routing must suppress every routing screen")
}

// TestRoutingScreens_DisableViewerSkipsSandbox is M4's fix 2: an operator who
// passed --disable-artifact-viewer must never be asked for a sandbox
// hostname — an answer there is exactly what makes RunInstall's
// validateWebdRouting refuse ("--sandbox-hostname and
// --disable-artifact-viewer cannot both be set"), after the build already
// ran. Mirrors webdRoutingInputs' own gate (routing_ask.go:82).
func TestRoutingScreens_DisableViewerSkipsSandbox(t *testing.T) {
	ctx := context.Background()
	scr := newRoutingScreens(cloud.ManagedProfile, WebdRoutingOpts{disableViewer: true}, DetectedSettings{})
	require.Len(t, scr, 3, "the sandbox screen is still built — it Skips, rather than being omitted")
	sandbox := scr[1]

	st := tui.NewState()
	st.Set(flagTrustedHostname, "webd.example.com") // a trusted origin exists…
	_, err := sandbox.Prepare(ctx, st)
	assert.Truef(t, errors.Is(err, tui.ErrSkip),
		"the sandbox screen must Skip under --disable-artifact-viewer even with a trusted origin present")
}

// TestRoutingScreens_TrustedScreenGatedOnProfile is M4's lesser fix 3: a kind
// that tolerates an install with no external hostname (local, desktop, the
// unmanaged default) must not be asked for one when nothing is already known
// about it — mirroring webdRoutingInputs' askTrusted gate (routing_ask.go:67).
// A kind that requires an external hostname is asked regardless.
func TestRoutingScreens_TrustedScreenGatedOnProfile(t *testing.T) {
	t.Run("hostname-tolerant kind, nothing detected: trusted screen absent", func(t *testing.T) {
		scr := newRoutingScreens(cloud.DevProfile, WebdRoutingOpts{}, DetectedSettings{})
		require.Len(t, scr, 2, "only sandbox + acme screens are built; the trusted screen is omitted")
		for _, s := range scr {
			assert.NotEqual(t, flagTrustedHostname, s.ID(), "the trusted screen must not be present")
		}
	})

	t.Run("hostname-tolerant kind, a hostname was detected: trusted screen present", func(t *testing.T) {
		scr := newRoutingScreens(cloud.DevProfile, WebdRoutingOpts{}, DetectedSettings{TrustedHostname: "webd.detected.com"})
		require.Len(t, scr, 3, "a detected hostname is worth confirming even on a tolerant kind")
		assert.Equal(t, flagTrustedHostname, scr[0].ID())
	})

	t.Run("hostname-requiring kind, nothing detected: trusted screen present", func(t *testing.T) {
		scr := newRoutingScreens(cloud.ManagedProfile, WebdRoutingOpts{}, DetectedSettings{})
		require.Len(t, scr, 3, "a managed cloud is always asked for a hostname")
		assert.Equal(t, flagTrustedHostname, scr[0].ID())
	})
}

func TestApplyRoutingAnswers_FoldsStateIntoOpts(t *testing.T) {
	st := tui.NewState()
	st.Set(flagTrustedHostname, "webd.example.com")
	st.Set(flagSandboxHostname, "sandbox.example.com")
	st.Set(flagACMEEmail, "ops@example.com")

	got := applyRoutingAnswers(WebdRoutingOpts{}, st)
	assert.Equal(t, "webd.example.com", got.trustedHostname)
	assert.Equal(t, "sandbox.example.com", got.sandboxHostname)
	assert.Equal(t, "ops@example.com", got.acmeEmail)
}

func TestApplyRoutingAnswers_BlankAnswersLeaveFlagValues(t *testing.T) {
	// A local install: the routing screens recorded empty answers. A flag-supplied
	// value must survive rather than be overwritten with "".
	base := WebdRoutingOpts{trustedHostname: "flag.example.com"}

	st := tui.NewState()
	st.Set(flagTrustedHostname, "")
	st.Set(flagSandboxHostname, "")
	st.Set(flagACMEEmail, "")

	got := applyRoutingAnswers(base, st)
	assert.Equal(t, "flag.example.com", got.trustedHostname, "a blank answer must not clobber a flag value")
	assert.Empty(t, got.sandboxHostname)
	assert.Empty(t, got.acmeEmail)
}
