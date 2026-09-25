//go:build integration

// pkg/controllers/agentsession/contentguarddetector_test.go
//
// Envtest reconcile tests for the content-guard detector pod lifecycle:
//
//  1. A detector pod + deny-all-egress NetworkPolicy are created.
//  2. PodName is stamped on status; PodIP is empty until the pod is Ready.
//  3. Runner-pod creation is held (RunnerReady=False/AwaitingDetector) while
//     PodIP is empty; once PodIP is reflected, the runner pod is created with
//     CONTENTGUARD_DETECTOR_ENDPOINT set.
//
// Mirrors the sidecar_inject_envtest_test.go pattern: PodRunnerFactory is
// wired with a real envtest apiserver; the pod IP is simulated by patching
// the pod's Status.PodIP + setting PodReady=True (envtest has no kubelet).
package agentsession_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"

	// Blank import registers the prompt-injection inspector in the global
	// contentguard registry (via init()), so contentguardregistry.Get("prompt-injection")
	// resolves during reconcile.
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection"
	// Also register url-allowlist for the noDetectorProvider test.
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"
)

// detectorImage is the fictional detector image used in the test config.
const detectorImage = "ghcr.io/example/promptinjection-detector:v1"

// detectorPort is the port declared in the test inspector config.
const detectorPort = int32(9080)

// detectorCfgRaw returns the raw prompt-injection inspector config JSON.
func detectorCfgRaw(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"detectorImage": detectorImage,
		"port":          detectorPort,
	})
	require.NoError(t, err, "marshal prompt-injection config")
	return raw
}

// createClusterSettingsWithDetector creates a ClusterAgentSettings (singleton
// "cluster") that includes a prompt-injection content inspector. The settings
// resolver picks it up on the next reconcile pass and populates
// sess.Status.EffectiveSettings.ContentInspectors, which the detector block
// reads. The object is cluster-scoped, so it must be cleaned up at test end;
// use t.Cleanup or envtest's per-test environment isolation.
func createClusterSettingsWithDetector(t *testing.T, ctx context.Context, env *testenv.Env) {
	t.Helper()
	cfgRaw := detectorCfgRaw(t)
	inspectors := []spiceboxv1alpha1.ContentInspectorConfig{
		{
			ID:     "prompt-injection",
			Config: apiextv1.JSON{Raw: cfgRaw},
		},
	}
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				ContentInspectors: &inspectors,
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas),
		"create ClusterAgentSettings with prompt-injection inspector")
}

// createClusterSettingsWithURLAllowlist creates a ClusterAgentSettings with a
// url-allowlist inspector (which does not implement DetectorProvider) to test
// that no detector pod is spawned.
func createClusterSettingsWithURLAllowlist(t *testing.T, ctx context.Context, env *testenv.Env) {
	t.Helper()
	rawCfg, err := json.Marshal(map[string]any{
		"rules": []map[string]string{{"domain": "example.com", "action": "allow"}},
	})
	require.NoError(t, err)
	inspectors := []spiceboxv1alpha1.ContentInspectorConfig{
		{ID: "url-allowlist", Config: apiextv1.JSON{Raw: rawCfg}},
	}
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				ContentInspectors: &inspectors,
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas),
		"create ClusterAgentSettings with url-allowlist inspector")
}

// labelSelector is a ListOption that filters pods by label key=value.
func labelSelector(key, value string) client.ListOption {
	return client.MatchingLabels{key: value}
}

// driveReconcileN runs exactly n reconcile passes for the named session,
// ignoring errors (callers check object state instead).
func driveReconcileN(t *testing.T, ctx context.Context, r *agentsession.Reconciler, name string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
	}
}

