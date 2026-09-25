package cloud

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
)

var npGVR = schema.GroupVersionResource{
	Group:    "networking.k8s.io",
	Version:  "v1",
	Resource: "networkpolicies",
}

func newNetpolClients(t *testing.T) Clients {
	t.Helper()
	return Clients{Dynamic: dynfake.NewSimpleDynamicClient(scheme.Scheme)}
}

// TestApplyDNSEgressPolicy_GKE verifies that a non-empty CIDR slice produces
// an allow-dns-cloud NetworkPolicy carrying those CIDRs.
func TestApplyDNSEgressPolicy_GKE(t *testing.T) {
	cl := newNetpolClients(t)
	cidrs := []string{"169.254.0.0/16"}
	require.NoError(t, ApplyDNSEgressPolicy(context.Background(), cl, cidrs))

	got, err := cl.Dynamic.Resource(npGVR).Namespace(WebdServiceNamespace).
		Get(context.Background(), "allow-dns-cloud", metav1.GetOptions{})
	require.NoError(t, err, "allow-dns-cloud must exist when CIDRs are supplied")

	raw, err := got.MarshalJSON()
	require.NoError(t, err)
	assert.Contains(t, string(raw), "169.254.0.0/16", "policy must carry the supplied CIDR")
}

// TestApplyDNSEgressPolicy_Noop verifies that an empty CIDR slice is a no-op.
func TestApplyDNSEgressPolicy_Noop(t *testing.T) {
	cl := newNetpolClients(t)
	require.NoError(t, ApplyDNSEgressPolicy(context.Background(), cl, nil))

	_, err := cl.Dynamic.Resource(npGVR).Namespace(WebdServiceNamespace).
		Get(context.Background(), "allow-dns-cloud", metav1.GetOptions{})
	assert.Error(t, err, "no allow-dns-cloud should exist when CIDRs are empty")
}

// TestApplyDNSEgressPolicy_Idempotent verifies that applying the same CIDR slice
// twice does not error. In production the second call is a server-side apply
// that succeeds; the dynamic fake cannot serve an apply patch for unstructured,
// so this test opts into applyDoc's fake-only Update fallback (see
// applyUpdateFallback — it is off in every production build).
func TestApplyDNSEgressPolicy_Idempotent(t *testing.T) {
	SetApplyUpdateFallbackForTest(t)
	cl := newNetpolClients(t)
	cidrs := []string{"169.254.0.0/16"}
	require.NoError(t, ApplyDNSEgressPolicy(context.Background(), cl, cidrs))
	require.NoError(t, ApplyDNSEgressPolicy(context.Background(), cl, cidrs))
}

// TestApplyDNSEgressPolicy_ExcludesExtractord is the I2 fix: the cloud-CIDR
// DNS policy must NOT use a bare podSelector: {} (matches every pod) —
// extractord dials nothing and resolves no names by design
// (config/networkpolicy/extractord.yaml's egress: []), so a bare selector
// would silently hand the one pod parsing hostile user-uploaded bytes a
// working DNS path — a live exfiltration channel via CoreDNS. Mirrors the
// same exclusion in the static config/networkpolicy/allow-dns.yaml.
func TestApplyDNSEgressPolicy_ExcludesExtractord(t *testing.T) {
	cl := newNetpolClients(t)
	require.NoError(t, ApplyDNSEgressPolicy(context.Background(), cl, []string{"169.254.0.0/16"}))

	got, err := cl.Dynamic.Resource(npGVR).Namespace(WebdServiceNamespace).
		Get(context.Background(), "allow-dns-cloud", metav1.GetOptions{})
	require.NoError(t, err)

	raw, err := got.MarshalJSON()
	require.NoError(t, err)
	s := string(raw)
	assert.Contains(t, s, "agentprimitives-extractord", "podSelector must name extractord to exclude it")
	assert.Contains(t, s, "NotIn", "extractord must be excluded via NotIn, not matched in")
	assert.NotContains(t, s, `"podSelector":{}`, "a bare podSelector would match every pod, including extractord")
}

