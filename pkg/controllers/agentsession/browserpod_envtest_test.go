//go:build integration

// pkg/controllers/agentsession/browserpod_envtest_test.go
//
// End-to-end integration tests for the AgentSession reconciler's browser-toolbox
// wiring (Task 8b): per-session browser pod creation, policy ConfigMap + gate
// token Secret + NetworkPolicy stamping, readiness gating of the runner pod,
// reap/wake with a byte-stable gate token, and the runner Role's pinned
// secrets/get grant for the gate token.
//
// Pod readiness is hand-stamped — envtest has no kubelet — mirroring
// setPodReady in identitychoice_envtest_test.go (adapted to the browser pod's
// "gate"/"chrome" containers).
package agentsession_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

// newBrowserReconciler is newReconciler plus the two fields the browser wiring
// needs: the first-party browser image ref (empty ref fails the pod build) and
// an enabled NetpolConfig so BuildBrowserNetworkPolicy emits a policy to assert.
func newBrowserReconciler(t *testing.T, env *testenv.Env) *agentsession.Reconciler {
	t.Helper()
	r := newReconciler(t, env)
	r.BrowserImageRef = "ap-browser:dev"
	r.Netpol = agentsession.NetpolConfig{
		Enabled:           true,
		OperatorNamespace: "agentprimitives-system",
		NATSPort:          4222,
		SpiceDBPort:       50051,
		SpiceDBInCluster:  true,
	}
	return r
}

// browserToolbox returns a minimal valid BrowserToolbox CR in allowlist mode
// over the given hosts.
func browserToolbox(name string, hosts ...string) *spiceboxv1alpha1.BrowserToolbox {
	sites := make([]spiceboxv1alpha1.BrowserSite, 0, len(hosts))
	for _, h := range hosts {
		sites = append(sites, spiceboxv1alpha1.BrowserSite{Host: h})
	}
	return &spiceboxv1alpha1.BrowserToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access:      spiceboxv1alpha1.BrowserAccess{Mode: "allowlist", Sites: sites},
			Interaction: "act",
			State:       "ephemeral",
		},
	}
}

// classWithBrowsers clones validClass and attaches browser refs.
func classWithBrowsers(name string, refs ...spiceboxv1alpha1.AgentClassBrowserToolboxRef) *spiceboxv1alpha1.AgentClass {
	ac := validClass(name)
	ac.Spec.BrowserToolboxes = refs
	return ac
}

// setBrowserPodReady flips a browser pod to Running+Ready with a PodIP — the
// edge reconcileBrowserPod watches to reflect BrowserPodIP and release the
// runner. envtest has no kubelet, so the test stamps the status by hand
// (mirrors setPodReady for the runner pod).
func setBrowserPodReady(t *testing.T, ctx context.Context, env *testenv.Env, podName string) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &pod),
		"get browser pod to mark Ready")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.44.0.7"
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "chrome", Ready: true},
		{Name: "gate", Ready: true},
	}
	require.NoError(t, env.Client.Status().Update(ctx, &pod), "mark browser pod Ready")
}

