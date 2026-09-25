package manifests_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// natsHostRe pulls the host (service DNS name) out of every nats:// URL in a
// manifest blob: nats://<host>:<port>. Components disagreeing on <host> point
// at different NATS services — the exact config bug we hit, where authzd's
// NATS_URL named a service the rest of the cluster did not use.
var natsHostRe = regexp.MustCompile(`nats://([a-zA-Z0-9.\-]+):\d+`)

// natsHosts returns the sorted, de-duplicated set of NATS hostnames referenced
// by any nats:// URL in the given YAML bytes.
func natsHosts(b []byte) []string {
	seen := map[string]struct{}{}
	for _, m := range natsHostRe.FindAllStringSubmatch(string(b), -1) {
		seen[m[1]] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// TestAuthzdNATSURLMatchesCluster guards the config bug we hit live: authzd's
// NATS_URL named a different service (nats.agentprimitives-system...) than the
// rest of the cluster (spicebox-nats), so authzd silently never received the
// runner's metaagent_request.
//
// The embedded install.yaml is the canonical bundle oap applies; the channelsd
// Deployment (applied from the embedded channelsd manifests) is the other end
// of the same NATS fabric. This test asserts every nats:// URL in either
// manifest resolves to the SAME host. It FAILS if authzd's URL diverges.
func TestAuthzdNATSURLMatchesCluster(t *testing.T) {
	root := repoRoot(t)

	// 1. authzd's NATS_URL comes from the embedded install bundle (canonical).
	installHosts := natsHosts(manifests.Install)
	require.NotEmpty(t, installHosts,
		"install.yaml must reference at least one nats:// URL (authzd's NATS_URL)")

	// 2. channelsd's NATS endpoint is set via a --nats-url flag in its
	// Deployment (the embedded copy oap actually applies). Fold its host in.
	channelsdYAML, err := os.ReadFile(filepath.Join(root, "pkg", "platform", "manifests", "channelsd", "deployment.yaml"))
	require.NoError(t, err, "read embedded channelsd/deployment.yaml")
	channelsdHosts := natsHosts(channelsdYAML)
	require.NotEmpty(t, channelsdHosts,
		"channelsd Deployment must reference a nats:// URL via --nats-url")

	// 3. Every host referenced anywhere must be identical: authzd and channelsd
	// must point at the same NATS service.
	all := map[string]struct{}{}
	for _, h := range installHosts {
		all[h] = struct{}{}
	}
	for _, h := range channelsdHosts {
		all[h] = struct{}{}
	}
	hosts := make([]string, 0, len(all))
	for h := range all {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	require.Len(t, hosts, 1,
		"all components must point at the SAME NATS service; got divergent hosts %v "+
			"(authzd install.yaml hosts=%v, channelsd config hosts=%v)",
		hosts, installHosts, channelsdHosts)
	assert.Contains(t, hosts[0], "spicebox-nats",
		"the shared NATS host must be the cluster's spicebox-nats service, got %q", hosts[0])
}