// TestReconcile_promptInjection_createsSeparateDetectorPod_noEgress is the
// primary integration test for the content-guard detector pod lifecycle.
//
//  1. Detector pod "sidecar-<session>-prompt-injection" is created.
//  2. A NetworkPolicy selecting {session, sidecar:prompt-injection} exists
//     with PolicyTypes incl. Egress and ZERO egress rules (deny-all).
//  3. status.resolvedContentGuardDetectors[0].PodName is set; PodIP is
//     empty until the pod is Ready.
//  4. While PodIP is empty, RunnerReady=False/AwaitingDetector is set and
//     the runner Pod is NOT created.
//  5. After patching the pod status with a real IP + PodReady=True, the
//     runner pod is created with
//     CONTENTGUARD_DETECTOR_ENDPOINT=http://<podIP>:<port>.
func TestReconcile_promptInjection_createsSeparateDetectorPod_noEgress(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// Wire a ClusterAgentSettings that includes the prompt-injection inspector.
	// The settings resolver picks it up on each reconcile pass and populates
	// eff.ContentInspectors so the detector block sees it.
	createClusterSettingsWithDetector(t, ctx, env)

	// Build the AgentClass.
	ac := validClass("ac-det")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	// Create the AgentSession.
	sess := validSession("s-det", "ac-det")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Drive through finalizer installation + settings resolution + detector block.
	driveReconcileN(t, ctx, r, "s-det", 4)

	// ── Assertion 1: detector pod exists ────────────────────────────────────
	detPodName := "sidecar-s-det-prompt-injection"
	var detPod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: detPodName}, &detPod),
		"detector pod %q must exist after reconcile passes", detPodName)

	// The detector pod must have the correct image.
	require.Len(t, detPod.Spec.Containers, 1, "detector pod must have exactly one container")
	assert.Equal(t, detectorImage, detPod.Spec.Containers[0].Image, "detector pod image")

	// AutomountServiceAccountToken=false: the detector has no business holding
	// an apiserver credential.
	require.NotNil(t, detPod.Spec.AutomountServiceAccountToken)
	assert.False(t, *detPod.Spec.AutomountServiceAccountToken,
		"detector pod must not automount a service-account token")

	// ── Assertion 2: deny-all-egress NetworkPolicy exists ───────────────────
	// The detector NP must use the SAME name cleanupOrphanedSidecarPods reaps
	// by (SidecarNetworkPolicyName), not cosidecar's default "sidecar-<sess>-<ref>".
	// Otherwise a mid-session inspector removal orphans the NP until session-GC.
	npName := agentsession.SidecarNetworkPolicyName(
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s-det"}}, "prompt-injection")
	assert.Equal(t, "s-det-sidecar-prompt-injection-netpol", npName,
		"detector NP name must match the cleanup-reaped name")
	var np networkingv1.NetworkPolicy
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: npName}, &np),
		"detector NetworkPolicy %q must exist", npName)

	// PolicyTypes must include Egress.
	var hasEgress bool
	for _, pt := range np.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeEgress {
			hasEgress = true
		}
	}
	assert.True(t, hasEgress, "NetworkPolicy must include PolicyType Egress")

	// Zero egress rules = deny-all egress.
	assert.Empty(t, np.Spec.Egress, "NetworkPolicy must have zero egress rules (deny-all)")

	// ── Assertion 3: status PodName stamped, PodIP empty ────────────────────
	var gotSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-det"}, &gotSess))
	require.Len(t, gotSess.Status.ResolvedContentGuardDetectors, 1,
		"one ResolvedContentGuardDetector in status")
	det := gotSess.Status.ResolvedContentGuardDetectors[0]
	assert.Equal(t, "prompt-injection", det.Inspector, "detector Inspector")
	assert.Equal(t, detPodName, det.PodName, "detector PodName")
	assert.Empty(t, det.PodIP, "PodIP must be empty before pod is Ready")
	assert.NotZero(t, det.Port, "detector Port must be allocated")

	// ── Assertion 4: runner pod NOT created; AwaitingDetector reason set ────
	var runnerPod corev1.Pod
	runnerErr := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-det-runner"}, &runnerPod)
	assert.True(t, runnerErr != nil, "runner pod must NOT exist while detector PodIP is empty")

	cond := apimeta.FindStatusCondition(gotSess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	require.NotNil(t, cond, "RunnerReady condition must be set while awaiting detector")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "RunnerReady must be False while awaiting detector")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector, cond.Reason,
		"RunnerReady reason must be AwaitingDetector")

	// ── Simulate detector pod becoming Ready with a PodIP ───────────────────
	// envtest has no kubelet; we manually patch the pod's Status to simulate
	// kubelet reporting Ready+PodIP.
	const fakePodIP = "10.244.1.42"
	{
		var liveDetPod corev1.Pod
		require.NoError(t, env.Client.Get(ctx,
			types.NamespacedName{Namespace: "default", Name: detPodName}, &liveDetPod))
		liveDetPod.Status.PodIP = fakePodIP
		liveDetPod.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}}
		require.NoError(t, env.Client.Status().Update(ctx, &liveDetPod),
			"patch detector pod status with Ready+PodIP")
	}

	// Drive the reconciler again — it should now reflect the PodIP and create
	// the runner pod.
	driveReconcileN(t, ctx, r, "s-det", 3)

	// ── Assertion 5: PodIP reflected, runner pod created with env ───────────
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-det"}, &gotSess))
	require.Len(t, gotSess.Status.ResolvedContentGuardDetectors, 1)
	det = gotSess.Status.ResolvedContentGuardDetectors[0]
	assert.Equal(t, fakePodIP, det.PodIP, "PodIP must be reflected after pod is Ready")

	// Runner pod must now exist.
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-det-runner"}, &runnerPod),
		"runner pod must be created after detector PodIP is reflected")

	// Runner container (index 0) must have CONTENTGUARD_DETECTOR_ENDPOINT set.
	require.NotEmpty(t, runnerPod.Spec.Containers, "runner pod must have containers")
	runnerContainer := runnerPod.Spec.Containers[0]
	assert.Equal(t, "runner", runnerContainer.Name, "Containers[0] must be the runner container")

	expectedEndpoint := fmt.Sprintf("http://%s:%d", fakePodIP, det.Port)
	var gotEndpoint string
	for _, e := range runnerContainer.Env {
		if e.Name == "CONTENTGUARD_DETECTOR_ENDPOINT" {
			gotEndpoint = e.Value
			break
		}
	}
	assert.Equal(t, expectedEndpoint, gotEndpoint,
		"runner container must have CONTENTGUARD_DETECTOR_ENDPOINT=http://<podIP>:<port>")
}