// TestReconcileCreatesBrowserPodAndPolicy proves the reconciler materializes the
// full browser-toolbox object set: the two-container pod, its NetworkPolicy
// (private-range except-list + gate-port ingress), the policy ConfigMap (whose
// JSON equals the derived PolicyDoc), the gate-token Secret, and a status entry
// carrying the effective sites, gate port, and the token hash of the secret.
func TestReconcileCreatesBrowserPodAndPolicy(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	tb := browserToolbox("btb-create", "example.com")
	require.NoError(t, env.Client.Create(ctx, tb), "create BrowserToolbox")

	ac := classWithBrowsers("ac-browser-create",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-create"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-browser-create", "ac-browser-create")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	reconcileToWork(t, ctx, r, "s-browser-create")

	podName := agentsession.BrowserPodName(sess, "btb-create")

	// Pod: two containers, owned by the session.
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &pod),
		"browser pod must be created")
	require.Len(t, pod.Spec.Containers, 2, "browser pod has chrome + gate containers")
	require.Len(t, pod.OwnerReferences, 1, "browser pod owner ref")
	assert.Equal(t, "AgentSession", pod.OwnerReferences[0].Kind)
	assert.Equal(t, "s-browser-create", pod.OwnerReferences[0].Name)

	// NetworkPolicy: ingress on the gate port, egress with the private-range
	// except-list.
	var np networkingv1.NetworkPolicy
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: agentsession.BrowserNetworkPolicyName(sess, "btb-create")}, &np),
		"browser NetworkPolicy must be created")
	require.Len(t, np.Spec.Ingress, 1, "one ingress rule")
	require.Len(t, np.Spec.Ingress[0].Ports, 1, "ingress on exactly the gate port")
	assert.Equal(t, int32(9223), np.Spec.Ingress[0].Ports[0].Port.IntVal, "ingress on the gate port 9223")
	var sawExcept bool
	for _, e := range np.Spec.Egress {
		for _, to := range e.To {
			if to.IPBlock != nil {
				assert.Contains(t, to.IPBlock.Except, "169.254.0.0/16",
					"browser egress must except the link-local/metadata range")
				sawExcept = true
			}
		}
	}
	assert.True(t, sawExcept, "browser NetworkPolicy must carry an IPBlock egress rule with an except-list")

	// ConfigMap: the policy JSON unmarshals to the derived PolicyDoc.
	var cm corev1.ConfigMap
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: agentsession.BrowserPolicyConfigMapName(sess, "btb-create")}, &cm),
		"browser policy ConfigMap must be created")
	var doc browserpolicy.PolicyDoc
	require.NoError(t, json.Unmarshal([]byte(cm.Data["policy.json"]), &doc), "policy ConfigMap holds valid PolicyDoc JSON")
	assert.Equal(t, "allowlist", doc.Mode, "policy mode carried through")
	require.Len(t, doc.Sites, 1, "one site in the policy doc")
	assert.Equal(t, "example.com", doc.Sites[0].Host, "policy site host carried through")

	// Secret: the per-session gate token.
	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: agentsession.BrowserTokenSecretName(sess, "btb-create")}, &sec),
		"browser gate-token Secret must be created")
	require.NotEmpty(t, sec.Data["token"], "gate-token Secret carries a token")

	// Status entry: effective sites, gate port, token hash of the secret value.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-browser-create"}, &got), "get session")
	require.Len(t, got.Status.ResolvedBrowserToolboxes, 1, "one resolved browser toolbox in status")
	rt := got.Status.ResolvedBrowserToolboxes[0]
	assert.Equal(t, "web", rt.Name, "resolved browser Name (LLM prefix)")
	assert.Equal(t, "btb-create", rt.Ref, "resolved browser Ref (CR name)")
	assert.Equal(t, int32(9223), rt.GatePort, "resolved browser GatePort == 9223")
	require.Len(t, rt.EffectiveSites, 1, "one effective site")
	assert.Equal(t, "example.com", rt.EffectiveSites[0].Host, "effective site host")
	assert.Equal(t, "explicit", rt.EffectiveSites[0].Source, "effective site source is explicit in Phase A")
	assert.Equal(t, agentsession.SecretTokenHash(sec.Data["token"]), rt.TokenHash,
		"status TokenHash is the hash of the secret value actually in hand")
}

// TestReconcileBrowserIsIdempotent proves a byte-identical re-reconcile rewrites
// nothing: the pod, ConfigMap, Secret, and session resourceVersions are stable
// across a second full pass (read-before-mint + write-if-different).
func TestReconcileBrowserIsIdempotent(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	tb := browserToolbox("btb-idem", "example.com", "docs.example.org")
	require.NoError(t, env.Client.Create(ctx, tb), "create BrowserToolbox")
	ac := classWithBrowsers("ac-browser-idem",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-idem"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	sess := validSession("s-browser-idem", "ac-browser-idem")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	reconcileToWork(t, ctx, r, "s-browser-idem")

	podName := agentsession.BrowserPodName(sess, "btb-idem")
	cmName := agentsession.BrowserPolicyConfigMapName(sess, "btb-idem")
	secName := agentsession.BrowserTokenSecretName(sess, "btb-idem")

	rv := func(obj interface{ GetResourceVersion() string }) string { return obj.GetResourceVersion() }

	var pod1 corev1.Pod
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &pod1))
	var cm1 corev1.ConfigMap
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: cmName}, &cm1))
	var sec1 corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &sec1))
	var sess1 spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-browser-idem"}, &sess1))

	// A second full set of reconciles must not rewrite any of them.
	reconcileToWork(t, ctx, r, "s-browser-idem")

	var pod2 corev1.Pod
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &pod2))
	var cm2 corev1.ConfigMap
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: cmName}, &cm2))
	var sec2 corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &sec2))
	var sess2 spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-browser-idem"}, &sess2))

	assert.Equal(t, rv(&pod1), rv(&pod2), "browser pod must not be rewritten on re-reconcile")
	assert.Equal(t, rv(&cm1), rv(&cm2), "browser policy ConfigMap must not be rewritten on re-reconcile")
	assert.Equal(t, rv(&sec1), rv(&sec2), "browser gate-token Secret must not be rewritten on re-reconcile")
	assert.Equal(t, rv(&sess1), rv(&sess2), "AgentSession must not be rewritten on re-reconcile")
}

