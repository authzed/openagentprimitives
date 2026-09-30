//go:build integration

// pkg/controllers/agentsession/netpol_envtest_test.go
package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
)

// netpolReconciler enables stamping on the shared test reconciler.
func netpolReconciler(t *testing.T, env *testenv.Env) *agentsession.Reconciler {
	t.Helper()
	r := newReconciler(t, env)
	r.Netpol = testNetpolConfig()
	return r
}

func reconcileN(t *testing.T, r *agentsession.Reconciler, ns, name string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	}
}

func TestReconcileStampsRunnerNetworkPolicy(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := netpolReconciler(t, env)

	ac := validClass("np-ac1")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, validSession("np-s1", "np-ac1")), "create AgentSession")
	reconcileN(t, r, "default", "np-s1", 4)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "np-s1"}})
	require.NoError(t, err, "steady-state re-reconcile (incl. netpol SSA re-apply) must not error")

	var np networkingv1.NetworkPolicy
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "np-s1-runner-netpol"}, &np),
		"runner NetworkPolicy should be stamped")
	assert.Equal(t, map[string]string{"agentprimitives.authzed.com/agentsession": "np-s1"},
		np.Spec.PodSelector.MatchLabels)
	require.Len(t, np.OwnerReferences, 1)
	assert.Equal(t, "AgentSession", np.OwnerReferences[0].Kind)
	assert.Len(t, np.Spec.Egress, 6, "DNS, NATS, operator, SpiceDB, sidecars, external")
	assert.Empty(t, np.Spec.Ingress, "all ingress denied")
}

func TestReconcileNetpolDisabled_NoPolicies(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env) // zero-value Netpol → disabled

	ac := validClass("np-ac2")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, validSession("np-s2", "np-ac2")), "create AgentSession")
	reconcileN(t, r, "default", "np-s2", 4)

	var np networkingv1.NetworkPolicy
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "np-s2-runner-netpol"}, &np)
	assert.True(t, apierrors.IsNotFound(err), "no NetworkPolicy when disabled, got err=%v", err)
}

func TestReconcileStampsSandboxNetworkPolicy(t *testing.T) {
	cases := []struct {
		name        string
		network     spiceboxv1alpha1.SpiceboxNetwork
		createClass bool
		wantEgress  int
	}{
		{name: "class mode none → full-deny sandbox policy", network: spiceboxv1alpha1.SpiceboxNetwork{Mode: spiceboxv1alpha1.NetworkModeNone}, createClass: true, wantEgress: 0},
		{name: "class mode allowlist → DNS + 443/80 coarse egress", network: spiceboxv1alpha1.SpiceboxNetwork{Mode: spiceboxv1alpha1.NetworkModeAllowlist, AllowedHosts: []string{"api.example.com"}}, createClass: true, wantEgress: 2},
		{name: "missing SpiceboxClass → fail-closed full-deny policy", createClass: false, wantEgress: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.Shared(t)
			ctx := context.Background()
			r := netpolReconciler(t, env)

			className := "np-toolbelt"
			if tc.createClass {
				require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
					ObjectMeta: metav1.ObjectMeta{Name: className},
					Spec: spiceboxv1alpha1.SpiceboxClassSpec{
						Image:     "spicebox-sandbox:dev",
						Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
						Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "echo", Command: []string{"/bin/echo"}}},
						Network:   tc.network,
					},
				}), "create SpiceboxClass")
			}
			require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
				ObjectMeta: metav1.ObjectMeta{Name: "np-id1", Namespace: "default"},
			}), "create AgentIdentity")

			acName := "np-ac-b"
			ac := validClass(acName)
			ac.Spec.AgentIdentity = "np-id1"
			ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
				{Name: "code", Class: className, Toolspecs: []string{}},
			}
			require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
			markValid(t, env, ac)

			sessName := "np-sb"
			require.NoError(t, env.Client.Create(ctx, validSession(sessName, acName)), "create AgentSession")
			reconcileN(t, r, "default", sessName, 5)

			var np networkingv1.NetworkPolicy
			require.NoError(t,
				env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: sessName + "-code-sandbox-netpol"}, &np),
				"sandbox NetworkPolicy should be stamped")
			assert.Equal(t, map[string]string{"agentprimitives.authzed.com/session": sessName + "-code"},
				np.Spec.PodSelector.MatchLabels)
			require.Len(t, np.OwnerReferences, 1)
			assert.Equal(t, "AgentSession", np.OwnerReferences[0].Kind)
			assert.Equal(t, sessName, np.OwnerReferences[0].Name)
			assert.Len(t, np.Spec.Egress, tc.wantEgress)
			assert.Empty(t, np.Spec.Ingress)
		})
	}
}

