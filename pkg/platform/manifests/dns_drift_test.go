package manifests_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/netpolrule"
)

// TestAllowDNSYAMLMatchesNetpolRule unifies the third copy of the DNS egress
// rule. netpolrule.DNSEgress() is the canonical, cloud-agnostic source used by
// the per-session and co-sidecar NetworkPolicies; the static
// config/networkpolicy/allow-dns.yaml mirrors it for the namespace-wide policy.
// This test fails if they drift. Cloud-specific DNS (e.g. GKE NodeLocal DNSCache)
// lives in Cloud.DNSEgressCIDRs + the allow-dns-cloud policy oap install creates —
// NOT in either of these, so neither should encode a 169.254.x.x / cloud CIDR.
func TestAllowDNSYAMLMatchesNetpolRule(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "config", "networkpolicy", "allow-dns.yaml"))
	require.NoError(t, err, "read allow-dns.yaml")

	var np networkingv1.NetworkPolicy
	require.NoError(t, yaml.Unmarshal(data, &np), "parse allow-dns.yaml")
	require.Len(t, np.Spec.Egress, 1, "allow-dns must have exactly one egress rule")
	assert.Equal(t, netpolrule.DNSEgress(), np.Spec.Egress[0],
		"config/networkpolicy/allow-dns.yaml egress must equal netpolrule.DNSEgress() (the canonical source)")
}
