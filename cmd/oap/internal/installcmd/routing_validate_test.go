package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

func TestValidateWebdRouting(t *testing.T) {
	cases := []struct {
		name          string
		profile       cloud.InstallProfile
		trusted       string
		sandbox       string
		disableViewer bool
		manualRouting bool
		wantErr       string // substring; "" = no error
	}{
		{name: "managed, no host, no opt-out: error", profile: cloud.ManagedProfile, wantErr: "--trusted-hostname"},
		{name: "managed, no host, manual-routing: ok", profile: cloud.ManagedProfile, manualRouting: true},
		{name: "local profile, no host: ok", profile: cloud.DevProfile},
		{name: "unmanaged, no host: ok (service-only)", profile: cloud.ProductionProfile},
		{name: "trusted only, no sandbox, no opt-out: error", profile: cloud.ManagedProfile, trusted: "w.example.com", wantErr: "--sandbox-hostname"},
		{name: "trusted + disable-viewer: ok", profile: cloud.ManagedProfile, trusted: "w.example.com", disableViewer: true},
		{name: "trusted + sandbox distinct: ok", profile: cloud.ManagedProfile, trusted: "w.example.com", sandbox: "a.example.com"},
		{name: "trusted == sandbox: error", profile: cloud.ManagedProfile, trusted: "w.example.com", sandbox: "w.example.com", wantErr: "must differ"},
		{name: "sandbox without trusted: error", profile: cloud.ManagedProfile, sandbox: "a.example.com", wantErr: "requires --trusted-hostname"},
		{name: "trusted + manual-routing: error", profile: cloud.ManagedProfile, trusted: "w.example.com", manualRouting: true, wantErr: "mutually exclusive"},
		{name: "sandbox + disable-viewer: error", profile: cloud.ManagedProfile, trusted: "w.example.com", sandbox: "a.example.com", disableViewer: true, wantErr: "both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWebdRouting(tc.profile, tc.trusted, tc.sandbox, tc.disableViewer, tc.manualRouting)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestWithHostnameSuffix(t *testing.T) {
	// derives webd. + sandbox. and clears the suffix (idempotent)
	r, err := WebdRoutingOpts{hostnameSuffix: "example.com"}.withHostnameSuffix()
	require.NoError(t, err)
	assert.Equal(t, "webd.example.com", r.trustedHostname)
	assert.Equal(t, "sandbox.example.com", r.sandboxHostname)
	assert.Empty(t, r.hostnameSuffix, "consumed → cleared so a second call is a no-op")

	// a leading dot is tolerated
	r, err = WebdRoutingOpts{hostnameSuffix: ".example.com"}.withHostnameSuffix()
	require.NoError(t, err)
	assert.Equal(t, "webd.example.com", r.trustedHostname)

	// --disable-artifact-viewer → trusted only, no sandbox
	r, err = WebdRoutingOpts{hostnameSuffix: "example.com", disableViewer: true}.withHostnameSuffix()
	require.NoError(t, err)
	assert.Equal(t, "webd.example.com", r.trustedHostname)
	assert.Empty(t, r.sandboxHostname)

	// empty suffix → no-op
	r, err = WebdRoutingOpts{}.withHostnameSuffix()
	require.NoError(t, err)
	assert.Empty(t, r.trustedHostname)

	// mutually exclusive with explicit hostnames + with manual routing
	_, err = WebdRoutingOpts{hostnameSuffix: "example.com", trustedHostname: "x.com"}.withHostnameSuffix()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
	_, err = WebdRoutingOpts{hostnameSuffix: "example.com", manualWebdRouting: true}.withHostnameSuffix()
	require.Error(t, err)

	// idempotent: init derives, then RunInstall re-derives the already-expanded opts
	r, err = WebdRoutingOpts{hostnameSuffix: "example.com"}.withHostnameSuffix()
	require.NoError(t, err)
	r2, err := r.withHostnameSuffix()
	require.NoError(t, err)
	assert.Equal(t, "webd.example.com", r2.trustedHostname)
	assert.Equal(t, "sandbox.example.com", r2.sandboxHostname)

	// the derived pair passes validateWebdRouting on a managed cluster
	d, err := WebdRoutingOpts{hostnameSuffix: "example.com"}.withHostnameSuffix()
	require.NoError(t, err)
	assert.NoError(t, validateWebdRouting(cloud.MustFor(cloud.KeyGKE).InstallProfile(), d.trustedHostname, d.sandboxHostname, d.disableViewer, d.manualWebdRouting))
}