// seedSeparatePodSidecarFixture creates a secret-gated SidecarToolbox, an
// AgentClass referencing it, an AgentSession, and the satisfied
// per-session secret-output Secret — the minimal fixture for the
// separate-pod sidecar path. Names derive from the given prefix.
func seedSeparatePodSidecarFixture(t *testing.T, env *testenv.Env, prefix string) (sessName, ref string) {
	t.Helper()
	ctx := context.Background()
	ref = prefix + "-box"
	tb := sidecarToolboxWithSecretInputs(t, ref, "TOKEN", "env", "token-key")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	acName := prefix + "-ac"
	ac := classWithSidecars(acName,
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: ref, Ref: ref},
	)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sessName = prefix + "-s"
	require.NoError(t, env.Client.Create(ctx, validSession(sessName, acName)), "create AgentSession")

	soSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretoutsrv.SecretOutputSecretName(sessName),
			Namespace: "default",
		},
		Data: map[string][]byte{"token-key": []byte("tok")},
	}
	require.NoError(t, env.Client.Create(ctx, soSecret), "create secret-output Secret")
	return sessName, ref
}

func TestReconcileStampsSidecarNetworkPolicy(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := netpolReconciler(t, env)

	sessName, ref := seedSeparatePodSidecarFixture(t, env, "npsc")
	reconcileToWork(t, ctx, r, sessName)

	// The sidecar pod was created…
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: sessName + "-sidecar-" + ref}, &pod),
		"separate sidecar pod must exist")

	// …and its NetworkPolicy was stamped alongside it.
	var np networkingv1.NetworkPolicy
	require.NoError(t, env.Client.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: sessName + "-sidecar-" + ref + "-netpol"}, &np),
		"sidecar NetworkPolicy should be stamped")
	assert.Equal(t, map[string]string{
		"agentprimitives.authzed.com/agentsession":   sessName,
		"agentprimitives.authzed.com/sidecartoolbox": ref,
	}, np.Spec.PodSelector.MatchLabels)
	require.Len(t, np.OwnerReferences, 1)
	assert.Equal(t, "AgentSession", np.OwnerReferences[0].Kind)

	// Default merged mode (no class network, no CR allowedHosts) is
	// none → fail-closed full egress deny.
	assert.Empty(t, np.Spec.Egress, "effective mode none → zero egress rules")

	// Ingress is runner-only on the operator-allocated sidecar port.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: sessName}, &got))
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1)
	require.Len(t, np.Spec.Ingress, 1)
	require.Len(t, np.Spec.Ingress[0].Ports, 1)
	require.NotNil(t, np.Spec.Ingress[0].Ports[0].Port, "ingress rule must pin a port")
	assert.Equal(t, got.Status.ResolvedSidecarToolboxes[0].Port,
		np.Spec.Ingress[0].Ports[0].Port.IntVal,
		"ingress port matches the resolved sidecar port")
}

