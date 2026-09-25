package cloud

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
)

// dnsEgressPodSelector is shared by ApplyDNSEgressPolicy and the bundled static
// config/networkpolicy/allow-dns.yaml (kept in sync by hand): every pod in the
// namespace gets DNS egress EXCEPT extractord, whose whole security story is
// deny-all egress including DNS (config/networkpolicy/extractord.yaml). It
// dials nothing and resolves no names, so a working DNS path out of the one pod
// that parses hostile user-uploaded bytes would be a live exfiltration channel
// (DNS-tunneling extracted text via CoreDNS, which forwards upstream by
// default). A bare podSelector{} would silently reopen exactly that path.
func dnsEgressPodSelector() metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "app.kubernetes.io/name",
				Operator: metav1.LabelSelectorOpNotIn,
				Values:   []string{"agentprimitives-extractord"},
			},
		},
	}
}

// ApplyDNSEgressPolicy creates an additive allow-dns-cloud NetworkPolicy in the
// agentprimitives-system namespace, permitting egress to the given CIDRs on
// UDP+TCP 53. It supplements the bundled cloud-agnostic allow-dns policy (scoped
// to kube-system) for clouds resolving DNS outside kube-system — e.g. GKE
// Autopilot's NodeLocal DNSCache on a link-local 169.254.x.x address. Without
// it, default-deny egress silently drops the lookup and every in-cluster
// service-name connect hangs.
//
// cidrs comes from the cloud Strategy's DNSEgressCIDRs(); nil/empty is a no-op.
func ApplyDNSEgressPolicy(ctx context.Context, cl Clients, cidrs []string) error {
	if len(cidrs) == 0 {
		return nil
	}
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(cidrs))
	for _, cidr := range cidrs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		})
	}
	port53 := func(proto corev1.Protocol) networkingv1.NetworkPolicyPort {
		p := intstr.FromInt32(53)
		return networkingv1.NetworkPolicyPort{Protocol: ptr.To(proto), Port: &p}
	}
	np := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1",
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-dns-cloud",
			Namespace: WebdServiceNamespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: dnsEgressPodSelector(),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: peers,
				Ports: []networkingv1.NetworkPolicyPort{
					port53(corev1.ProtocolUDP),
					port53(corev1.ProtocolTCP),
				},
			}},
		},
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(np)
	if err != nil {
		return fmt.Errorf("convert allow-dns-cloud NetworkPolicy: %w", err)
	}
	// The typed→unstructured converter emits a null creationTimestamp and an
	// empty status that some apiservers reject on apply; drop both.
	unstructured.RemoveNestedField(obj, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(obj, "status")
	if err := applyNetworkPolicy(ctx, cl.Dynamic, &unstructured.Unstructured{Object: obj}); err != nil {
		return fmt.Errorf("apply allow-dns-cloud NetworkPolicy: %w", err)
	}
	return nil
}

// ApplyGatewayBackendIngressPolicy creates an additive allow-webd-gateway
// NetworkPolicy permitting the cloud managed Gateway's load-balancer source
// ranges to reach webd's port 8080. Without it, the namespace-wide
// default-deny ingress drops the L7 health-check probes — which arrive from
// the LB's ranges, NOT from the node — so the load balancer marks the backend
// unhealthy and serves 503 on every request, even though webd's pod is Ready
// (the kubelet readiness probe reaches it from the node, which default-deny
// does not gate). This is invisible on Docker Desktop (its CNI does not enforce
// NetworkPolicy) and fatal on GKE / any enforcing CNI.
//
// cidrs is a caller-supplied CIDR slice — the per-cloud value comes from the
// cloud Strategy's GatewayBackendIngressCIDRs(). A nil/empty slice is a no-op.
// Clouds whose Gateway backend traffic originates from in-cluster Envoy proxy
// pods rather than a fixed range should use a pod/namespace-selector allow
// instead; pass nil/empty here in that case.
func ApplyGatewayBackendIngressPolicy(ctx context.Context, cl Clients, cidrs []string) error {
	if len(cidrs) == 0 {
		return nil
	}
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(cidrs))
	for _, cidr := range cidrs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		})
	}
	port := intstr.FromInt32(WebdServicePort)
	np := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1",
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-webd-gateway",
			Namespace: WebdServiceNamespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app.kubernetes.io/name": WebdServiceName},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: peers,
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: ptr.To(corev1.ProtocolTCP), Port: &port},
				},
			}},
		},
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(np)
	if err != nil {
		return fmt.Errorf("convert allow-webd-gateway NetworkPolicy: %w", err)
	}
	// The typed→unstructured converter emits a null creationTimestamp and an empty
	// status that some apiservers reject on apply; drop both.
	unstructured.RemoveNestedField(obj, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(obj, "status")
	if err := applyNetworkPolicy(ctx, cl.Dynamic, &unstructured.Unstructured{Object: obj}); err != nil {
		return fmt.Errorf("apply allow-webd-gateway NetworkPolicy: %w", err)
	}
	return nil
}

// applyNetworkPolicy applies a NetworkPolicy object via the dynamic client,
// delegating the SSA Patch→Create→Update fallback to applyDoc (the shared
// apply helper in component.go). NetworkPolicy is in resolveDocGVR's table so
// applyDoc resolves the GVR correctly.
func applyNetworkPolicy(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured) error {
	return applyDoc(ctx, dyn, obj, "ap-install")
}
