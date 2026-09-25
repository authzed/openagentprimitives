package agentsession_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
)

func TestParseNATSPort(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		want    int32
		wantErr bool
	}{
		{name: "default operator URL → 4222", url: "nats://spicebox-nats.agentprimitives-system.svc:4222", want: 4222},
		{name: "nonstandard port is honored", url: "nats://nats.example.com:14222", want: 14222},
		{name: "no port → NATS default 4222", url: "nats://nats.example.com", want: 4222},
		{name: "unparseable URL → error", url: "://bad", wantErr: true},
		{name: "non-numeric port → error", url: "nats://nats.example.com:abc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := agentsession.ParseNATSPort(tc.url)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseSpiceDBEndpoint(t *testing.T) {
	cases := []struct {
		name          string
		endpoint      string
		wantPort      int32
		wantInCluster bool
		wantErr       bool
	}{
		{name: "in-cluster FQDN service → in-cluster, port honored", endpoint: "spicebox-spicedb.agentprimitives-system.svc:50051", wantPort: 50051, wantInCluster: true},
		{name: "in-cluster short name → in-cluster", endpoint: "spicebox-spicedb:50051", wantPort: 50051, wantInCluster: true},
		{name: "in-cluster cluster.local FQDN → in-cluster", endpoint: "spicebox-spicedb.agentprimitives-system.svc.cluster.local:50051", wantPort: 50051, wantInCluster: true},
		{name: "external endpoint → not in-cluster", endpoint: "grpc.example.com:443", wantPort: 443, wantInCluster: false},
		{name: "no port → SpiceDB default 50051", endpoint: "spicedb.example.com", wantPort: 50051, wantInCluster: false},
		{name: "scheme prefix is stripped", endpoint: "grpc://spicebox-spicedb.agentprimitives-system.svc:50051", wantPort: 50051, wantInCluster: true},
		{name: "empty endpoint → error", endpoint: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, inCluster, err := agentsession.ParseSpiceDBEndpoint(tc.endpoint)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPort, port)
			assert.Equal(t, tc.wantInCluster, inCluster)
		})
	}
}

func testNetpolConfig() agentsession.NetpolConfig {
	return agentsession.NetpolConfig{
		Enabled:           true,
		OperatorNamespace: "agentprimitives-system",
		NATSPort:          4222,
		SpiceDBPort:       50051,
		SpiceDBInCluster:  true,
	}
}

func netpolSession(name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: "uid-1"},
	}
}

// policyPorts flattens NetworkPolicy ports as "PROTO/port" strings.
func policyPorts(ports []networkingv1.NetworkPolicyPort) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, fmt.Sprintf("%s/%d", *p.Protocol, p.Port.IntVal))
	}
	return out
}

// egressPorts flattens one egress rule's ports as "PROTO/port" strings.
func egressPorts(rule networkingv1.NetworkPolicyEgressRule) []string {
	return policyPorts(rule.Ports)
}

func TestBuildRunnerNetworkPolicy(t *testing.T) {
	sess := netpolSession("s1")
	np := agentsession.BuildRunnerNetworkPolicy(sess, testNetpolConfig())

	assert.Equal(t, "s1-runner-netpol", np.Name)
	assert.Equal(t, "default", np.Namespace)
	require.Len(t, np.OwnerReferences, 1)
	assert.Equal(t, "AgentSession", np.OwnerReferences[0].Kind)
	assert.Equal(t, "s1", np.OwnerReferences[0].Name)

	// Selects exactly this session's runner pod (not sidecar pods, which
	// carry the same agentsession label plus sidecartoolbox).
	assert.Equal(t, map[string]string{"agentprimitives.authzed.com/agentsession": "s1"},
		np.Spec.PodSelector.MatchLabels)
	require.Len(t, np.Spec.PodSelector.MatchExpressions, 1)
	assert.Equal(t, "agentprimitives.authzed.com/sidecartoolbox", np.Spec.PodSelector.MatchExpressions[0].Key)
	assert.Equal(t, metav1.LabelSelectorOpDoesNotExist, np.Spec.PodSelector.MatchExpressions[0].Operator)
	// Both directions isolated; no ingress rules → all ingress denied.
	assert.ElementsMatch(t,
		[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		np.Spec.PolicyTypes)
	assert.Empty(t, np.Spec.Ingress)

	// DNS, NATS, operator, SpiceDB (in-cluster), sidecars, external.
	require.Len(t, np.Spec.Egress, 6)
	assert.ElementsMatch(t, []string{"UDP/53", "TCP/53"}, egressPorts(np.Spec.Egress[0]), "DNS rule")
	assert.ElementsMatch(t, []string{"TCP/4222"}, egressPorts(np.Spec.Egress[1]), "NATS rule")
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "spicebox-nats"},
		np.Spec.Egress[1].To[0].PodSelector.MatchLabels)
	assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": "agentprimitives-system"},
		np.Spec.Egress[1].To[0].NamespaceSelector.MatchLabels)
	assert.ElementsMatch(t, []string{"TCP/8082", "TCP/8443"}, egressPorts(np.Spec.Egress[2]), "operator rule")
	assert.ElementsMatch(t, []string{"TCP/50051"}, egressPorts(np.Spec.Egress[3]), "SpiceDB rule")

	// Sidecar rule: this session's separate-pod sidecars, any port.
	sidecarPeer := np.Spec.Egress[4].To[0]
	assert.Nil(t, sidecarPeer.NamespaceSelector, "sidecar peer is same-namespace")
	assert.Equal(t, map[string]string{"agentprimitives.authzed.com/agentsession": "s1"},
		sidecarPeer.PodSelector.MatchLabels)
	require.Len(t, sidecarPeer.PodSelector.MatchExpressions, 1)
	assert.Equal(t, metav1.LabelSelectorOpExists, sidecarPeer.PodSelector.MatchExpressions[0].Operator)
	assert.Empty(t, np.Spec.Egress[4].Ports, "sidecar ports are dynamic — all ports allowed to those pods")

	assert.ElementsMatch(t, []string{"TCP/443", "TCP/6443"}, egressPorts(np.Spec.Egress[5]), "external rule")
	require.NotNil(t, np.Spec.Egress[5].To[0].IPBlock)
	assert.Equal(t, "0.0.0.0/0", np.Spec.Egress[5].To[0].IPBlock.CIDR)
}