// TestReconcileFieldIsolatedSidecar_SeparatePodOwnNetpolNoRunnerContainer is
// Task 2 of the plan-0a declared-isolation brief: it pins spec §3 R5's first
// two claims for a toolbox isolated by the Isolation field ALONE (no
// SecretInputs) -- (1) it is not a container in the runner pod, and (2) it
// gets its own NetworkPolicy, distinct from the runner's.
//
// The "isolation absent" row is the control: same toolbox, same fixture
// shape, Isolation simply unset. It must show the OPPOSITE of every
// assertion the "isolated" row makes. Without it these assertions could pass
// for a reason unrelated to the field -- e.g. RunModeFor always choosing
// separate-pod, or an empty container list for an unrelated reason. Row
// "isolated" fails against main's sidecars.go if the isolation branch of
// RunModeFor is reverted (confirmed by temporarily removing that branch and
// re-running -- see the task report).
func TestReconcileFieldIsolatedSidecar_SeparatePodOwnNetpolNoRunnerContainer(t *testing.T) {
	cases := []struct {
		name         string
		prefix       string
		isolation    spiceboxv1alpha1.SidecarToolboxIsolation
		wantSeparate bool
	}{
		{
			name:         "isolation=isolated, no secretInputs: separate pod + own NetworkPolicy, absent from runner pod",
			prefix:       "fiiso",
			isolation:    spiceboxv1alpha1.SidecarToolboxIsolationIsolated,
			wantSeparate: true,
		},
		{
			name:         "isolation absent (control), no secretInputs: in-pod container, no separate pod or NetworkPolicy",
			prefix:       "fiauto",
			isolation:    "",
			wantSeparate: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.Shared(t)
			ctx := context.Background()
			r := netpolReconciler(t, env)

			ref := tc.prefix + "-box"
			tb := sidecarToolboxWithIsolation(t, ref, tc.isolation)
			require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

			acName := tc.prefix + "-ac"
			ac := classWithSidecars(acName, spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: ref, Ref: ref})
			require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
			markValid(t, env, ac)

			sessName := tc.prefix + "-s"
			require.NoError(t, env.Client.Create(ctx, validSession(sessName, acName)), "create AgentSession")

			reconcileToWork(t, ctx, r, sessName)

			// Resolved RunMode matches expectation before anything else is asserted.
			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: sessName}, &got))
			require.Len(t, got.Status.ResolvedSidecarToolboxes, 1)
			wantMode := agentsession.RunModeInPod
			if tc.wantSeparate {
				wantMode = agentsession.RunModeSeparatePod
			}
			require.Equal(t, wantMode, got.Status.ResolvedSidecarToolboxes[0].RunMode, "resolved RunMode")

			// Separate pod existence.
			sepPodKey := types.NamespacedName{Namespace: "default", Name: sessName + "-sidecar-" + ref}
			sepPodErr := env.Client.Get(ctx, sepPodKey, &corev1.Pod{})

			// Claim 2: the sidecar's own NetworkPolicy.
			sepNPErr := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: sessName + "-sidecar-" + ref + "-netpol"}, &networkingv1.NetworkPolicy{})

			if tc.wantSeparate {
				assert.NoError(t, sepPodErr, "isolated toolbox must get its own separate pod")
				assert.NoError(t, sepNPErr, "claim 2: isolated toolbox must get its own NetworkPolicy")

				// The runner pod is HELD until the separate sidecar pod reports
				// Ready (controller.go's sidecarPodNotReady gate) — envtest has no
				// kubelet, so simulate readiness the same way
				// TestReconcileSecretGatedSidecar_SeparatePodCreatedAndPodIPReflected
				// does, then reconcile again so the runner pod actually gets built.
				var sepPod corev1.Pod
				require.NoError(t, env.Client.Get(ctx, sepPodKey, &sepPod), "re-get separate sidecar pod to mark it Ready")
				sepPod.Status.PodIP = "10.9.9.9"
				sepPod.Status.Phase = corev1.PodRunning
				sepPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				require.NoError(t, env.Client.Status().Update(ctx, &sepPod), "set separate sidecar pod Ready+PodIP")
				reconcileToWork(t, ctx, r, sessName)
			} else {
				assert.True(t, apierrors.IsNotFound(sepPodErr), "control: isolation-absent toolbox must NOT get a separate pod, got err=%v", sepPodErr)
				assert.True(t, apierrors.IsNotFound(sepNPErr), "control: isolation-absent toolbox must NOT get a separate NetworkPolicy, got err=%v", sepNPErr)
			}

			// Claim 1: container placement in the runner pod.
			var runnerPod corev1.Pod
			require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: sessName + "-runner"}, &runnerPod),
				"runner pod should exist")
			hasSidecarContainer := false
			for _, c := range runnerPod.Spec.Containers {
				if c.Name == "sidecar-"+ref {
					hasSidecarContainer = true
				}
			}
			if tc.wantSeparate {
				assert.False(t, hasSidecarContainer, "claim 1: isolated toolbox must NOT be a container in the runner pod")
			} else {
				assert.True(t, hasSidecarContainer, "control: isolation-absent toolbox must run in-pod")
			}
		})
	}
}

