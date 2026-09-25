package unmanaged_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
	// Blank-imported so DetectedManagedKey has a real managed Strategy (GKE's
	// gce:// prefix) to match against in TestValidateNeverHonorsAllowOverride —
	// this test package registers no managed cloud of its own.
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
)

func TestDefaultKindIsRegisteredAndIsTheFallback(t *testing.T) {
	s, err := cloud.Default()
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, cloud.KeyDefault, s.Key())

	byKey, err := cloud.For(cloud.KeyDefault)
	require.NoError(t, err)
	assert.Equal(t, s.Key(), byKey.Key(), "Default() and For(KeyDefault) must agree")
}

func TestDefaultKindCarriesTheProductionProfile(t *testing.T) {
	s := cloud.MustFor(cloud.KeyDefault)
	p := s.InstallProfile()
	// The `default` kind must behave exactly as the anonymous default did for a
	// non---local install: postgres everywhere, no project-managed tunnel.
	assert.Equal(t, "postgres", p.MemoryBackend())
	assert.Equal(t, cloud.DatastorePostgres, p.SpiceDBDatastore())
	assert.Equal(t, cloud.PublicEndpointNever, p.PublicEndpointPolicy(),
		"a durable cluster has real ingress; no project-managed tunnel may reach it")
	assert.False(t, p.RequiresExternalHostname(),
		"on-prem may legitimately be service-only; only managed clouds require a hostname")
}

func TestLocalKindIsOptInAndNeverDetected(t *testing.T) {
	s := cloud.MustFor(cloud.KeyLocal)
	assert.Equal(t, cloud.KeyLocal, s.Key())
	assert.Empty(t, s.ProviderIDPrefix(),
		"local must report no providerID prefix so Detect can never select it")
	assert.Equal(t, cloud.DatastoreMemory, s.InstallProfile().SpiceDBDatastore())
}

func TestDefaultAndLocalShareTheirCloudBehavior(t *testing.T) {
	// unmanaged inherits local's TLS/Workspace/Stateful/Gateway wholesale, so
	// on-prem installs and `mage smoke` are byte-identical to today.
	l, d := cloud.MustFor(cloud.KeyLocal), cloud.MustFor(cloud.KeyDefault)
	assert.False(t, l.IsManaged())
	assert.False(t, d.IsManaged())
	assert.Equal(t, l.DNSEgressCIDRs(), d.DNSEgressCIDRs())
	assert.Equal(t, l.GatewayBackendIngressCIDRs(), d.GatewayBackendIngressCIDRs())
	lDeadline, lETA := l.GatewayAddressWait()
	dDeadline, dETA := d.GatewayAddressWait()
	assert.Equal(t, lDeadline, dDeadline)
	assert.Equal(t, lETA, dETA)
}

// TestValidateNeverHonorsAllowOverride pins the managed-cloud refusal
// unconditionally: AllowOverride is --allow-non-local-cluster, local's
// kubeconfig-host-pattern escape hatch (a corporate dev cluster on a private
// VPN) — it must never also waive unmanaged's providerID check. The generic
// profile is not available on a cluster whose nodes identify a managed cloud,
// at all: installing it there would silently skip that cloud's storage,
// artifact, and Gateway handling. There is no flag or explicit --cluster-kind
// that gets the generic profile onto a detected managed cloud.
func TestValidateNeverHonorsAllowOverride(t *testing.T) {
	s := unmanaged.Strategy{}

	cases := []struct {
		name          string
		node          *corev1.Node
		allowOverride bool
		wantErr       string // substring; "" = no error
	}{
		{
			name:    "no providerID: nil (nothing to compare against)",
			node:    &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
			wantErr: "",
		},
		{
			name:    "GKE providerID: refused, error names gke",
			node:    &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "gce://proj/us-central1-a/n1"}},
			wantErr: "gke",
		},
		{
			name:          "GKE node + AllowOverride: still refused — the override is local's host heuristic only",
			node:          &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "gce://proj/us-central1-a/n1"}},
			allowOverride: true,
			wantErr:       "gke",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.node)
			err := s.Validate(context.Background(), cloud.ValidateParams{
				Clients:       cloud.Clients{Typed: kc},
				AllowOverride: tc.allowOverride,
			})
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
