package workshopprobe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestBuildProbePod_IsHardened(t *testing.T) {
	cases := []struct {
		name string
		spec v1alpha1.WorkshopProbeSpec
	}{
		{
			name: "image spec",
			spec: v1alpha1.WorkshopProbeSpec{Image: "ghcr.io/demo/mcp:v1", TimeoutSeconds: 90},
		},
		{
			name: "script spec",
			spec: v1alpha1.WorkshopProbeSpec{
				Script:         &v1alpha1.WorkshopProbeScript{BaseImage: "ghcr.io/demo/base:v1", Script: "#!/bin/sh\necho hi\n"},
				TimeoutSeconds: 90,
			},
		},
		{
			name: "cliHelp spec",
			spec: v1alpha1.WorkshopProbeSpec{
				CliHelp:        &v1alpha1.WorkshopProbeCliHelp{Image: "ghcr.io/demo/cli:v1", Binary: "democli", Args: []string{"--help"}},
				TimeoutSeconds: 90,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+": hardened, timeout-bounded, owner-refed pod regardless of discriminator", func(t *testing.T) {
			wp := &v1alpha1.WorkshopProbe{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ws-abc123def456", UID: "u1"},
				Spec:       tc.spec,
			}
			pod, err := BuildProbePod(wp, "spicebox-system")
			require.NoError(t, err)
			assert.Equal(t, ptr.To(false), pod.Spec.AutomountServiceAccountToken)
			assert.Equal(t, ptr.To(true), pod.Spec.SecurityContext.RunAsNonRoot)
			assert.Equal(t, int64(90), *pod.Spec.ActiveDeadlineSeconds)
			assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
			assert.Empty(t, pod.Spec.ImagePullSecrets, "no pull secret: a private image must fail to pull")
			require.NotEmpty(t, pod.Spec.Containers)
			c := pod.Spec.Containers[0]
			assert.Equal(t, ptr.To(false), c.SecurityContext.AllowPrivilegeEscalation)
			assert.Equal(t, ptr.To(true), c.SecurityContext.ReadOnlyRootFilesystem)
			assert.Contains(t, c.SecurityContext.Capabilities.Drop, corev1.Capability("ALL"))
			require.NotNil(t, c.Resources.Limits, "resource-limited")
			// owner-refed to the probe so GC reaps it with the CR / namespace
			require.Len(t, pod.OwnerReferences, 1)
			assert.Equal(t, "WorkshopProbe", pod.OwnerReferences[0].Kind)
			assert.Equal(t, ptr.To(true), pod.OwnerReferences[0].Controller)
		})
	}
}

func TestBuildProbeNetworkPolicy_OperatorIngressOnly_DNSand443Egress(t *testing.T) {
	wp := &v1alpha1.WorkshopProbe{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ws-x", UID: "u1"}}
	np := BuildProbeNetworkPolicy(wp, "spicebox-system")
	assert.ElementsMatch(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	require.Len(t, np.Spec.Ingress, 1, "exactly one ingress rule: from the operator on the MCP port")

	ingress := np.Spec.Ingress[0]
	require.Len(t, ingress.From, 1, "ingress admits exactly one peer: the operator")
	peer := ingress.From[0]
	require.NotNil(t, peer.NamespaceSelector, "peer is namespace-scoped to the operator namespace")
	assert.Equal(t, "spicebox-system", peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	require.NotNil(t, peer.PodSelector, "peer is pod-scoped to the operator workload")
	assert.Equal(t, "spicebox-operator", peer.PodSelector.MatchLabels["app.kubernetes.io/name"])
	require.Len(t, ingress.Ports, 1)
	require.NotNil(t, ingress.Ports[0].Port)
	assert.Equal(t, 8080, ingress.Ports[0].Port.IntValue(), "ingress is scoped to the MCP port, not all ports")

	// egress carries DNS + external 443, never port 80, never the API server (the probe pod needs neither)
	var has443, has80 bool
	for _, e := range np.Spec.Egress {
		assert.NotEmpty(t, e.Ports, "every egress rule names explicit ports: an empty Ports slice means ALL ports, a silent hole")
		for _, p := range e.Ports {
			if p.Port.IntValue() == 443 {
				has443 = true
			}
			if p.Port.IntValue() == 80 {
				has80 = true
			}
		}
	}
	assert.True(t, has443)
	assert.False(t, has80, "probe egress is 443-only, not 80")
}

func TestValidateProbeImage(t *testing.T) {
	cases := []struct {
		name, ref, trusted string
		wantErr            bool
	}{
		{"public ghcr ok", "ghcr.io/demo/mcp:v1", "registry.internal.example/ap", false},
		{"docker hub ok", "demo/mcp:v1", "registry.internal.example/ap", false},
		{"empty refused", "", "registry.internal.example/ap", true},
		{"unparseable refused", "::::", "registry.internal.example/ap", true},
		{"first-party registry refused", "registry.internal.example/ap/anything:v1", "registry.internal.example/ap", true},
		// Segment boundary: a sibling namespace that merely shares a prefix
		// string ("apple" starts with "ap") is a different registry path and
		// must be allowed — a basename/raw-prefix match would wrongly refuse it.
		{"sibling namespace allowed (not a basename match)", "registry.internal.example/apple/foo:v1", "registry.internal.example/ap", false},
		// Exact first-party path, no extra segments beyond the trusted prefix.
		{"exact first-party path refused", "registry.internal.example/ap/x:v1", "registry.internal.example/ap", true},
		// Masquerade: a foreign registry host that happens to spell out the
		// word "trustedregistry" must never be mistaken for the actual
		// trusted registry — the registry HOST must match, not any substring.
		{"foreign registry never mistaken for trusted (no false refusal)", "trustedregistry.evil.com/ap/x:v1", "registry.internal.example/ap", false},
		// docker.io/index.docker.io alias collapse: whichever spelling the
		// operator configured trustedImageRegistry with, BOTH ref spellings
		// of the same registry must be refused — proves the match is decided
		// from each side's own raw registry token canonicalized through the
		// same alias mapping, not from name.ParseReference's one-sided
		// normalization.
		{"docker.io alias: docker.io ref refused against docker.io-configured trust", "docker.io/ap/x:v1", "docker.io/ap", true},
		{"docker.io alias: index.docker.io ref refused against docker.io-configured trust", "index.docker.io/ap/x:v1", "docker.io/ap", true},
		// Bare-host trustedImageRegistry (the "oap install --image-registry
		// ghcr.io" shape, no org/path): every repository under that host must
		// be refused, and a genuinely foreign registry must still be allowed.
		{"bare-host trusted registry refuses any path under it", "ghcr.io/ap/x:v1", "ghcr.io", true},
		{"bare-host trusted registry allows a foreign registry", "quay.io/ap/x:v1", "ghcr.io", false},
		// Registry hosts are case-insensitive (DNS/registry semantics); only
		// the case of the HOST component is folded, not the repository path.
		{"mixed-case host still refused (case-insensitive host match)", "GHCR.IO/ap/x:v1", "ghcr.io/ap", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProbeImage(tc.ref, tc.trusted)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