func TestReconcileSidecarOrphan_NetpolDeletedWithPod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := netpolReconciler(t, env)

	sessName, ref := seedSeparatePodSidecarFixture(t, env, "nporph")
	reconcileToWork(t, ctx, r, sessName)

	npKey := types.NamespacedName{Namespace: "default", Name: sessName + "-sidecar-" + ref + "-netpol"}
	var np networkingv1.NetworkPolicy
	require.NoError(t, env.Client.Get(ctx, npKey, &np),
		"sidecar NetworkPolicy must exist before toolbox removal")

	// Remove the toolbox from the AgentClass mid-session.
	var liveAC spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: "nporph-ac"}, &liveAC))
	liveAC.Spec.SidecarToolboxes = nil
	require.NoError(t, env.Client.Update(ctx, &liveAC), "remove sidecar toolbox from AgentClass")

	// Cleanup happens in a single pass; the extra two reconciles are headroom
	// for status-update requeues that follow the delete sweep.
	reconcileN(t, r, "default", sessName, 3)

	// Pod AND policy are both reaped.
	var pod corev1.Pod
	podErr := env.Client.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: sessName + "-sidecar-" + ref}, &pod)
	assert.True(t, apierrors.IsNotFound(podErr), "orphaned sidecar pod deleted, got err=%v", podErr)
	npErr := env.Client.Get(ctx, npKey, &np)
	assert.True(t, apierrors.IsNotFound(npErr), "orphaned sidecar NetworkPolicy deleted, got err=%v", npErr)
}

// workshopEgressFixture seeds a secret-gated (separate-pod) SidecarToolbox +
// AgentClass + AgentSession — the same shape seedSeparatePodSidecarFixture
// builds — sanctions it via a real ClusterAgentSettings (the ONLY path
// ensureWorkshop will let a Workshop CR survive, sanctioned or not, on every
// reconcile pass), then manually stands up a Ready Workshop CR naming this
// sidecar and controller-owned by the session, bypassing the workshop
// controller's own provisioning reconcile (out of scope here; only the READ
// side, workshopIdentityFor, matters to this test).
//
// t.Cleanup deletes the cluster-scoped ClusterAgentSettings singleton
// afterward so it does not leak into the rest of this package's shared
// envtest instance.
func workshopEgressFixture(t *testing.T, env *testenv.Env, prefix string) (sessName, ref string) {
	t.Helper()
	ctx := context.Background()
	sessName, ref = seedSeparatePodSidecarFixture(t, env, prefix)

	cluster := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				BuilderClasses: &[]spiceboxv1alpha1.BuilderClassRef{
					{Namespace: "default", Name: prefix + "-ac", SidecarToolbox: ref},
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cluster), "create ClusterAgentSettings sanction")
	t.Cleanup(func() {
		_ = env.Client.Delete(context.Background(), cluster)
	})

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: sessName}, &sess),
		"get AgentSession for its real UID")

	ws := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WorkshopName(sessName),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spiceboxv1alpha1.SchemeGroupVersion.String(),
				Kind:               "AgentSession",
				Name:               sess.Name,
				UID:                sess.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:        spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: sessName},
			SidecarToolbox: ref,
			Limits: spiceboxv1alpha1.WorkshopLimits{
				MaxAge:              metav1.Duration{Duration: 168 * time.Hour},
				MaxObjectsPerKind:   20,
				MaxObjects:          100,
				MaxConcurrentProbes: 2,
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")
	ws.Status = spiceboxv1alpha1.WorkshopStatus{
		Phase:     spiceboxv1alpha1.WorkshopPhaseReady,
		Namespace: "ws-" + sessName,
		SidecarIdentity: &spiceboxv1alpha1.WorkshopSidecarIdentity{
			ServiceAccount: sessName + "-workshop-sa",
			TokenSecret:    sessName + "-workshop-token",
		},
	}
	require.NoError(t, env.Client.Status().Update(ctx, ws), "set Workshop Ready + SidecarIdentity")
	return sessName, ref
}

