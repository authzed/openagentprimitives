package manifests_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func loadNetpol(t *testing.T, root, name string) networkingv1.NetworkPolicy {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "config", "networkpolicy", name+".yaml"))
	require.NoErrorf(t, err, "read %s.yaml", name)
	var np networkingv1.NetworkPolicy
	require.NoErrorf(t, yaml.Unmarshal(data, &np), "parse %s.yaml", name)
	return np
}

// egressExternalMembers is the set of component names egress-external selects.
func egressExternalMembers(t *testing.T, root string) map[string]bool {
	np := loadNetpol(t, root, "egress-external")
	out := map[string]bool{}
	for _, expr := range np.Spec.PodSelector.MatchExpressions {
		if expr.Key == "app.kubernetes.io/name" {
			for _, v := range expr.Values {
				out[v] = true
			}
		}
	}
	return out
}

// ingressAllowlist is the set of component names the target NP allows ingress from
// (by app.kubernetes.io/name; non-name peers like agentsession pods are ignored).
func ingressAllowlist(t *testing.T, root, target string) map[string]bool {
	np := loadNetpol(t, root, target)
	out := map[string]bool{}
	for _, rule := range np.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.PodSelector != nil {
				if name := peer.PodSelector.MatchLabels["app.kubernetes.io/name"]; name != "" {
					out[name] = true
				}
			}
		}
	}
	return out
}

// TestNetworkPolicyAllowlistsCoverControlPlane asserts every control-plane
// component that dials an in-cluster service is granted that flow on BOTH sides:
// egress-external (egress) AND the target's ingress policy. NetworkPolicy is
// two-sided + additive, so a gap on either side silently blocks the flow. And
// because Docker Desktop's CNI does not enforce NetworkPolicy, such a gap is
// invisible locally and only bites on an enforcing cluster (GKE Autopilot/Cilium)
// — this guards the regression that took authzd + webd down on the first real GKE
// deploy. The required map is derived from each component's deployment env
// (NATS_URL->nats, SPICEDB_ENDPOINT->spicedb, (OPERATOR_)MEMORY_URL->operator).
func TestNetworkPolicyAllowlistsCoverControlPlane(t *testing.T) {
	required := map[string][]string{
		"spicebox-channelsd":     {"nats", "spicedb", "operator"},
		"agentprimitives-authzd": {"nats", "spicedb", "operator"},
		"spicebox-webd":          {"nats", "spicedb", "operator"},
		// The operator is extractord's sole caller (config/networkpolicy/extractord.yaml
		// allows ingress from spicebox-operator only); this closes the loop on the other
		// side, so a future one-sided edit to either file fails here instead of only
		// showing up as a live hang once the caller is wired up.
		// The operator dials webd too, since the public tunnel moved into it
		// (pkg/controllers/publicendpoint). TestNetworkPolicyAllowsTheOperatorToReachWebd
		// below pins the ports; this keeps the who-dials-whom table complete.
		"spicebox-operator": {"extractord", "webd"},
	}
	root := repoRoot(t)
	egress := egressExternalMembers(t, root)
	ingress := map[string]map[string]bool{
		"nats":       ingressAllowlist(t, root, "nats"),
		"spicedb":    ingressAllowlist(t, root, "spicedb"),
		"operator":   ingressAllowlist(t, root, "operator"),
		"extractord": ingressAllowlist(t, root, "extractord"),
		"webd":       ingressAllowlist(t, root, "webd"),
	}
	for comp, targets := range required {
		assert.Truef(t, egress[comp], "egress-external must select %s (egress side)", comp)
		for _, tgt := range targets {
			assert.Truef(t, ingress[tgt][comp], "%s ingress must allow %s (ingress side)", tgt, comp)
		}
	}
}

// ingressAllowsPort reports whether any ingress rule allows the given TCP port.
func ingressAllowsPort(np networkingv1.NetworkPolicy, port int32) bool {
	for _, rule := range np.Spec.Ingress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == port {
				return true
			}
		}
	}
	return false
}

// ingressFromNamespace reports whether any ingress rule allows traffic from the
// given namespace (matched by the kubernetes.io/metadata.name label).
func ingressFromNamespace(np networkingv1.NetworkPolicy, ns string) bool {
	for _, rule := range np.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == ns {
				return true
			}
		}
	}
	return false
}

