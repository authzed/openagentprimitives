package desktop_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/desktop"
	// Blank-imported so cloud.MustFor(cloud.KeyLocal) resolves and
	// DetectedManagedKey has a real managed Strategy (GKE's gce:// prefix) to
	// match against in TestDesktopValidateRefusesManagedClusters — this test
	// package registers no other kind of its own.
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
)

// TestDesktopKindIsRegistered guards the desktop package's init(): it must
// register itself under cloud.KeyDesktop with the right display name.
func TestDesktopKindIsRegistered(t *testing.T) {
	s, err := cloud.For(cloud.KeyDesktop)
	require.NoError(t, err)
	assert.Equal(t, cloud.KeyDesktop, s.Key())
	assert.Equal(t, "oap desktop VM", s.DisplayName())
}

// TestDesktopIsOptInAndNeverDetected mirrors local's own guard: desktop must
// never be auto-selected by cloud.Detect.
func TestDesktopIsOptInAndNeverDetected(t *testing.T) {
	s := cloud.MustFor(cloud.KeyDesktop)
	assert.Empty(t, s.ProviderIDPrefix(),
		"desktop must report no providerID prefix so cloud.Detect can never select it")
	assert.Equal(t, cloud.DatastoreMemory, s.InstallProfile().SpiceDBDatastore())
}

// TestDesktopServesTheLocalWebChatWhereLocalDoesNot pins the first of the two
// differences between the kinds: desktop's network-confined VM may serve the
// single-user built-in chat; local's public ngrok tunnel may not. It drives no
// behavior — see InstallProfile.ServesLocalWebChat.
func TestDesktopServesTheLocalWebChatWhereLocalDoesNot(t *testing.T) {
	l, d := cloud.MustFor(cloud.KeyLocal), cloud.MustFor(cloud.KeyDesktop)
	assert.False(t, l.InstallProfile().ServesLocalWebChat(),
		"local (public ngrok tunnel) must not serve the single-user chat")
	assert.True(t, d.InstallProfile().ServesLocalWebChat(),
		"desktop (network-confined VM) may serve the single-user chat")
}

// TestDesktopOpensATunnelOnDemandWhereLocalAlwaysDoes pins the second, and the
// first one that actually gates something: a desktop VM is useful with no public
// tunnel at all, so one is opened only when something needs inbound webhooks,
// whereas a `local` cluster's whole dev flow IS the tunnel install opens for it.
//
// Both permit a tunnel, which is what the PublicEndpoint reconciler asks; they
// differ only in who creates the CR.
func TestDesktopOpensATunnelOnDemandWhereLocalAlwaysDoes(t *testing.T) {
	l, d := cloud.MustFor(cloud.KeyLocal), cloud.MustFor(cloud.KeyDesktop)
	assert.Equal(t, cloud.PublicEndpointAlways, l.InstallProfile().PublicEndpointPolicy(),
		"local's bring-up creates the PublicEndpoint itself")
	assert.Equal(t, cloud.PublicEndpointOnDemand, d.InstallProfile().PublicEndpointPolicy(),
		"a desktop VM opens a tunnel only when something needs one")
	assert.True(t, l.InstallProfile().PublicEndpointPolicy().AllowsTunnel())
	assert.True(t, d.InstallProfile().PublicEndpointPolicy().AllowsTunnel(),
		"OnDemand still permits a tunnel; it only changes who creates the CR")
}

// TestDesktopAndLocalShareEveryOtherProfileFact guards against the profile
// override drifting beyond the two differences above: everything else on
// DesktopDevProfile must equal DevProfile.
func TestDesktopAndLocalShareEveryOtherProfileFact(t *testing.T) {
	l, d := cloud.MustFor(cloud.KeyLocal), cloud.MustFor(cloud.KeyDesktop)
	lp, dp := l.InstallProfile(), d.InstallProfile()
	assert.Equal(t, lp.MemoryBackend(), dp.MemoryBackend())
	assert.Equal(t, lp.SpiceDBDatastore(), dp.SpiceDBDatastore())
	assert.Equal(t, lp.UsesLocalDevImages(), dp.UsesLocalDevImages())
	assert.Equal(t, lp.AllowsSharedOrigin(), dp.AllowsSharedOrigin())
	assert.Equal(t, lp.RequiresExternalHostname(), dp.RequiresExternalHostname())
	assert.Equal(t, lp.PromptsForIdentityProvider(), dp.PromptsForIdentityProvider())
	assert.Equal(t, lp.PromptsForMonitoring(), dp.PromptsForMonitoring())
	assert.Equal(t, lp.AllowsLocalOnlyIdentityProviders(), dp.AllowsLocalOnlyIdentityProviders())
}

// TestDesktopSharesLocalsCloudBehaviorViaEmbedding guards the embedding
// itself: everything other than Key/DisplayName/InstallProfile must be
// byte-identical to local.Strategy's own answers, proving desktop.Strategy
// genuinely delegates rather than re-implementing.
func TestDesktopSharesLocalsCloudBehaviorViaEmbedding(t *testing.T) {
	l, d := cloud.MustFor(cloud.KeyLocal), cloud.MustFor(cloud.KeyDesktop)
	assert.False(t, d.IsManaged())
	assert.Equal(t, l.DNSEgressCIDRs(), d.DNSEgressCIDRs())
	assert.Equal(t, l.GatewayBackendIngressCIDRs(), d.GatewayBackendIngressCIDRs())
	lDeadline, lETA := l.GatewayAddressWait()
	dDeadline, dETA := d.GatewayAddressWait()
	assert.Equal(t, lDeadline, dDeadline)
	assert.Equal(t, lETA, dETA)
	assert.Equal(t, l.RegistryFromProviderID("x"), d.RegistryFromProviderID("x"))
	assert.Equal(t, l.ProjectFromProviderID("x"), d.ProjectFromProviderID("x"))
	assert.Equal(t, l.TLS(), d.TLS())
	assert.Equal(t, l.StatefulStorage(), d.StatefulStorage())
	assert.Equal(t, l.ArtifactStorage(), d.ArtifactStorage())
}

// TestDesktopValidateRefusesManagedClusters proves Validate (defined in
// local/validate.go, inherited via embedding — desktop overrides no method
// here) genuinely runs for the desktop Strategy: a managed-cloud providerID
// is refused exactly as it would be for local.
func TestDesktopValidateRefusesManagedClusters(t *testing.T) {
	s := cloud.MustFor(cloud.KeyDesktop)
	kc := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       corev1.NodeSpec{ProviderID: "gce://proj/us-central1-a/n1"},
	})
	err := s.Validate(context.Background(), cloud.ValidateParams{Clients: cloud.Clients{Typed: kc}})
	require.Error(t, err, "desktop inherits local's managed-cloud refusal via embedding")
}
