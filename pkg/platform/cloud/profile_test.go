package cloud

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDevProfileIsTheLightweightDeveloperProfile(t *testing.T) {
	p := DevProfile
	assert.Equal(t, "sqlite", p.MemoryBackend())
	assert.Equal(t, DatastoreMemory, p.SpiceDBDatastore())
	assert.True(t, p.UsesLocalDevImages())
	assert.True(t, p.AllowsSharedOrigin())
	assert.False(t, p.RequiresExternalHostname())
	assert.False(t, p.PromptsForIdentityProvider())
	assert.False(t, p.PromptsForMonitoring())
	assert.Equal(t, PublicEndpointAlways, p.PublicEndpointPolicy(),
		"`oap install` creates the PublicEndpoint on a local cluster; it has no other route to the Internet")
	assert.True(t, p.AllowsLocalOnlyIdentityProviders())
	assert.False(t, p.ServesLocalWebChat(),
		"DevProfile backs the `local` kind (--local's public ngrok tunnel): the single-user built-in chat must stay OFF there; only DesktopDevProfile turns it on")
}

func TestDesktopDevProfileMatchesDevProfileExceptItsTwoOverrides(t *testing.T) {
	p := DesktopDevProfile
	assert.Equal(t, DevProfile.MemoryBackend(), p.MemoryBackend())
	assert.Equal(t, DevProfile.SpiceDBDatastore(), p.SpiceDBDatastore())
	assert.Equal(t, DevProfile.UsesLocalDevImages(), p.UsesLocalDevImages())
	assert.Equal(t, DevProfile.AllowsSharedOrigin(), p.AllowsSharedOrigin())
	assert.Equal(t, DevProfile.RequiresExternalHostname(), p.RequiresExternalHostname())
	assert.Equal(t, DevProfile.PromptsForIdentityProvider(), p.PromptsForIdentityProvider())
	assert.Equal(t, DevProfile.PromptsForMonitoring(), p.PromptsForMonitoring())
	assert.Equal(t, DevProfile.AllowsLocalOnlyIdentityProviders(), p.AllowsLocalOnlyIdentityProviders())
	assert.True(t, p.ServesLocalWebChat(),
		"DesktopDevProfile backs the `desktop` kind (oap desktop's network-confined VM): the single-user built-in chat is safe there")
	assert.NotEqual(t, DevProfile.ServesLocalWebChat(), p.ServesLocalWebChat(),
		"ServesLocalWebChat is one of the two facts that must differ between the dev profiles")
	assert.Equal(t, PublicEndpointOnDemand, p.PublicEndpointPolicy(),
		"a desktop VM is useful with no tunnel; one is opened only when something needs inbound webhooks")
	assert.NotEqual(t, DevProfile.PublicEndpointPolicy(), p.PublicEndpointPolicy(),
		"PublicEndpointPolicy is the other, and the first override that actually gates something")
}

// TestPublicEndpointPolicyFailsClosedAtItsZeroValue is why the enum starts at
// Never. A profile that forgot to answer, or a consumer holding an unset value,
// must refuse to open a tunnel rather than open one on infrastructure that has
// real ingress and a real external URL already configured.
func TestPublicEndpointPolicyFailsClosedAtItsZeroValue(t *testing.T) {
	var unset PublicEndpointPolicy
	assert.Equal(t, PublicEndpointNever, unset, "the zero value must be the refusing one")
	assert.False(t, unset.AllowsTunnel())

	assert.True(t, PublicEndpointAlways.AllowsTunnel())
	assert.True(t, PublicEndpointOnDemand.AllowsTunnel(),
		"OnDemand permits a tunnel; it says who creates the CR, not whether one may exist")

	// An unrecognized value must also refuse — the same fail-closed reasoning
	// applied to a policy added without deciding what it means here.
	assert.False(t, PublicEndpointPolicy(99).AllowsTunnel())

	assert.Equal(t, "Never", PublicEndpointNever.String())
	assert.Equal(t, "Always", PublicEndpointAlways.String())
	assert.Equal(t, "OnDemand", PublicEndpointOnDemand.String())
	assert.Equal(t, "PublicEndpointPolicy(99)", PublicEndpointPolicy(99).String(),
		"an unnamed value must render as itself, not silently as a real policy")
}

func TestProductionProfileIsTheDurableClusterProfile(t *testing.T) {
	p := ProductionProfile
	assert.Equal(t, "postgres", p.MemoryBackend())
	assert.Equal(t, DatastorePostgres, p.SpiceDBDatastore())
	assert.False(t, p.UsesLocalDevImages())
	assert.False(t, p.AllowsSharedOrigin(),
		"a shared origin defeats the cross-origin isolation of the artifact viewer")
	// Confirmed against cmd/oap/internal/installcmd/init.go — shouldPromptIdp and
	// shouldPromptMonitoring are both `!localMode && !noX && !skipInstall &&
	// isTTY`; for a production install (localMode=false) with default flags,
	// both are true.
	assert.True(t, p.PromptsForIdentityProvider())
	assert.True(t, p.PromptsForMonitoring())
	assert.Equal(t, PublicEndpointNever, p.PublicEndpointPolicy(),
		"a durable cluster has real ingress, and `oap install` has already pointed webd's external URL at it")
	assert.False(t, p.AllowsLocalOnlyIdentityProviders(),
		"local-only IdP kinds (e.g. password) have no anti-brute-force posture")
	assert.False(t, p.ServesLocalWebChat())
}

func TestProductionProfileRequiresExternalHostnameOnlyOnManagedClouds(t *testing.T) {
	// RequiresExternalHostname is the one profile method whose answer differs
	// WITHIN the production profile: a managed cloud with no hostname is
	// reachable only via port-forward, while an on-prem cluster may legitimately
	// be service-only. Each managed kind (gke/eks/aks) returns ManagedProfile to
	// pick up the override (see TestManagedProfileRequiresAnExternalHostname
	// below); this test pins the shared production profile's own default.
	assert.False(t, ProductionProfile.RequiresExternalHostname(),
		"the shared production profile defaults to false; managed kinds override")
}

func TestManagedProfileRequiresAnExternalHostname(t *testing.T) {
	assert.True(t, ManagedProfile.RequiresExternalHostname(),
		"a managed cloud with no hostname is reachable only via kubectl port-forward")
	// It differs from ProductionProfile in exactly that one answer.
	assert.Equal(t, ProductionProfile.MemoryBackend(), ManagedProfile.MemoryBackend())
	assert.Equal(t, ProductionProfile.SpiceDBDatastore(), ManagedProfile.SpiceDBDatastore())
}

// profileOverrideStrategy lets a test pin InstallProfile() to an arbitrary
// value while satisfying the rest of Strategy via the embedded fakeStrategy
// (defined in cloud_test.go, same package).
type profileOverrideStrategy struct {
	fakeStrategy
	profile InstallProfile
}

func (s profileOverrideStrategy) InstallProfile() InstallProfile { return s.profile }

func TestNeedsBundledPostgresIsDerivedFromInstallProfile(t *testing.T) {
	cases := []struct {
		name    string
		profile InstallProfile
		want    bool
	}{
		{"dev profile: sqlite + in-memory SpiceDB datastore → no bundled postgres", DevProfile, false},
		{"production profile: postgres + postgres SpiceDB datastore → bundled postgres", ProductionProfile, true},
		{"managed profile inherits production's postgres facts → bundled postgres", ManagedProfile, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NeedsBundledPostgres(profileOverrideStrategy{profile: tc.profile}))
		})
	}
}