func TestBuildRunnerNetworkPolicy_ExternalSpiceDB_NoPeerRule(t *testing.T) {
	cfg := testNetpolConfig()
	cfg.SpiceDBInCluster = false
	np := agentsession.BuildRunnerNetworkPolicy(netpolSession("s1"), cfg)
	// DNS, NATS, operator, sidecars, external — no SpiceDB peer rule.
	require.Len(t, np.Spec.Egress, 5)
	for i, rule := range np.Spec.Egress {
		assert.NotContains(t, egressPorts(rule), "TCP/50051", "egress rule %d", i)
	}
}

func TestBuildRunnerNetworkPolicy_SpiceDBPeerUsesOperatorLabel(t *testing.T) {
	np := agentsession.BuildRunnerNetworkPolicy(netpolSession("s1"), testNetpolConfig())

	var found bool
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["authzed.com/cluster-component"] == "spicedb" {
				found = true
			}
		}
	}
	assert.True(t, found, "runner egress must target SpiceDB by authzed.com/cluster-component=spicedb")
}

func TestBuildSandboxNetworkPolicy(t *testing.T) {
	cases := []struct {
		name       string
		mode       spiceboxv1alpha1.NetworkMode
		wantEgress int // 0 = full deny; 2 = DNS + external 443/80
	}{
		{name: "mode none → full isolation, zero egress rules", mode: spiceboxv1alpha1.NetworkModeNone, wantEgress: 0},
		{name: "empty mode → treated as none (fail-closed)", mode: "", wantEgress: 0},
		{name: "mode allowlist → DNS + coarse 443/80 egress", mode: spiceboxv1alpha1.NetworkModeAllowlist, wantEgress: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := netpolSession("s1")
			np := agentsession.BuildSandboxNetworkPolicy(sess, "s1-code", tc.mode)

			assert.Equal(t, "s1-code-sandbox-netpol", np.Name)
			assert.Equal(t, map[string]string{"agentprimitives.authzed.com/session": "s1-code"},
				np.Spec.PodSelector.MatchLabels)
			assert.ElementsMatch(t,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				np.Spec.PolicyTypes)
			assert.Empty(t, np.Spec.Ingress)
			require.Len(t, np.Spec.Egress, tc.wantEgress)
			if tc.wantEgress == 2 {
				assert.ElementsMatch(t, []string{"UDP/53", "TCP/53"}, egressPorts(np.Spec.Egress[0]))
				assert.ElementsMatch(t, []string{"TCP/443", "TCP/80"}, egressPorts(np.Spec.Egress[1]))
				require.NotNil(t, np.Spec.Egress[1].To[0].IPBlock)
				assert.Equal(t, "0.0.0.0/0", np.Spec.Egress[1].To[0].IPBlock.CIDR)
			}
			require.Len(t, np.OwnerReferences, 1)
			assert.Equal(t, "s1", np.OwnerReferences[0].Name, "owned by the AgentSession")
		})
	}
}

func TestSidecarNetworkPolicyName(t *testing.T) {
	assert.Equal(t, "s1-sidecar-k8s-proxy-netpol",
		agentsession.SidecarNetworkPolicyName(netpolSession("s1"), "k8s-proxy"))
}