// TestNetworkPolicyAllowlistsCoverGatewayAndWebhook guards the three default-deny
// gaps the networking audit found after the webd 503. Each is a flow the
// apiserver or the Gateway data path needs that default-deny silently drops —
// invisible on Docker Desktop (no NP enforcement), fatal on an enforcing CNI. The
// originating control-plane peer (apiserver, Envoy proxy) is not a selectable
// in-namespace pod, so the existing control-plane-mesh test cannot catch these.
func TestNetworkPolicyAllowlistsCoverGatewayAndWebhook(t *testing.T) {
	root := repoRoot(t)

	// 1. Operator admission webhook (:9443) must be reachable by the apiserver, or
	//    the toolcall webhook (failurePolicy: Fail) rejects every ToolCall write
	//    and the Ignore webhooks silently stop validating.
	op := loadNetpol(t, root, "operator")
	assert.Truef(t, ingressAllowsPort(op, 9443),
		"operator NP must allow ingress on :9443 (admission webhook) — else the apiserver can't reach it and ToolCall writes are rejected")

	// 2. webd must accept ingress from the Envoy proxy (the non-GKE Gateway path),
	//    or it returns 503 through the Gateway on every enforcing-CNI cluster
	//    running Envoy Gateway.
	we := loadNetpol(t, root, "webd-envoy")
	assert.Equalf(t, "spicebox-webd", we.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"],
		"allow-webd-envoy must target webd")
	assert.Truef(t, ingressFromNamespace(we, "envoy-gateway-system"),
		"allow-webd-envoy must allow ingress from envoy-gateway-system")
	assert.Truef(t, ingressAllowsPort(we, 8080), "allow-webd-envoy must allow webd's port 8080")

	// 3. The cert-manager HTTP-01 solver must be reachable through the Gateway, or
	//    the ACME challenge fails and the real Let's Encrypt cert never issues.
	as := loadNetpol(t, root, "acme-solver")
	assert.Equalf(t, "true", as.Spec.PodSelector.MatchLabels["acme.cert-manager.io/http01-solver"],
		"allow-acme-solver must target the cert-manager solver pod")
	assert.Truef(t, ingressFromNamespace(as, "envoy-gateway-system"),
		"allow-acme-solver must allow ingress from envoy-gateway-system")
	assert.Truef(t, ingressAllowsPort(as, 8089), "allow-acme-solver must allow the solver port 8089")
}

// TestSpiceDBNetpolTargetsOperatorLabels asserts the spicedb ingress policy and
// the egress-external destination select the operator-managed pods (the
// app.kubernetes.io/name=spicebox-spicedb label no longer exists on them).
func TestSpiceDBNetpolTargetsOperatorLabels(t *testing.T) {
	root := repoRoot(t)

	sp := loadNetpol(t, root, "spicedb")
	assert.Equal(t, "spicedb", sp.Spec.PodSelector.MatchLabels["authzed.com/cluster-component"],
		"spicedb NP must select operator-managed SpiceDB pods")

	// Postgres ingress must admit everything under the spicedb-operator's
	// ownership (serving pods + migration Job + our createdb Job), which the
	// operator/we stamp with authzed.com/cluster=spicebox-spicedb — not the
	// narrower authzed.com/cluster-component=spicedb label, which the
	// migration/createdb Jobs do NOT carry.
	pg := loadNetpol(t, root, "postgres")
	var fromSpiceDB bool
	for _, rule := range pg.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["authzed.com/cluster"] == "spicebox-spicedb" {
				fromSpiceDB = true
			}
		}
	}
	assert.True(t, fromSpiceDB, "postgres NP must allow ingress from the spicedb-operator's owned pods (serving + migration + createdb)")

	// SpiceDB egress policy's podSelector must also cover the owner label, so
	// the migration/createdb Jobs (not just serving pods) can reach Postgres.
	eg := loadNetpol(t, root, "spicedb-egress")
	assert.Equal(t, "spicebox-spicedb", eg.Spec.PodSelector.MatchLabels["authzed.com/cluster"])
	var toPG bool
	for _, rule := range eg.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == 5432 {
				toPG = true
			}
		}
	}
	assert.True(t, toPG, "spicedb-egress must allow egress to Postgres 5432")
}

// egressExternalAllowsTo reports whether egress-external permits a flow to pods
// labelled app.kubernetes.io/name=<name> on the given TCP port. Peer AND port,
// because a rule that names the right destination on the wrong port drops the
// flow exactly as silently as no rule at all.
func egressExternalAllowsTo(t *testing.T, root, name string, port int32) bool {
	t.Helper()
	np := loadNetpol(t, root, "egress-external")
	for _, rule := range np.Spec.Egress {
		var toName bool
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app.kubernetes.io/name"] == name {
				toName = true
			}
		}
		if !toName {
			continue
		}
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == port {
				return true
			}
		}
	}
	return false
}