// TestRunnerWaitsForBrowserPod proves runner-pod creation is held until every
// browser pod is Ready: no runner after the first pass, then a runner once the
// browser pod reports Running+Ready+PodIP.
func TestRunnerWaitsForBrowserPod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	tb := browserToolbox("btb-wait", "example.com")
	require.NoError(t, env.Client.Create(ctx, tb), "create BrowserToolbox")
	ac := classWithBrowsers("ac-browser-wait",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-wait"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	sess := validSession("s-browser-wait", "ac-browser-wait")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	reconcileToWork(t, ctx, r, "s-browser-wait")

	// Browser pod exists but is not Ready (no kubelet) → runner must be absent.
	podName := agentsession.BrowserPodName(sess, "btb-wait")
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &corev1.Pod{}),
		"browser pod exists after first reconcile")
	var runner corev1.Pod
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: agentsession.RunnerPodName(sess)}, &runner)
	assert.True(t, apierrors.IsNotFound(err), "runner pod must NOT exist while the browser pod is not Ready, got err=%v", err)

	// Mark the browser pod Ready; the runner must now come up.
	setBrowserPodReady(t, ctx, env, podName)
	reconcileToWork(t, ctx, r, "s-browser-wait")

	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: agentsession.RunnerPodName(sess)}, &runner),
		"runner pod must exist once the browser pod is Ready")
}

// TestReapDeletesBrowserPodWakeRecreates proves reap deletes the browser pod but
// keeps its gate-token Secret, and a wake recreates the pod reusing the same
// token (read-before-mint keeps TokenHash byte-stable across reap/wake).
func TestReapDeletesBrowserPodWakeRecreates(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	tb := browserToolbox("btb-reap", "example.com")
	require.NoError(t, env.Client.Create(ctx, tb), "create BrowserToolbox")
	ac := classWithBrowsers("ac-browser-reap",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-reap"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	sess := validSession("s-browser-reap", "ac-browser-reap")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	reconcileToWork(t, ctx, r, "s-browser-reap")

	podName := agentsession.BrowserPodName(sess, "btb-reap")
	secName := agentsession.BrowserTokenSecretName(sess, "btb-reap")
	key := types.NamespacedName{Namespace: "default", Name: "s-browser-reap"}

	// Capture the original token hash.
	var before spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &before), "get session before reap")
	require.Len(t, before.Status.ResolvedBrowserToolboxes, 1)
	tokenHash := before.Status.ResolvedBrowserToolboxes[0].TokenHash
	require.NotEmpty(t, tokenHash, "token hash is recorded before reap")

	// Drive to the reap path: an archive-swept Succeeded session reaps its pods
	// immediately (terminalReapAction), and no wake annotation is present yet, so
	// shouldWake is false and the reap block runs.
	var drive spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &drive), "get session to drive to Succeeded")
	drive.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	now := metav1.Now()
	drive.Status.FinishedAt = &now
	meta.SetStatusCondition(&drive.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.AgentSessionConditionIdle,
		Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived,
	})
	require.NoError(t, env.Client.Status().Update(ctx, &drive), "mark session archive-swept Succeeded")

	reconcileToWork(t, ctx, r, "s-browser-reap")

	// Browser pod reaped; gate-token Secret retained.
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &corev1.Pod{})
	assert.True(t, apierrors.IsNotFound(err), "browser pod must be reaped, got err=%v", err)
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: secName}, &corev1.Secret{}),
		"gate-token Secret must survive reap")

	// Wake: stamp the wake annotation (newer than lastWakeAt), so shouldWake is
	// true, the reap block is skipped, and the browser loop recreates the pod.
	var waking spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &waking), "get session to wake")
	if waking.Annotations == nil {
		waking.Annotations = map[string]string{}
	}
	waking.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	require.NoError(t, env.Client.Update(ctx, &waking), "stamp wake annotation")

	reconcileToWork(t, ctx, r, "s-browser-reap")

	// Pod recreated; token (and so TokenHash) unchanged.
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &corev1.Pod{}),
		"browser pod must be recreated on wake")
	var after spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &after), "get session after wake")
	require.Len(t, after.Status.ResolvedBrowserToolboxes, 1)
	assert.Equal(t, tokenHash, after.Status.ResolvedBrowserToolboxes[0].TokenHash,
		"gate token (TokenHash) must be unchanged across reap/wake")
}

// TestRunnerRoleGrantsBrowserTokenRead proves the runner Role pins secrets/get
// to each browser gate-token Secret derived from the class's browser refs.
func TestRunnerRoleGrantsBrowserTokenRead(t *testing.T) {
	sess := validSession("s-browser-rbac", "ac-browser-rbac")
	ac := classWithBrowsers("ac-browser-rbac",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-rbac"})

	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok", nil, nil, "", "", "", "", "", "", nil)

	want := agentsession.BrowserTokenSecretName(sess, "btb-rbac")
	var found bool
	for _, rule := range role.Rules {
		if !ruleCoversSecretsGet(rule) {
			continue
		}
		for _, n := range rule.ResourceNames {
			if n == want {
				found = true
			}
		}
	}
	assert.True(t, found, "runner Role must grant secrets/get on the browser gate-token Secret %q", want)
}

func ruleCoversSecretsGet(rule rbacv1.PolicyRule) bool {
	hasCore, hasSecrets, hasGet := false, false, false
	for _, g := range rule.APIGroups {
		if g == "" {
			hasCore = true
		}
	}
	for _, res := range rule.Resources {
		if res == "secrets" {
			hasSecrets = true
		}
	}
	for _, v := range rule.Verbs {
		if v == "get" {
			hasGet = true
		}
	}
	return hasCore && hasSecrets && hasGet
}