func TestBuildSidecarNetworkPolicy(t *testing.T) {
	cases := []struct {
		name       string
		mode       spiceboxv1alpha1.NetworkMode
		wantEgress int // 0 = full deny; 2 = DNS + external 443/80
	}{
		{name: "effective mode none → full egress deny", mode: spiceboxv1alpha1.NetworkModeNone, wantEgress: 0},
		{name: "empty mode → treated as none (fail-closed)", mode: "", wantEgress: 0},
		{name: "effective mode allowlist → DNS + coarse 443/80 egress", mode: spiceboxv1alpha1.NetworkModeAllowlist, wantEgress: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := netpolSession("s1")
			rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
				Ref:                  "k8s-proxy",
				Port:                 18080,
				EffectiveNetworkMode: tc.mode,
			}
			np := agentsession.BuildSidecarNetworkPolicy(sess, rt, testNetpolConfig(), false)

			assert.Equal(t, "s1-sidecar-k8s-proxy-netpol", np.Name)
			assert.Equal(t, "default", np.Namespace)
			require.Len(t, np.OwnerReferences, 1)
			assert.Equal(t, "AgentSession", np.OwnerReferences[0].Kind)
			assert.Equal(t, "s1", np.OwnerReferences[0].Name)

			// Selects exactly this toolbox's sidecar pod.
			assert.Equal(t, map[string]string{
				"agentprimitives.authzed.com/agentsession":   "s1",
				"agentprimitives.authzed.com/sidecartoolbox": "k8s-proxy",
			}, np.Spec.PodSelector.MatchLabels)
			assert.ElementsMatch(t,
				[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				np.Spec.PolicyTypes)

			// Ingress: ONLY this session's runner pod, on exactly the
			// sidecar's MCP port. The runner is the same-namespace pod with
			// the agentsession label and NO sidecartoolbox label.
			require.Len(t, np.Spec.Ingress, 1)
			require.Len(t, np.Spec.Ingress[0].From, 1)
			runnerPeer := np.Spec.Ingress[0].From[0]
			assert.Nil(t, runnerPeer.NamespaceSelector, "runner peer is same-namespace")
			require.NotNil(t, runnerPeer.PodSelector)
			assert.Equal(t, map[string]string{"agentprimitives.authzed.com/agentsession": "s1"},
				runnerPeer.PodSelector.MatchLabels)
			require.Len(t, runnerPeer.PodSelector.MatchExpressions, 1)
			assert.Equal(t, "agentprimitives.authzed.com/sidecartoolbox",
				runnerPeer.PodSelector.MatchExpressions[0].Key)
			assert.Equal(t, metav1.LabelSelectorOpDoesNotExist,
				runnerPeer.PodSelector.MatchExpressions[0].Operator)
			require.Len(t, np.Spec.Ingress[0].Ports, 1)
			assert.ElementsMatch(t, []string{"TCP/18080"}, policyPorts(np.Spec.Ingress[0].Ports))

			require.Len(t, np.Spec.Egress, tc.wantEgress)
			if tc.wantEgress == 2 {
				assert.ElementsMatch(t, []string{"UDP/53", "TCP/53"}, egressPorts(np.Spec.Egress[0]), "DNS rule")
				assert.ElementsMatch(t, []string{"TCP/443", "TCP/80"}, egressPorts(np.Spec.Egress[1]), "external rule")
				require.NotNil(t, np.Spec.Egress[1].To[0].IPBlock)
				assert.Equal(t, "0.0.0.0/0", np.Spec.Egress[1].To[0].IPBlock.CIDR)
			}
		})
	}
}

