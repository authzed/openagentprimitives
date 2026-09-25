package sidecartoolbox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// These tests pin the fix for the SECOND way the admission probe failed live
// on oap-desktop 2026-09-11 (after declaration-only mode fixed the first).
// egress-external already lets the OPERATOR dial probe-labeled pods on :8080
// (guarded by TestNetworkPolicyAllowsOperatorToReachSidecarProbe) — but that
// is half the data path: under a namespace default-deny floor
// (agentprimitives-system ships one) nothing admitted INGRESS to the probe
// pod, so the dial dropped for the whole deadline while the kubelet's
// readiness probe, from the node's own netns, kept reporting Ready.
//
// The ingress half is controller-owned, not static config/: probe pods are
// minted in the TOOLBOX's namespace — any namespace — and a config-only
// policy would fix agentprimitives-system while any session namespace with a
// default-deny floor re-broke silently.

func probeNetpolCR() *spiceboxv1alpha1.SidecarToolbox {
	return &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "workshop", Namespace: "agentprimitives-system", UID: "uid-1"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{TimeoutSeconds: 1}},
		},
	}
}

// TestProbeNetworkPolicy_SelectsTheProbePod derives the agreement instead of
// transcribing it: the policy's selector is applied to the labels
// buildProbePod actually stamps, so a rename on either side fails HERE — and
// a policy that selects a DIFFERENT toolbox's probe pod must not match.
func TestProbeNetworkPolicy_SelectsTheProbePod(t *testing.T) {
	cr := probeNetpolCR()
	r := &Reconciler{}
	pod := r.buildProbePod(cr, "probe-workshop-x")
	np := buildProbeNetworkPolicy(cr, "op-ns")

	sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
	require.NoError(t, err)
	assert.True(t, sel.Matches(labels.Set(pod.Labels)),
		"the ingress policy must select exactly the pod buildProbePod builds")

	other := probeNetpolCR()
	other.Name = "other-toolbox"
	otherPod := r.buildProbePod(other, "probe-other-x")
	assert.False(t, sel.Matches(labels.Set(otherPod.Labels)),
		"per-toolbox scope: one toolbox's policy must not select another's probe pods")
}

// TestBuildProbeNetworkPolicy_AdmitsOperatorOnly pins the policy's whole
// shape: ingress-only, exactly the operator as peer, exactly the probe port —
// a probe pod needs nothing else, and everything else stays denied.
func TestBuildProbeNetworkPolicy_AdmitsOperatorOnly(t *testing.T) {
	cr := probeNetpolCR()
	np := buildProbeNetworkPolicy(cr, "op-ns")

	assert.Equal(t, cr.Namespace, np.Namespace)
	require.Len(t, np.OwnerReferences, 1, "owned by the toolbox: reaped with it, standing across its probe pods")
	assert.Equal(t, "SidecarToolbox", np.OwnerReferences[0].Kind)
	assert.Equal(t, cr.Name, np.OwnerReferences[0].Name)

	assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, np.Spec.PolicyTypes,
		"ingress-only: this policy must not change what the probe pod may dial OUT")

	require.Len(t, np.Spec.Ingress, 1)
	rule := np.Spec.Ingress[0]
	require.Len(t, rule.From, 1)
	assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": "op-ns"}, rule.From[0].NamespaceSelector.MatchLabels)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "spicebox-operator"}, rule.From[0].PodSelector.MatchLabels)
	require.Len(t, rule.Ports, 1)
	assert.Equal(t, int32(probePort), rule.Ports[0].Port.IntVal)
	assert.Equal(t, corev1.ProtocolTCP, *rule.Ports[0].Protocol)
}

// TestProbeOnce_StampsProbeNetpol: the policy must exist by the time the pod
// runs (created BEFORE the pod), converge on re-probe instead of erroring on
// AlreadyExists, and not be created at all when stamping is disabled — the
// same opt-in the per-session policies key off.
func TestProbeOnce_StampsProbeNetpol(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	for _, enabled := range []bool{true, false} {
		cr := probeNetpolCR()
		base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
		r := &Reconciler{
			Client:      base,
			ProbeNetpol: ProbeNetpolConfig{Enabled: enabled, OperatorNamespace: "op-ns"},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := r.probeOnce(ctx, cr)
		cancel()
		require.Error(t, err, "no kubelet in a fake client; the probe itself must still time out")

		var np networkingv1.NetworkPolicy
		getErr := base.Get(context.Background(), client.ObjectKey{Namespace: cr.Namespace, Name: probeNetworkPolicyName(cr)}, &np)
		if enabled {
			require.NoError(t, getErr, "enabled: the policy must be stamped before the probe pod exists")
			// Converge: a second probe with the object already present must not fail.
			ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
			_, err2 := r.probeOnce(ctx2, cr)
			cancel2()
			require.Error(t, err2)
			require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: cr.Namespace, Name: probeNetworkPolicyName(cr)}, &np))
		} else {
			assert.True(t, client.IgnoreNotFound(getErr) == nil && getErr != nil,
				"disabled: no policy — stamping is the cluster operator's opt-in, mirroring the per-session policies")
		}
	}
}

// TestBuildProbePod_SetsProbeModeEnv pins that the admission probe opts the
// image into declaration-only via AP_PROBE_MODE. Without it, the workshop
// image fail-closes at boot (no identity) and the probe pod crashes instead
// of serving tools/list — the reachability probe could never pass. This is
// the deliberate, EXPLICIT signal that distinguishes a probe (no identity
// ever) from a session sidecar transiently awaiting its identity (must
// fail-closed so the runner waits).
func TestBuildProbePod_SetsProbeModeEnv(t *testing.T) {
	r := &Reconciler{}
	pod := r.buildProbePod(probeNetpolCR(), "probe-workshop-x")
	var got string
	var present bool
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "AP_PROBE_MODE" {
			got, present = e.Value, true
		}
	}
	require.True(t, present, "the probe pod must set AP_PROBE_MODE so a fail-closed image serves declaration-only")
	assert.Equal(t, "1", got)
}