// TestReconcile_noDetectorProvider_noDetectorPod proves that an inspector that
// does NOT implement DetectorProvider (e.g. url-allowlist) does not spawn a
// detector pod, and the runner pod is created normally.
func TestReconcile_noDetectorProvider_noDetectorPod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// Wire a ClusterAgentSettings with url-allowlist (not a DetectorProvider).
	createClusterSettingsWithURLAllowlist(t, ctx, env)

	ac := validClass("ac-nd")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-nd", "ac-nd")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	driveReconcileN(t, ctx, r, "s-nd", 4)

	// ResolvedContentGuardDetectors must be empty or nil.
	var gotSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-nd"}, &gotSess))
	assert.Empty(t, gotSess.Status.ResolvedContentGuardDetectors,
		"no detectors should appear in status for a non-DetectorProvider inspector")

	// No sidecar pod with a detector label should exist (only the runner).
	var podList corev1.PodList
	require.NoError(t, env.Client.List(ctx, &podList,
		labelSelector("agentprimitives.authzed.com/agentsession", "s-nd"),
	))
	for _, p := range podList.Items {
		assert.NotContains(t, p.Name, "sidecar-s-nd-url-allowlist",
			"no detector pod should be created for url-allowlist (not a DetectorProvider)")
	}

	// The runner pod SHOULD be created (no detector gating needed).
	var runnerPod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-nd-runner"}, &runnerPod),
		"runner pod must be created when no detector is required")

	// CONTENTGUARD_DETECTOR_ENDPOINT must NOT be set on the runner.
	for _, e := range runnerPod.Spec.Containers[0].Env {
		assert.NotEqual(t, "CONTENTGUARD_DETECTOR_ENDPOINT", e.Name,
			"CONTENTGUARD_DETECTOR_ENDPOINT must not be set when no detector is needed")
	}
}

// TestReconcile_detectorNeverReady_deadlineBackstopFailsClosed proves the
// no-silent-hang deadline backstop: a detector pod that never reports a PodIP
// (e.g. ImagePullBackOff) must not requeue on AwaitingDetector forever. Once the
// session has been awaiting a detector IP past detectorReadyDeadline (measured
// from CreationTimestamp), the reconcile flips to Failed/ContentGuardHalt with a
// message naming the inspector and the cause — not another AwaitingDetector requeue.
//
// envtest sets CreationTimestamp to "now" on Create and it is immutable, so the
// deadline is tripped by advancing the reconciler's injectable clock (r.Now)
// past CreationTimestamp + detectorReadyDeadline rather than waiting wall-clock.
func TestReconcile_detectorNeverReady_deadlineBackstopFailsClosed(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	createClusterSettingsWithDetector(t, ctx, env)

	ac := validClass("ac-dl")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-dl", "ac-dl")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// First drive a few passes with the real clock: the detector pod is created
	// but never gets a PodIP (no kubelet in envtest), so the session parks in
	// RunnerReady=False/AwaitingDetector — NOT yet Failed.
	driveReconcileN(t, ctx, r, "s-dl", 4)

	var gotSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-dl"}, &gotSess))
	require.Len(t, gotSess.Status.ResolvedContentGuardDetectors, 1, "detector recorded in status")
	assert.Empty(t, gotSess.Status.ResolvedContentGuardDetectors[0].PodIP, "PodIP stays empty (never Ready)")
	cond := apimeta.FindStatusCondition(gotSess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	require.NotNil(t, cond, "RunnerReady condition set while awaiting detector")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector, cond.Reason,
		"before the deadline the reason is AwaitingDetector, not a failure")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, gotSess.Status.Phase,
		"session must NOT be Failed before the deadline")

	// Advance the reconciler's clock past CreationTimestamp + deadline so the
	// next AwaitingDetector pass trips the backstop.
	r.Now = func() time.Time {
		return gotSess.CreationTimestamp.Time.Add(6 * time.Minute)
	}

	driveReconcileN(t, ctx, r, "s-dl", 2)

	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-dl"}, &gotSess))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, gotSess.Status.Phase,
		"session must be Failed once the detector deadline is exceeded")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionContentGuardHalt, gotSess.Status.FailureReason,
		"failure reason must be ContentGuardHalt")
	failedCond := apimeta.FindStatusCondition(gotSess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, failedCond, "Failed condition must be set")
	assert.Equal(t, metav1.ConditionTrue, failedCond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionContentGuardHalt, failedCond.Reason)
	// The message must name the offending inspector and the cause (no-silent-hang).
	assert.Contains(t, failedCond.Message, "prompt-injection",
		"failure message must name the inspector")
	assert.Contains(t, failedCond.Message, "never became Ready",
		"failure message must state the cause")

	// The runner pod must NOT have been created (failed closed before runner start).
	var runnerPod corev1.Pod
	runnerErr := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-dl-runner"}, &runnerPod)
	assert.Error(t, runnerErr, "runner pod must not be created when the detector deadline trips")
}
