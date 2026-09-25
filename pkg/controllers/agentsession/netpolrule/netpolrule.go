// Package netpolrule holds NetworkPolicy rule fragments shared by the per-session
// pod policies (pkg/controllers/agentsession) and the co-sidecar policies
// (pkg/controllers/agentsession/cosidecar). It is the single, CLOUD-AGNOSTIC
// source of truth for the cluster-DNS egress rule, mirrored by the static
// config/networkpolicy/allow-dns.yaml (drift-guarded by a test in
// pkg/platform/manifests).
//
// Cloud-specific DNS endpoints (e.g. GKE Autopilot's NodeLocal DNSCache, which
// answers on a link-local 169.254.x.x address) are deliberately NOT encoded
// here. The cloud provider supplies those CIDRs (see Cloud.DNSEgressCIDRs in
// cmd/oap) and the consumer adds them — keeping this rule portable.
package netpolrule

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// Port builds a NetworkPolicyPort for a protocol + numeric port.
func Port(proto corev1.Protocol, port int32) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: ptr.To(proto), Port: &p}
}

// DNSEgress allows UDP+TCP 53 to the in-cluster DNS in kube-system (CoreDNS/
// kube-dns), relying on the apiserver-stamped kubernetes.io/metadata.name
// namespace label. It is cloud-agnostic: clusters whose DNS resolves via a
// non-kube-system endpoint (e.g. NodeLocal DNSCache on GKE Autopilot) get those
// extra CIDRs from the cloud provider's config, layered on by the consumer.
func DNSEgress() networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
			}},
		},
		Ports: []networkingv1.NetworkPolicyPort{
			Port(corev1.ProtocolUDP, 53),
			Port(corev1.ProtocolTCP, 53),
		},
	}
}

// ControlPlanePeer selects one control-plane workload in the operator
// namespace by its app.kubernetes.io/name label. Shared by the per-session
// policy builders (agentsession) and the SidecarToolbox admission-probe
// policy — the same peer, spelled once.
func ControlPlanePeer(operatorNamespace, appName string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": operatorNamespace},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"app.kubernetes.io/name": appName},
		},
	}
}