// TestNetworkPolicyAllowsTheOperatorToReachWebd guards the tunnel's DATA PATH.
//
// pkg/controllers/publicendpoint runs the tunnel inside the operator process
// and forwards it at spicebox-webd.<ns>.svc.cluster.local:8080 — a pod-to-pod
// dial. What it replaced was an `oap init --local` port-forward, whose traffic
// reached webd from the kubelet and was therefore exempt from NetworkPolicy;
// moving the tunnel moved it out of that exemption, and nothing carried an
// allow rule over with it.
//
// The failure this guards is silent AND complete: the ngrok agent's own egress
// is on 443 (already allowed), so the tunnel opens, status.url goes Ready, and
// the external-URL ConfigMap advertises a public address — while every request
// arriving through it is dropped one hop later at webd. It is also invisible in
// the mode most people test: Docker Desktop and kind do not enforce
// NetworkPolicy, while `ap desktop`'s k3s does.
//
// Both sides are asserted with their port, since NetworkPolicy is two-sided and
// additive: either half alone permits nothing.
func TestNetworkPolicyAllowsTheOperatorToReachWebd(t *testing.T) {
	root := repoRoot(t)

	assert.True(t, egressExternalAllowsTo(t, root, "spicebox-webd", 8080),
		"egress-external must allow the operator to dial webd on 8080 (egress side) — "+
			"else the tunnel opens, reports Ready, and every request through it is dropped")

	wd := loadNetpol(t, root, "webd")
	assert.Equal(t, "spicebox-webd", wd.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"],
		"the webd policy must target webd")
	assert.True(t, ingressAllowlist(t, root, "webd")["spicebox-operator"],
		"webd ingress must allow spicebox-operator (ingress side)")
	assert.True(t, ingressAllowsPort(wd, 8080),
		"webd ingress must name port 8080 — the port upstreamAddr dials")
}

// egressExternalAllowsProbePods reports whether egress-external permits the
// operator to dial pods carrying the SidecarToolbox reachability-probe label
// (any value, since it is keyed by CR name) on the given TCP port, across
// namespaces. The probe label is matched by KEY via an Exists expression: a
// SidecarToolbox may live in any namespace, so the rule cannot pin a value.
func egressExternalAllowsProbePods(t *testing.T, root string, port int32) bool {
	t.Helper()
	const probeLabel = "agentprimitives.authzed.com/sidecartoolbox-probe"
	np := loadNetpol(t, root, "egress-external")
	for _, rule := range np.Spec.Egress {
		toProbe := false
		for _, peer := range rule.To {
			if peer.PodSelector == nil {
				continue
			}
			if _, ok := peer.PodSelector.MatchLabels[probeLabel]; ok {
				toProbe = true
			}
			for _, expr := range peer.PodSelector.MatchExpressions {
				if expr.Key == probeLabel && expr.Operator == metav1.LabelSelectorOpExists {
					toProbe = true
				}
			}
		}
		if !toProbe {
			continue
		}
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == port {
				return true
			}
		}
	}
	return false
}

// TestNetworkPolicyAllowsOperatorToReachSidecarProbe guards the SidecarToolbox
// reachability probe's DATA PATH. The operator creates a short-lived probe Pod
// in the SidecarToolbox's OWN namespace — any namespace, not just
// agentprimitives-system — and dials its MCP endpoint on :8080
// (pkg/controllers/sidecartoolbox/probe.go) to confirm reachability and
// discover the live tool list. egress-external is the operator's egress
// allowlist; without a rule for the probe Pod the dial is silently dropped on
// an enforcing CNI (k3s/Cilium/Calico) and EVERY SidecarToolbox lands
// Reachable=False/ProbeFailed with "MCP endpoint did not become reachable in
// time". Like every other flow in this file, it is invisible on Docker Desktop,
// which does not enforce NetworkPolicy, and only bites on an enforcing cluster.
func TestNetworkPolicyAllowsOperatorToReachSidecarProbe(t *testing.T) {
	root := repoRoot(t)
	assert.True(t, egressExternalAllowsProbePods(t, root, 8080),
		"egress-external must allow the operator to dial SidecarToolbox probe pods "+
			"(label agentprimitives.authzed.com/sidecartoolbox-probe) on :8080 across all namespaces — "+
			"else admission-time reachability probing is dropped on an enforcing CNI and every SidecarToolbox fails")
}