// operatorAndAPIServerPorts reports which of the workshop-only egress
// additions a rule's ports match: the operator peer (8082+8443, as a pair)
// or the external apiserver rule (6443 alone).
func operatorAndAPIServerPorts(ports []string) (isOperator, isAPIServer bool) {
	isOperator = len(ports) == 2 && (ports[0] == "TCP/8082" || ports[1] == "TCP/8082")
	isAPIServer = len(ports) == 1 && ports[0] == "TCP/6443"
	return
}

// TestReconcileStampsWorkshopSidecarNetworkPolicy proves the end-to-end
// wiring (controller.go's isWorkshopSidecar computation →
// ensureSidecarNetworkPolicy → BuildSidecarNetworkPolicy) for the ONE
// sidecar a Ready, session-owned Workshop names: its NetworkPolicy carries
// egress to the operator (8082+8443) and the apiserver (6443), on top of
// whatever EffectiveNetworkMode already grants.
func TestReconcileStampsWorkshopSidecarNetworkPolicy(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := netpolReconciler(t, env)

	sessName, ref := workshopEgressFixture(t, env, "npwsyes")
	reconcileToWork(t, ctx, r, sessName)

	var np networkingv1.NetworkPolicy
	require.NoError(t, env.Client.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: sessName + "-sidecar-" + ref + "-netpol"}, &np),
		"workshop sidecar NetworkPolicy should be stamped")

	var foundOperator, foundAPIServer bool
	for _, rule := range np.Spec.Egress {
		isOperator, isAPIServer := operatorAndAPIServerPorts(policyPorts(rule.Ports))
		if isOperator {
			foundOperator = true
			assert.ElementsMatch(t, []string{"TCP/8082", "TCP/8443"}, policyPorts(rule.Ports))
		}
		if isAPIServer {
			foundAPIServer = true
		}
	}
	assert.True(t, foundOperator, "workshop sidecar must have operator egress on 8082+8443, got %+v", np.Spec.Egress)
	assert.True(t, foundAPIServer, "workshop sidecar must have external apiserver egress on 6443, got %+v", np.Spec.Egress)
}

// TestReconcileStampsNonWorkshopSidecarNetworkPolicy_Unchanged is the
// contrasting negative: a separate-pod sidecar with no Workshop naming it
// (the ordinary case, and everything before the workshop seam existed) gets
// none of the operator/apiserver egress additions. Read alongside
// TestReconcileStampsWorkshopSidecarNetworkPolicy above, this proves the
// gate actually distinguishes the two — not merely that the workshop case
// carries extra rules.
func TestReconcileStampsNonWorkshopSidecarNetworkPolicy_Unchanged(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := netpolReconciler(t, env)

	sessName, ref := seedSeparatePodSidecarFixture(t, env, "npwsno")
	reconcileToWork(t, ctx, r, sessName)

	var np networkingv1.NetworkPolicy
	require.NoError(t, env.Client.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: sessName + "-sidecar-" + ref + "-netpol"}, &np),
		"sidecar NetworkPolicy should be stamped")

	for i, rule := range np.Spec.Egress {
		isOperator, isAPIServer := operatorAndAPIServerPorts(policyPorts(rule.Ports))
		assert.False(t, isOperator, "egress rule %d must not grant operator egress, got %+v", i, rule)
		assert.False(t, isAPIServer, "egress rule %d must not grant apiserver egress, got %+v", i, rule)
	}
}
