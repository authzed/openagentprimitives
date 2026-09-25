package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

func TestResolveAcceptMode(t *testing.T) {
	existing := DetectedSettings{Existing: true, ACMEEmail: "demo@example.test"}
	fresh := DetectedSettings{Existing: false}
	cases := []struct {
		name             string
		accept, def, yes bool
		d                DetectedSettings
		want             acceptMode
		wantErr          bool
	}{
		{name: "accept-existing on existing cluster: acceptAllExisting", accept: true, d: existing, want: acceptAllExisting},
		{name: "accept-existing on fresh cluster: loud error", accept: true, d: fresh, wantErr: true},
		{name: "no flags: review each", d: existing, want: acceptReviewEach},
		{name: "defaults only: review each (baseline handled elsewhere)", def: true, d: existing, want: acceptReviewEach},
		{name: "assume-yes only: review each (accepting offers is not keeping existing)", yes: true, d: existing, want: acceptReviewEach},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAcceptMode(tc.accept, tc.def, tc.yes, tc.d)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSeedStateFromDetected(t *testing.T) {
	t.Run("detected keys are seeded", func(t *testing.T) {
		st := tui.NewState()
		seedStateFromDetected(st, DetectedSettings{
			Existing:          true,
			ACMEEmail:         "demo@example.test",
			TrustedHostname:   "webd.demo.test",
			SandboxHostname:   "sandbox.demo.test",
			WorkspaceClass:    "ap-workspace-rwx",
			IdPKind:           "google",
			MonitoringChannel: "demo-mon",
		}, WebdRoutingOpts{})

		require.True(t, st.Has(flagACMEEmail))
		assert.Equal(t, "demo@example.test", st.Get(flagACMEEmail))

		require.True(t, st.Has(flagTrustedHostname))
		assert.Equal(t, "webd.demo.test", st.Get(flagTrustedHostname))

		require.True(t, st.Has(flagSandboxHostname))
		assert.Equal(t, "sandbox.demo.test", st.Get(flagSandboxHostname))

		require.True(t, st.Has(keyWorkspaceClass))
		assert.Equal(t, "ap-workspace-rwx", st.Get(keyWorkspaceClass))

		require.True(t, st.Has(keyIdP))
		assert.Equal(t, "google", st.Get(keyIdP))

		require.True(t, st.Has(keyMonitoring))
		assert.Equal(t, monitoringKeepValue, st.Get(keyMonitoring))
	})

	t.Run("undetected keys are NOT seeded, so the screen still asks; ExternalSpiceDB is the deliberate exception", func(t *testing.T) {
		st := tui.NewState()
		seedStateFromDetected(st, DetectedSettings{Existing: false}, WebdRoutingOpts{})

		assert.False(t, st.Has(flagACMEEmail))
		assert.False(t, st.Has(flagTrustedHostname))
		assert.False(t, st.Has(flagSandboxHostname))
		assert.False(t, st.Has(keyWorkspaceClass))
		assert.False(t, st.Has(keyIdP))
		assert.False(t, st.Has(keyMonitoring))
		// Unlike every field above, detection positively determines the SpiceDB
		// backend either way (the ConfigMap is read either way), so a bundled
		// cluster is seeded false rather than left unseeded — the N5 fix. This
		// keeps accept-all's SpiceDB screen from presenting on the common
		// (bundled) case, matching how it already short-circuits on a detected
		// external backend.
		require.True(t, st.Has(keyExternalSpiceDB))
		assert.False(t, st.Bool(keyExternalSpiceDB), "a bundled/undetected backend seeds the SpiceDB decision as bundled, not external")
	})

	t.Run("ExternalSpiceDB seeded true when detected external, so the SpiceDB screen keeps external", func(t *testing.T) {
		st := tui.NewState()
		seedStateFromDetected(st, DetectedSettings{Existing: true, ExternalSpiceDB: true, ExternalSpiceDBEndpoint: "spicedb.example.com:443"}, WebdRoutingOpts{})
		require.True(t, st.Has(keyExternalSpiceDB))
		assert.True(t, st.Bool(keyExternalSpiceDB), "a detected external backend seeds the SpiceDB decision as external")
	})

	t.Run("ExternalSpiceDB seeded false when detected bundled (in-cluster ConfigMap present), so accept-all keeps bundled without asking", func(t *testing.T) {
		st := tui.NewState()
		seedStateFromDetected(st, DetectedSettings{Existing: true, ACMEEmail: "demo@example.test", ExternalSpiceDB: false}, WebdRoutingOpts{})
		require.True(t, st.Has(keyExternalSpiceDB), "N5: a positively-detected bundled backend must still short-circuit the SpiceDB screen under accept-all")
		assert.False(t, st.Bool(keyExternalSpiceDB))
	})

	// Round-3 audit R1: a detected trusted/sandbox hostname must not be seeded
	// when the corresponding routing flag would make folding it contradictory —
	// --manual-webd-routing (routing not oap's to set up at all) and
	// --disable-artifact-viewer (no sandbox origin wanted). Seeding it anyway
	// would manufacture RunInstall's post-build mutual-exclusivity refusal over
	// a flag the operator never passed (see the doc comment above).
	t.Run("manual-webd-routing: detected trusted/sandbox hostnames are NOT seeded", func(t *testing.T) {
		st := tui.NewState()
		seedStateFromDetected(st, DetectedSettings{
			Existing:        true,
			TrustedHostname: "webd.demo.test",
			SandboxHostname: "sandbox.demo.test",
		}, WebdRoutingOpts{manualWebdRouting: true})

		assert.False(t, st.Has(flagTrustedHostname), "a detected trusted hostname must not fold under --manual-webd-routing")
		assert.False(t, st.Has(flagSandboxHostname), "a detected sandbox hostname must not fold under --manual-webd-routing")
	})

	t.Run("disable-artifact-viewer: detected sandbox hostname is NOT seeded, trusted still is", func(t *testing.T) {
		st := tui.NewState()
		seedStateFromDetected(st, DetectedSettings{
			Existing:        true,
			TrustedHostname: "webd.demo.test",
			SandboxHostname: "sandbox.demo.test",
		}, WebdRoutingOpts{disableViewer: true})

		require.True(t, st.Has(flagTrustedHostname), "the trusted hostname is unaffected by --disable-artifact-viewer")
		assert.Equal(t, "webd.demo.test", st.Get(flagTrustedHostname))
		assert.False(t, st.Has(flagSandboxHostname), "a detected sandbox hostname must not fold under --disable-artifact-viewer")
	})
}