// TestApplyDNSEgressPolicy_ApplyRejectionNeverRewritesTheLivePolicy is the
// counterpart to the gate the sibling cmd/oap/internal/kube.Apply already has:
// when the server-side apply patch is rejected for any reason other than
// NotFound, applyDoc must FAIL, not degrade to a full-object Update.
//
// That Update carries no resourceVersion and no managedFields, so it replaces
// the live object with the manifest — discarding controller-added finalizers,
// ownerReferences, injected webhook caBundles, and every field another manager
// owns — and then returns nil, telling `oap install` the policy was applied. The
// object here is a NetworkPolicy, i.e. a security control, and
// ApplyDNSEgressPolicy runs against the already-existing one on every
// re-install.
//
// The dynamic fake cannot serve an apply patch for *unstructured.Unstructured,
// so a pre-existing object reproduces exactly that non-NotFound rejection.
func TestApplyDNSEgressPolicy_ApplyRejectionNeverRewritesTheLivePolicy(t *testing.T) {
	cl := newNetpolClients(t)
	ctx := context.Background()

	// A live policy carrying a finalizer no manifest sets — the stand-in for
	// any field a controller or another field manager owns.
	live := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":       "allow-dns-cloud",
			"namespace":  WebdServiceNamespace,
			"finalizers": []any{"example.com/owned-by-another-manager"},
		},
		"spec": map[string]any{"podSelector": map[string]any{}},
	}}
	_, err := cl.Dynamic.Resource(npGVR).Namespace(WebdServiceNamespace).
		Create(ctx, live, metav1.CreateOptions{})
	require.NoError(t, err, "seed the pre-existing policy")

	applyErr := ApplyDNSEgressPolicy(ctx, cl, []string{"169.254.0.0/16"})
	assert.Error(t, applyErr,
		"a rejected server-side apply must be reported, never downgraded to a full-object Update that reports success")

	got, err := cl.Dynamic.Resource(npGVR).Namespace(WebdServiceNamespace).
		Get(ctx, "allow-dns-cloud", metav1.GetOptions{})
	require.NoError(t, err, "the live policy must still exist")
	assert.Equal(t, []string{"example.com/owned-by-another-manager"}, got.GetFinalizers(),
		"a failed apply must leave the live policy untouched — the Update fallback strips every field the manifest does not set")
}

// TestApplyGatewayBackendIngressPolicy_WithCIDRs verifies that a non-empty CIDR
// slice produces an allow-webd-gateway NetworkPolicy scoped to webd's port.
func TestApplyGatewayBackendIngressPolicy_WithCIDRs(t *testing.T) {
	cl := newNetpolClients(t)
	cidrs := []string{"35.191.0.0/16", "130.211.0.0/22"}
	require.NoError(t, ApplyGatewayBackendIngressPolicy(context.Background(), cl, cidrs))

	got, err := cl.Dynamic.Resource(npGVR).Namespace(WebdServiceNamespace).
		Get(context.Background(), "allow-webd-gateway", metav1.GetOptions{})
	require.NoError(t, err, "allow-webd-gateway must exist when CIDRs are supplied")

	raw, err := got.MarshalJSON()
	require.NoError(t, err)
	s := string(raw)
	assert.Contains(t, s, "35.191.0.0/16", "policy must carry the first CIDR")
	assert.Contains(t, s, "130.211.0.0/22", "policy must carry the second CIDR")
	assert.Contains(t, s, WebdServiceName, "policy must select the webd pods")
}

// TestApplyGatewayBackendIngressPolicy_Noop verifies that an empty CIDR slice
// is a no-op.
func TestApplyGatewayBackendIngressPolicy_Noop(t *testing.T) {
	cl := newNetpolClients(t)
	require.NoError(t, ApplyGatewayBackendIngressPolicy(context.Background(), cl, nil))

	_, err := cl.Dynamic.Resource(npGVR).Namespace(WebdServiceNamespace).
		Get(context.Background(), "allow-webd-gateway", metav1.GetOptions{})
	assert.Error(t, err, "no allow-webd-gateway should exist when CIDRs are empty")
}

// TestApplyGatewayBackendIngressPolicy_Idempotent verifies applying the same
// CIDR slice twice does not error. Opts into applyDoc's fake-only Update
// fallback for the same reason as TestApplyDNSEgressPolicy_Idempotent.
func TestApplyGatewayBackendIngressPolicy_Idempotent(t *testing.T) {
	SetApplyUpdateFallbackForTest(t)
	cl := newNetpolClients(t)
	cidrs := []string{"35.191.0.0/16"}
	require.NoError(t, ApplyGatewayBackendIngressPolicy(context.Background(), cl, cidrs))
	require.NoError(t, ApplyGatewayBackendIngressPolicy(context.Background(), cl, cidrs))
}
