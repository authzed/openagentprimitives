package installcmd

import (
	"bytes"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/gatewayhealth"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// cloudNetworkResourceFile is the overlay file holding the cloud-specific
// network objects `oap install` layers on top of the cloud-agnostic base.
const cloudNetworkResourceFile = "cloud-network.yaml"

// renderCloudNetworkResources builds the objects `oap install` applies per cloud
// on top of the bundled default-deny, from the SAME definitions install uses:
//
//   - cloud.DNSEgressPolicy(Strat.DNSEgressCIDRs()) — DNS outside kube-system
//     (e.g. a node-local DNS cache on a link-local address), always;
//   - cloud.GatewayBackendIngressPolicy(Strat.GatewayBackendIngressCIDRs()) and
//     gatewayhealth.For(Strat.Key()).Manifests() — the managed load balancer's
//     health-check reachability and probe path, only when routing is emitted
//     (they exist to serve the emitted Gateway).
//
// Every per-cloud answer comes from the Strategy or the gatewayhealth registry,
// so this consumer carries no cluster-kind branch. emitted is false when the
// cloud needs none of them; renderKustomization lists the file under the same
// result, so the writer and the kustomization cannot disagree. Pure function of
// cfg.
func renderCloudNetworkResources(cfg RenderConfig) (out []byte, emitted bool, err error) {
	var objs []any
	if np := cloud.DNSEgressPolicy(cfg.Strat.DNSEgressCIDRs()); np != nil {
		objs = append(objs, np)
	}
	if cfg.Routing.trustedHostname != "" {
		if np := cloud.GatewayBackendIngressPolicy(cfg.Strat.GatewayBackendIngressCIDRs()); np != nil {
			objs = append(objs, np)
		}
		for _, u := range gatewayhealth.For(cfg.Strat.Key()).Manifests() {
			objs = append(objs, u.Object)
		}
	}
	if len(objs) == 0 {
		return nil, false, nil
	}
	docs := make([][]byte, 0, len(objs))
	for _, o := range objs {
		b, err := marshalClean(o)
		if err != nil {
			return nil, false, err
		}
		docs = append(docs, b)
	}
	return bytes.Join(docs, []byte("---\n")), true, nil
}