// TestBuildSidecarNetworkPolicy_WorkshopGatesOperatorAndAPIServerEgress pins
// the plan-3b workshop-sidecar addition: workshop=true adds the operator
// peer (8082+8443) and external apiserver (6443) egress UNCONDITIONALLY —
// on top of whatever EffectiveNetworkMode already grants — and workshop=false
// (every non-workshop separate-pod sidecar) is byte-identical to before this
// parameter existed. Both directions asserted so a regression in either can
// only pass by accident.
func TestBuildSidecarNetworkPolicy_WorkshopGatesOperatorAndAPIServerEgress(t *testing.T) {
	sess := netpolSession("s1")
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Ref:                  "workshop",
		Port:                 18080,
		EffectiveNetworkMode: spiceboxv1alpha1.NetworkModeAllowlist,
	}
	cfg := testNetpolConfig()

	t.Run("workshop=true: operator 8082+8443, external 6443, in addition to DNS+443", func(t *testing.T) {
		np := agentsession.BuildSidecarNetworkPolicy(sess, rt, cfg, true)

		require.Len(t, np.Spec.Egress, 4, "DNS, external 443/80, operator, external 6443")
		assert.ElementsMatch(t, []string{"UDP/53", "TCP/53"}, egressPorts(np.Spec.Egress[0]), "DNS rule unchanged")
		assert.ElementsMatch(t, []string{"TCP/443", "TCP/80"}, egressPorts(np.Spec.Egress[1]), "allowlist 443/80 rule unchanged")

		var foundOperator, foundAPIServer bool
		for _, rule := range np.Spec.Egress {
			ports := egressPorts(rule)
			isOperatorPorts := len(ports) == 2 && (ports[0] == "TCP/8082" || ports[1] == "TCP/8082")
			isAPIServerPorts := len(ports) == 1 && ports[0] == "TCP/6443"
			switch {
			case isOperatorPorts:
				foundOperator = true
				assert.ElementsMatch(t, []string{"TCP/8082", "TCP/8443"}, ports, "operator rule ports")
				require.Len(t, rule.To, 1)
				assert.Equal(t, map[string]string{"app.kubernetes.io/name": "spicebox-operator"},
					rule.To[0].PodSelector.MatchLabels)
				assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": cfg.OperatorNamespace},
					rule.To[0].NamespaceSelector.MatchLabels)
			case isAPIServerPorts:
				foundAPIServer = true
				require.Len(t, rule.To, 1)
				require.NotNil(t, rule.To[0].IPBlock)
				assert.Equal(t, "0.0.0.0/0", rule.To[0].IPBlock.CIDR)
			}
		}
		assert.True(t, foundOperator, "expected an operator peer rule on TCP 8082+8443, got %+v", np.Spec.Egress)
		assert.True(t, foundAPIServer, "expected an external TCP 6443 rule, got %+v", np.Spec.Egress)
	})

	t.Run("workshop=false: no operator or apiserver egress (unchanged from before the workshop seam)", func(t *testing.T) {
		np := agentsession.BuildSidecarNetworkPolicy(sess, rt, cfg, false)

		require.Len(t, np.Spec.Egress, 2, "DNS + external 443/80 only")
		for i, rule := range np.Spec.Egress {
			ports := egressPorts(rule)
			assert.NotContains(t, ports, "TCP/8082", "egress rule %d must not grant operator 8082", i)
			assert.NotContains(t, ports, "TCP/8443", "egress rule %d must not grant operator 8443", i)
			assert.NotContains(t, ports, "TCP/6443", "egress rule %d must not grant apiserver 6443", i)
		}
	})
}

// TestEnsureDetectorPod_PolicyIsStampedBeforeThePod pins the ordering rule the
// runner, sandbox and sidecar paths all follow: the NetworkPolicy lands before
// the pod it constrains, so an enforcing CNI never sees an unconstrained pod.
//
// It matters most here. The detector runs a USER-SUPPLIED image and its entire
// security property is Egress:Denied — it ingests potentially hostile tool text
// and must not be able to exfiltrate it. With the pod created first, a failing
// policy write returns the reconcile with the detector already scheduled under
// no policy at all.
func TestEnsureDetectorPod_PolicyIsStampedBeforeThePod(t *testing.T) {
	var order []string
	record := func(obj client.Object) string {
		switch obj.(type) {
		case *networkingv1.NetworkPolicy:
			return "networkpolicy"
		case *corev1.Pod:
			return "pod"
		default:
			return fmt.Sprintf("%T", obj)
		}
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				order = append(order, "create "+record(obj))
				return nil
			},
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				order = append(order, "apply "+record(obj))
				return nil
			},
		}).Build()

	r := &agentsession.Reconciler{Client: c}
	sess := netpolSession("s1")
	spec := cosidecar.Spec{
		Ref:   "prompt-injection",
		Name:  agentsession.SidecarNetworkPolicyName(sess, "prompt-injection"),
		Image: "example.test/detector:v1",
		Port:  9080,
		// The property the ordering protects.
		Egress: cosidecar.EgressPolicy{Denied: true},
	}
	pod, _, err := cosidecar.BuildPod(sess, spec)
	require.NoError(t, err, "build detector pod")

	require.NoError(t, r.EnsureDetectorPodForTest(context.Background(), sess, spec, pod))

	require.Len(t, order, 2, "one policy write and one pod write")
	assert.Equal(t, "networkpolicy", strings.SplitN(order[0], " ", 2)[1],
		"the deny-all-egress policy must be written BEFORE the detector pod exists")
	assert.Equal(t, "create pod", order[1])
	assert.Equal(t, "apply networkpolicy", order[0],
		"server-side apply, like every ensure*NetworkPolicy sibling — Create-and-ignore-AlreadyExists never corrects a drifted policy")
}
