//go:build integration

// pkg/controllers/agentsession/sidecar_reconcile_test.go
//
// Reconcile-level tests for the AgentSession sidecar-toolbox block (step
// 4d): per-session Secret materialization, status snapshotting, the
// BootFailed terminal path, and secret-gated (separate-pod) toolbox gating.
// Pure-function helpers (port allocation, network merge, container build) are
// covered in sidecars_test.go.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
)

// sidecarToolbox returns a minimal valid SidecarToolbox CR. provider and
// envVar drive whether the controller resolves a credential or writes an
// empty Secret; an empty provider/envVar is the echo-example path.
func sidecarToolbox(t *testing.T, name, provider, envVar string) *spiceboxv1alpha1.SidecarToolbox {
	t.Helper()
	return &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:      name,
			Version:   "v1",
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/" + name + ":v1"},
			Sandbox:   spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{
				Provider: provider,
				EnvVar:   envVar,
			},
			Tools: []spiceboxv1alpha1.MCPServerTool{},
		},
	}
}

// classWithSidecars clones validClass and attaches sidecar refs. Each ref's
// Name is the LLM prefix and Ref names the SidecarToolbox CR.
func classWithSidecars(name string, refs ...spiceboxv1alpha1.AgentClassSidecarToolboxRef) *spiceboxv1alpha1.AgentClass {
	ac := validClass(name)
	ac.Spec.SidecarToolboxes = refs
	return ac
}

// reconcileToWork drives the reconcile loop enough times to install the
// finalizer and perform the full setup pass.
func reconcileToWork(t *testing.T, ctx context.Context, r *agentsession.Reconciler, name string) {
	t.Helper()
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
	}
}

// TestReconcileSidecarNoProviderWritesEmptySecretAndSnapshot covers the
// echo-example path: a sidecar with no upstream credential need writes an
// empty per-session Secret (so the container's envFrom resolves) and
// snapshots the resolved toolbox into status.
func TestReconcileSidecarNoProviderWritesEmptySecretAndSnapshot(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// Empty provider/envVar → no credential resolution → empty Secret.
	tb := sidecarToolbox(t, "echo", "", "")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-echo", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "echo", Ref: "echo"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-echo", "ac-echo")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	reconcileToWork(t, ctx, r, "s-echo")

	// Per-session sidecar Secret exists and carries no data (empty).
	var sec corev1.Secret
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "agentsession-s-echo-toolbox-echo"}, &sec),
		"per-session sidecar Secret should exist before Start")
	assert.Empty(t, sec.Data, "echo sidecar Secret should carry no credential data")

	// Status snapshot: one resolved toolbox with an allocated port and the
	// CR's spec, and the session did NOT fail.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-echo"}, &got), "Get AgentSession")
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1, "one resolved sidecar in status")
	rt := got.Status.ResolvedSidecarToolboxes[0]
	assert.Equal(t, "echo", rt.Name, "resolved sidecar Name (LLM prefix)")
	assert.Equal(t, "echo", rt.Ref, "resolved sidecar Ref (CR name)")
	assert.Equal(t, int32(18080), rt.Port, "allocated loopback port starts at 18080")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"echo sidecar must not boot-fail the session")
	failed := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	if failed != nil {
		assert.NotEqual(t, metav1.ConditionTrue, failed.Status, "Failed condition must not be True")
	}
}

// TestReconcileSidecarMissingCRBootFails covers the missing-CR path: an
// AgentClass referencing a SidecarToolbox that does not exist must drive the
// session to phase=Failed with Failed=True/SidecarToolboxMissing.
func TestReconcileSidecarMissingCRBootFails(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// No SidecarToolbox CR is created — the ref dangles.
	ac := classWithSidecars("ac-missing", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "ghost", Ref: "ghost"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-missing", "ac-missing")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	reconcileToWork(t, ctx, r, "s-missing")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-missing"}, &got), "Get AgentSession")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "session should boot-fail")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionSidecarToolboxMissing, got.Status.FailureReason, "FailureReason")
	failed := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, failed, "Failed condition should be set")
	assert.Equal(t, metav1.ConditionTrue, failed.Status, "Failed=True")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionSidecarToolboxMissing, failed.Reason, "Failed reason")
	require.NotNil(t, got.Status.FinishedAt, "FinishedAt should be stamped on boot-fail")
}

// TestReconcileSidecarStaticCredProjectsEnvVar covers the credentialed path:
// a sidecar declaring a provider + envVar resolves the named AgentCredential
// (static) via credresolve and projects the resolved token into the
// per-session Secret under the declared env-var name. The credential name
// (<ref>-creds) round-trips with the toolbox: authkind SuggestedName.
func TestReconcileSidecarStaticCredProjectsEnvVar(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// Backing Secret the static credential reads.
	backing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reddit-backing", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("super-secret-token")},
	}
	require.NoError(t, env.Client.Create(ctx, backing), "create backing Secret")

	// AgentIdentity holding a static credential named "<ref>-creds".
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-reddit", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "reddit-creds", // = <ref>-creds, matches authkind SuggestedName
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "reddit-backing", Key: "token"},
				},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity")

	tb := sidecarToolbox(t, "reddit", "reddit", "REDDIT_TOKEN")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-reddit", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "reddit", Ref: "reddit"})
	ac.Spec.AgentIdentity = "ai-reddit" // class-default identity drives resolution
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-reddit", "ac-reddit")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	reconcileToWork(t, ctx, r, "s-reddit")

	// The per-session Secret carries the resolved token under REDDIT_TOKEN.
	var sec corev1.Secret
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "agentsession-s-reddit-toolbox-reddit"}, &sec),
		"per-session sidecar Secret should exist")
	// StringData is write-only; the apiserver folds it into Data on persist.
	assert.Equal(t, "super-secret-token", string(sec.Data["REDDIT_TOKEN"]),
		"resolved static credential should be projected into the declared env var")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-reddit"}, &got), "Get AgentSession")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "credentialed sidecar must not boot-fail")
}

// TestReconcileSidecarMissingCredentialBootFails covers the explicit-error
// path: a sidecar declaring a provider + envVar whose AgentIdentity lacks the
// expected <ref>-creds credential must boot-fail (not silently skip).
func TestReconcileSidecarMissingCredentialBootFails(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// AgentIdentity exists but has NO matching credential.
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-empty", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentIdentitySpec{},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity")

	tb := sidecarToolbox(t, "needy", "reddit", "REDDIT_TOKEN")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-needy", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "needy", Ref: "needy"})
	ac.Spec.AgentIdentity = "ai-empty"
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-needy", "ac-needy")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	reconcileToWork(t, ctx, r, "s-needy")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-needy"}, &got), "Get AgentSession")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "missing credential should boot-fail")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed, got.Status.FailureReason,
		"FailureReason should be SidecarBootFailed (not Missing — the CR exists)")
}

// sidecarToolboxWithSecretInputs returns a SidecarToolbox with secretInputs
// set so it is classified as a secret-gated (separate-pod) toolbox.
// The single SecretInput uses name=envVarName, deliver=deliverSpec, from=fromHandle.
func sidecarToolboxWithSecretInputs(t *testing.T, name, envVarName, deliverSpec, fromHandle string) *spiceboxv1alpha1.SidecarToolbox {
	t.Helper()
	tb := sidecarToolbox(t, name, "", "")
	tb.Spec.SecretInputs = []spiceboxv1alpha1.SidecarToolboxSecretInput{
		{Name: envVarName, Deliver: deliverSpec, From: fromHandle},
	}
	return tb
}

// TestReconcileSecretGatedSidecar_AwaitingSecretUntilSatisfied covers the
// two-phase secret-gated separate-pod path:
//
// Case A: the toolbox has secretInputs, the secret-output handle is NOT yet
// satisfied → after reconcile:
//   - resolved RunMode=="separate-pod"
//   - AwaitingSecret==true
//   - no sidecar pod is created (pod creation is Task 5; we assert absence)
//   - session does NOT boot-fail
//
// Case B: seed SatisfiedSecretOutputs + the per-session secret-output Secret
// with the matching Data key, then reconcile again →
//   - AwaitingSecret==false
//   - session still does NOT boot-fail
func TestReconcileSecretGatedSidecar_AwaitingSecretUntilSatisfied(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// SidecarToolbox with one SecretInput (from="kubeconfig").
	tb := sidecarToolboxWithSecretInputs(t, "k8s-proxy", "KUBECONFIG", "file:/root/.kube/config", "kubeconfig")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-k8s-proxy",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "k8s-proxy", Ref: "k8s-proxy"},
	)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-k8s-proxy", "ac-k8s-proxy")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// ── Case A: secret-output NOT yet satisfied ──────────────────────────────
	reconcileToWork(t, ctx, r, "s-k8s-proxy")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-k8s-proxy"}, &got),
		"Case A: Get AgentSession")
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1, "Case A: one resolved sidecar in status")
	rtA := got.Status.ResolvedSidecarToolboxes[0]
	assert.Equal(t, agentsession.RunModeSeparatePod, rtA.RunMode,
		"Case A: secret-gated toolbox must have RunMode=separate-pod")
	assert.True(t, rtA.AwaitingSecret,
		"Case A: AwaitingSecret must be true when the secret-output is not yet satisfied")

	// No sidecar pod named like the toolbox ref should exist (Task 5 creates it).
	var sidecarPod corev1.Pod
	sidecarPodKey := types.NamespacedName{Namespace: "default", Name: "s-k8s-proxy-sidecar-k8s-proxy"}
	err := env.Client.Get(ctx, sidecarPodKey, &sidecarPod)
	assert.True(t, err != nil, "Case A: no sidecar pod must exist while AwaitingSecret (Task 5 creates it)")

	// Session must not boot-fail while awaiting the secret.
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"Case A: session must not boot-fail while awaiting secret")
	failedCond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	if failedCond != nil {
		assert.NotEqual(t, metav1.ConditionTrue, failedCond.Status,
			"Case A: Failed condition must not be True while awaiting secret")
	}

	// ── Case B: seed satisfied secret-output ─────────────────────────────────
	// Write the per-session secret-output Secret with Data["kubeconfig"] set.
	secretOutputSecretName := secretoutsrv.SecretOutputSecretName("s-k8s-proxy")
	soSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretOutputSecretName,
			Namespace: "default",
		},
		Data: map[string][]byte{
			"kubeconfig": []byte("kubeconfig-value"),
		},
	}
	require.NoError(t, env.Client.Create(ctx, soSecret),
		"Case B: create per-session secret-output Secret")

	// Stamp SatisfiedSecretOutputs on the session status (the runner does this
	// via WriteSatisfiedSecretOutput; we seed it directly for the test).
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-k8s-proxy"}, &fresh),
		"Case B: Get AgentSession before seeding status")
	now := metav1.Now()
	fresh.Status.SatisfiedSecretOutputs = []spiceboxv1alpha1.SecretOutputCompletion{
		{
			Handle:     "so-handle-1",
			SecretName: secretOutputSecretName,
			WrittenAt:  &now,
		},
	}
	require.NoError(t, env.Client.Status().Update(ctx, &fresh),
		"Case B: seed SatisfiedSecretOutputs on session status")

	// Reconcile again — this time the secret-output is satisfied.
	reconcileToWork(t, ctx, r, "s-k8s-proxy")

	var gotB spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-k8s-proxy"}, &gotB),
		"Case B: Get AgentSession after seeding")
	require.Len(t, gotB.Status.ResolvedSidecarToolboxes, 1, "Case B: one resolved sidecar in status")
	rtB := gotB.Status.ResolvedSidecarToolboxes[0]
	assert.Equal(t, agentsession.RunModeSeparatePod, rtB.RunMode,
		"Case B: RunMode still separate-pod after secret satisfied")
	assert.False(t, rtB.AwaitingSecret,
		"Case B: AwaitingSecret must be false once the secret-output is satisfied")

	// Session must not boot-fail after satisfaction.
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, gotB.Status.Phase,
		"Case B: session must not boot-fail after secret satisfied")
}

// TestReconcileSecretGatedSidecar_SeparatePodCreatedAndPodIPReflected covers
// Task 5: once a secret-gated toolbox's secret-output is satisfied, the
// operator creates a SEPARATE per-session pod with the sidecar container + the
// secret wired, stamps SidecarPodName, and — once the pod reports Ready+PodIP
// (set manually here; envtest has no kubelet) — reflects SidecarPodIP into
// status.
func TestReconcileSecretGatedSidecar_SeparatePodCreatedAndPodIPReflected(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// file:-delivered secret input so we exercise the volume/mount wiring.
	tb := sidecarToolboxWithSecretInputs(t, "k8s-proxy", "KUBECONFIG", "file:/root/.kube/config", "kubeconfig")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-k8s-proxy",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "k8s-proxy", Ref: "k8s-proxy"},
	)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-k8s-proxy", "ac-k8s-proxy")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Seed the satisfied secret-output Secret BEFORE the first reconcile so the
	// gate opens immediately.
	soSecretName := secretoutsrv.SecretOutputSecretName("s-k8s-proxy")
	soSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: soSecretName, Namespace: "default"},
		Data:       map[string][]byte{"kubeconfig": []byte("kubeconfig-value")},
	}
	require.NoError(t, env.Client.Create(ctx, soSecret), "create per-session secret-output Secret")

	reconcileToWork(t, ctx, r, "s-k8s-proxy")

	// The separate sidecar pod exists with the sidecar container + secret wired.
	podKey := types.NamespacedName{Namespace: "default", Name: "s-k8s-proxy-sidecar-k8s-proxy"}
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, podKey, &pod),
		"separate sidecar pod must exist once the secret is satisfied")
	require.Len(t, pod.Spec.Containers, 1, "exactly one sidecar container")
	c := pod.Spec.Containers[0]
	assert.Equal(t, "ghcr.io/x/k8s-proxy:v1", c.Image, "sidecar container image from source")
	assert.Equal(t, corev1.RestartPolicyOnFailure, pod.Spec.RestartPolicy, "long-running MCP restarts on failure")

	// file:-delivery → a Secret-backed volume projecting the From key, mounted.
	var soVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Secret != nil && pod.Spec.Volumes[i].Secret.SecretName == soSecretName {
			soVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, soVol, "a volume projects the secret-output Secret")
	require.Len(t, soVol.Secret.Items, 1, "one secret key projected")
	assert.Equal(t, "kubeconfig", soVol.Secret.Items[0].Key, "projects the From key")
	mounted := false
	for _, vm := range c.VolumeMounts {
		if vm.Name == soVol.Name {
			mounted = true
		}
	}
	assert.True(t, mounted, "the secret volume is mounted into the sidecar container")

	// SidecarPodName stamped; PodIP not yet reflected (pod not Ready).
	sessKey := types.NamespacedName{Namespace: "default", Name: "s-k8s-proxy"}
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &got))
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1)
	rt := got.Status.ResolvedSidecarToolboxes[0]
	assert.Equal(t, "s-k8s-proxy-sidecar-k8s-proxy", rt.SidecarPodName, "SidecarPodName stamped")
	assert.NotEmpty(t, rt.SecretTokenHash, "SecretTokenHash stamped from the resolved secret")
	assert.Empty(t, rt.SidecarPodIP, "SidecarPodIP not reflected before the pod is Ready")

	// Manually set the pod's PodIP + PodReady (envtest has no kubelet).
	pod.Status.PodIP = "10.1.2.3"
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, env.Client.Status().Update(ctx, &pod), "set sidecar pod Ready+PodIP")

	// Reconcile again — the PodIP is now reflected into status.
	reconcileToWork(t, ctx, r, "s-k8s-proxy")

	var got2 spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &got2))
	require.Len(t, got2.Status.ResolvedSidecarToolboxes, 1)
	rt2 := got2.Status.ResolvedSidecarToolboxes[0]
	assert.Equal(t, "s-k8s-proxy-sidecar-k8s-proxy", rt2.SidecarPodName, "SidecarPodName still stamped")
	assert.Equal(t, "10.1.2.3", rt2.SidecarPodIP, "SidecarPodIP reflected once Ready")
	assert.False(t, rt2.AwaitingSecret, "AwaitingSecret remains false")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got2.Status.Phase, "session must not boot-fail")
}

// TestReconcileSecretGatedSidecar_TokenRotationReplacesPod covers Task 7 Part A:
// a running separate-pod sidecar (SecretTokenHash H1, PodIP reflected) whose
// per-session secret-output Secret VALUE changes (producer re-emitted a
// different token → new hash H2) must have its pod REPLACED — the operator
// deletes the old pod and requeues, the next reconcile recreates it with the
// new value, status.SecretTokenHash flips to H2, and SidecarPodIP is
// re-reflected for the new pod (empty until it reports Ready again).
func TestReconcileSecretGatedSidecar_TokenRotationReplacesPod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	tb := sidecarToolboxWithSecretInputs(t, "k8s-proxy", "KUBECONFIG", "file:/root/.kube/config", "kubeconfig")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-k8s-proxy",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "k8s-proxy", Ref: "k8s-proxy"},
	)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-k8s-proxy", "ac-k8s-proxy")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Seed the satisfied secret-output Secret with the ORIGINAL token value.
	soSecretName := secretoutsrv.SecretOutputSecretName("s-k8s-proxy")
	soSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: soSecretName, Namespace: "default"},
		Data:       map[string][]byte{"kubeconfig": []byte("token-v1")},
	}
	require.NoError(t, env.Client.Create(ctx, soSecret), "create per-session secret-output Secret")

	reconcileToWork(t, ctx, r, "s-k8s-proxy")

	// Drive the first pod to Ready+PodIP (envtest has no kubelet).
	podKey := types.NamespacedName{Namespace: "default", Name: "s-k8s-proxy-sidecar-k8s-proxy"}
	var pod1 corev1.Pod
	require.NoError(t, env.Client.Get(ctx, podKey, &pod1), "first sidecar pod must exist")
	originalPodUID := pod1.UID
	pod1.Status.PodIP = "10.1.2.3"
	pod1.Status.Phase = corev1.PodRunning
	pod1.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, env.Client.Status().Update(ctx, &pod1), "set first pod Ready+PodIP")

	reconcileToWork(t, ctx, r, "s-k8s-proxy")

	sessKey := types.NamespacedName{Namespace: "default", Name: "s-k8s-proxy"}
	var gotV1 spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &gotV1))
	require.Len(t, gotV1.Status.ResolvedSidecarToolboxes, 1)
	hashV1 := gotV1.Status.ResolvedSidecarToolboxes[0].SecretTokenHash
	require.NotEmpty(t, hashV1, "SecretTokenHash stamped for v1 token")
	require.Equal(t, "10.1.2.3", gotV1.Status.ResolvedSidecarToolboxes[0].SidecarPodIP,
		"v1 PodIP reflected before rotation")

	// ── Token rotation: the producer re-emits a DIFFERENT value ──────────────
	var liveSO corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: soSecretName}, &liveSO))
	liveSO.Data["kubeconfig"] = []byte("token-v2-rotated")
	require.NoError(t, env.Client.Update(ctx, &liveSO), "rotate the secret-output value")

	// One reconcile should detect the hash change and DELETE the old pod,
	// requeuing. Run a single reconcile so we can observe the delete in
	// isolation (the deterministic pod name is reused by the recreate).
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: sessKey})

	// The old pod must be gone (deleted) or already replaced by a new-UID pod.
	var afterDelete corev1.Pod
	delErr := env.Client.Get(ctx, podKey, &afterDelete)
	if delErr == nil {
		assert.NotEqual(t, originalPodUID, afterDelete.UID,
			"if a pod exists at the name it must be the NEW pod, not the stale one")
	}

	// Subsequent reconciles recreate the pod with the new value. Drive the new
	// pod to Ready+PodIP and reconcile so the IP re-reflects.
	reconcileToWork(t, ctx, r, "s-k8s-proxy")
	var pod2 corev1.Pod
	require.NoError(t, env.Client.Get(ctx, podKey, &pod2), "replacement sidecar pod must be recreated")
	assert.NotEqual(t, originalPodUID, pod2.UID, "replacement pod must be a distinct object (new UID)")
	pod2.Status.PodIP = "10.1.2.99"
	pod2.Status.Phase = corev1.PodRunning
	pod2.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, env.Client.Status().Update(ctx, &pod2), "set replacement pod Ready+PodIP")

	reconcileToWork(t, ctx, r, "s-k8s-proxy")

	var gotV2 spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &gotV2))
	require.Len(t, gotV2.Status.ResolvedSidecarToolboxes, 1)
	rtV2 := gotV2.Status.ResolvedSidecarToolboxes[0]
	assert.NotEqual(t, hashV1, rtV2.SecretTokenHash, "SecretTokenHash must flip to H2 after rotation")
	assert.Equal(t, agentsession.SecretTokenHash([]byte("token-v2-rotated")), rtV2.SecretTokenHash,
		"SecretTokenHash must match the new token value")
	assert.Equal(t, "10.1.2.99", rtV2.SidecarPodIP, "SidecarPodIP re-reflected for the replacement pod")
	assert.False(t, rtV2.AwaitingSecret, "AwaitingSecret remains false")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, gotV2.Status.Phase, "session must not boot-fail")
}

// TestReconcileSidecarCascade_OwnerRefOnSeparatePod asserts Part 1 of Task 8:
// a separate-pod sidecar pod carries a Controller+BlockOwnerDeletion ownerref
// pointing to the AgentSession. This is what drives GC cascade when the
// session is deleted in a real cluster. envtest does not run the GC controller
// so we assert the ownerref itself, not that the pod vanishes.
// crashSidecarPod puts the named sidecar pod into the state a kubelet produces
// for a container that started, exited non-zero, and is now backing off:
// Waiting=CrashLoopBackOff with a content-free back-off message, plus a
// LastTerminationState whose message is the container's log tail (which the
// kubelet fills in because the pod is built with FallbackToLogsOnError).
// envtest has no kubelet, so the status is set by hand.
func crashSidecarPod(t *testing.T, ctx context.Context, env *testenv.Env, podName, logTail string) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &pod),
		"get sidecar pod to crash it")
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: pod.Spec.Containers[0].Name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  "CrashLoopBackOff",
			Message: "back-off 2m40s restarting failed container=" + pod.Spec.Containers[0].Name,
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1,
			Reason:   "Error",
			Message:  logTail,
		}},
	}}
	require.NoError(t, env.Client.Status().Update(ctx, &pod), "set sidecar pod CrashLoopBackOff")
}

// TestReconcileSecretGatedSidecar_CrashLoopMidSessionRecordsPodFailure covers
// the silent-degradation bug this change fixes.
//
// A secret-gated sidecar comes up MID-session: the agent runs the producer tool
// that emits the gating secret, and only then does the operator create the pod.
// If that pod then crash-loops it never reports Ready, so it never receives a
// PodIP — and the runner's prober, which is gated on PodIP, is never invoked.
// Before this change the operator simply requeued every 2s forever while the
// live agent, told nothing, finished its turn tool-less.
//
// The runner is already started here, so the fix must NOT kill the session: it
// records the failure (carrying the log tail, the only actionable text) for the
// runner to surface to the agent and user.
func TestReconcileSecretGatedSidecar_CrashLoopMidSessionRecordsPodFailure(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	const logTail = `no cluster match for "demo-cluster.abc123" in region "us-east-1" using /etc/dedicated-mcp/clusters.csv`

	tb := sidecarToolboxWithSecretInputs(t, "crash-box", "KUBECONFIG", "file:/root/.kube/config", "kubeconfig")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-crash",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "crash-box", Ref: "crash-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-crash", "ac-crash")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	sessKey := types.NamespacedName{Namespace: "default", Name: "s-crash"}
	soSecretName := secretoutsrv.SecretOutputSecretName("s-crash")
	soSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: soSecretName, Namespace: "default"},
		Data:       map[string][]byte{"kubeconfig": []byte("kubeconfig-value")},
	}
	require.NoError(t, env.Client.Create(ctx, soSecret), "create per-session secret-output Secret")

	// The runner is ALREADY live — the state a secret-gated sidecar is always in,
	// since the agent running in the runner is what produced the gating secret.
	// podAlreadyStarted reads the RunnerReady condition, so seed it.
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &live))
	meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{
		Type:    string(spiceboxv1alpha1.AgentSessionConditionRunnerReady),
		Status:  metav1.ConditionTrue,
		Reason:  "RunnerReady",
		Message: "runner pod is live",
	})
	require.NoError(t, env.Client.Status().Update(ctx, &live), "seed RunnerReady")

	reconcileToWork(t, ctx, r, "s-crash")
	crashSidecarPod(t, ctx, env, "s-crash-sidecar-crash-box", logTail)
	reconcileToWork(t, ctx, r, "s-crash")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &got))
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1, "one resolved sidecar in status")
	rt := got.Status.ResolvedSidecarToolboxes[0]

	require.NotNil(t, rt.PodFailure, "a crash-looping sidecar pod must record a terminal PodFailure")
	assert.Equal(t, "CrashLoopBackOff", rt.PodFailure.Reason, "reason")
	assert.Equal(t, logTail, rt.PodFailure.Message,
		"message must carry the log tail, not the content-free kubelet back-off text")
	require.NotNil(t, rt.PodFailure.ExitCode, "a container that ran has an exit code")
	assert.Equal(t, int32(1), *rt.PodFailure.ExitCode, "exit code")
	assert.Empty(t, rt.SidecarPodIP, "a crash-looping pod never reports a PodIP")

	// The runner is live: the session keeps serving. The agent is told via the
	// recorded failure rather than having the whole session killed under it.
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"a mid-session sidecar crash must NOT fail a session whose runner is already live")
}

// TestReconcileSecretGatedSidecar_CrashLoopAtBootFailsSession covers the other
// half of the same detection: when the sidecar pod crash-loops BEFORE the
// runner pod exists there is no agent to tell, so the only way the failure can
// reach a human is the terminal session phase (which channelsd relays to the
// channel). This mirrors the content-guard detector precedent.
func TestReconcileSecretGatedSidecar_CrashLoopAtBootFailsSession(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	const logTail = `no cluster match for "demo-cluster.abc123" in region "us-east-1"`

	tb := sidecarToolboxWithSecretInputs(t, "bootcrash-box", "KUBECONFIG", "file:/root/.kube/config", "kubeconfig")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-bootcrash",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "bootcrash-box", Ref: "bootcrash-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-bootcrash", "ac-bootcrash")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Secret satisfied up front and NO RunnerReady condition seeded: the pod is
	// created on the first pass, before the runner exists.
	soSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretoutsrv.SecretOutputSecretName("s-bootcrash"),
			Namespace: "default",
		},
		Data: map[string][]byte{"kubeconfig": []byte("kubeconfig-value")},
	}
	require.NoError(t, env.Client.Create(ctx, soSecret), "create per-session secret-output Secret")

	reconcileToWork(t, ctx, r, "s-bootcrash")
	crashSidecarPod(t, ctx, env, "s-bootcrash-sidecar-bootcrash-box", logTail)
	reconcileToWork(t, ctx, r, "s-bootcrash")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-bootcrash"}, &got))

	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"a sidecar that crash-loops before the runner exists must fail the session, not hang it")

	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed, got.Status.FailureReason,
		"the failure is attributed to the sidecar, not a generic timeout")

	cond := meta.FindStatusCondition(got.Status.Conditions, string(spiceboxv1alpha1.AgentSessionConditionFailed))
	require.NotNil(t, cond, "boot failure surfaces on the Failed condition (channelsd relays it)")
	assert.Contains(t, cond.Message, logTail,
		"the terminal message must name the actual cause so the user can act on it")
	assert.Contains(t, cond.Message, "bootcrash-box", "the terminal message must name which sidecar failed")
}

func TestReconcileSidecarCascade_OwnerRefOnSeparatePod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	tb := sidecarToolboxWithSecretInputs(t, "cascade-box", "TOKEN", "env", "token-key")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-cascade",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "cascade-box", Ref: "cascade-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-cascade", "ac-cascade")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Seed the satisfied secret-output Secret so the pod is created immediately.
	soSecretName := secretoutsrv.SecretOutputSecretName("s-cascade")
	soSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: soSecretName, Namespace: "default"},
		Data:       map[string][]byte{"token-key": []byte("tok")},
	}
	require.NoError(t, env.Client.Create(ctx, soSecret), "create secret-output Secret")

	reconcileToWork(t, ctx, r, "s-cascade")

	// The separate sidecar pod must exist.
	podKey := types.NamespacedName{Namespace: "default", Name: "s-cascade-sidecar-cascade-box"}
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, podKey, &pod),
		"separate sidecar pod must exist once the secret is satisfied")

	// Re-fetch the session to get its UID (assigned by the apiserver).
	var liveSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: "s-cascade"}, &liveSess))

	// Assert the ownerref: Controller=true, BlockOwnerDeletion=true, UID matches.
	require.Len(t, pod.OwnerReferences, 1, "exactly one owner ref")
	owner := pod.OwnerReferences[0]
	assert.Equal(t, "AgentSession", owner.Kind,
		"ownerref Kind must be AgentSession")
	assert.Equal(t, "s-cascade", owner.Name,
		"ownerref Name must match the session name")
	assert.Equal(t, liveSess.UID, owner.UID,
		"ownerref UID must match the session's UID (drives GC cascade)")
	require.NotNil(t, owner.Controller,
		"ownerref Controller must be set (true = this is the gc-controller owner)")
	assert.True(t, *owner.Controller,
		"ownerref Controller=true")
	require.NotNil(t, owner.BlockOwnerDeletion,
		"ownerref BlockOwnerDeletion must be set")
	assert.True(t, *owner.BlockOwnerDeletion,
		"ownerref BlockOwnerDeletion=true (garbage collector blocks pod deletion until the owner is gone)")
}

// TestReconcileSidecarOrphan_PodDeletedWhenToolboxRemovedFromClass asserts
// Part 2 of Task 8: when a SidecarToolbox ref is REMOVED from the AgentClass
// mid-session, the reconciler deletes the orphaned separate-pod sidecar pod.
// The pod is owned by the session (not the toolbox) so it would otherwise
// linger until session deletion without this explicit cleanup.
func TestReconcileSidecarOrphan_PodDeletedWhenToolboxRemovedFromClass(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	tb := sidecarToolboxWithSecretInputs(t, "orphan-box", "TOKEN", "env", "token-key")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	// AgentClass initially WITH the sidecar toolbox.
	ac := classWithSidecars("ac-orphan",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "orphan-box", Ref: "orphan-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-orphan", "ac-orphan")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Seed the satisfied secret-output Secret so the pod is created immediately.
	soSecretName := secretoutsrv.SecretOutputSecretName("s-orphan")
	soSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: soSecretName, Namespace: "default"},
		Data:       map[string][]byte{"token-key": []byte("tok")},
	}
	require.NoError(t, env.Client.Create(ctx, soSecret), "create secret-output Secret")

	// First reconcile: sidecar pod created.
	reconcileToWork(t, ctx, r, "s-orphan")

	podKey := types.NamespacedName{Namespace: "default", Name: "s-orphan-sidecar-orphan-box"}
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, podKey, &pod),
		"separate sidecar pod must exist before toolbox is removed")

	// Now REMOVE the sidecar toolbox from the AgentClass (simulate mid-session
	// removal).
	var liveAC spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: "ac-orphan"}, &liveAC))
	liveAC.Spec.SidecarToolboxes = nil
	require.NoError(t, env.Client.Update(ctx, &liveAC),
		"remove sidecar toolbox from AgentClass")

	// Reconcile again: the orphaned pod should be deleted.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s-orphan"}})

	// The orphaned sidecar pod must be gone.
	var deletedPod corev1.Pod
	err := env.Client.Get(ctx, podKey, &deletedPod)
	assert.True(t, err != nil,
		"orphaned sidecar pod must be deleted when the toolbox is removed from the AgentClass: got %v", err)
}

// sidecarToolboxWithIsolation returns a minimal SidecarToolbox with NO
// SecretInputs and the given Isolation value, so RunModeFor's isolation
// branch -- not secret-gating -- is the only thing that can put it in a
// separate pod. Task 2 of the plan-0a declared-isolation brief: proving what
// spec.isolation=isolated buys on its own.
func sidecarToolboxWithIsolation(t *testing.T, name string, isolation spiceboxv1alpha1.SidecarToolboxIsolation) *spiceboxv1alpha1.SidecarToolbox {
	t.Helper()
	tb := sidecarToolbox(t, name, "", "")
	tb.Spec.Isolation = isolation
	return tb
}

// podReferencesSecret reports whether any container's EnvFrom/Env, or any
// pod volume, references the named Secret -- across every container in pod.
// Used to check a credential's REACHABILITY from a pod's spec, independent
// of whether the Secret object itself exists.
func podReferencesSecret(pod *corev1.Pod, secretName string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == secretName {
			return true
		}
	}
	for _, c := range pod.Spec.Containers {
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name == secretName {
				return true
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == secretName {
				return true
			}
		}
	}
	return false
}

// TestReconcileFieldIsolatedSidecar_CredentialStaysOutOfRunnerPod is Task 2
// Step 3 of the plan-0a declared-isolation brief. The design spec (§3 R2/R5)
// claims a separate-pod sidecar's upstreamAuth credential "stays out of the
// runner pod" -- written from reasoning, not from reading the mount path.
// This traces where the resolved credential Secret is ACTUALLY materialized
// and referenced, for a toolbox isolated by the Isolation field alone (no
// SecretInputs).
//
// The same per-session cred Secret name (cosidecar.CredentialSecretName, via
// controller.go's sidecarSecretName closure) is materialized for BOTH in-pod
// and separate-pod sidecars -- so credential separation cannot come from a
// different Secret existing. It has to come from which pod's spec actually
// references that Secret: podspec.go's BuildRunnerPod only folds
// BuildSidecarContainers' output (which envFrom's the cred Secret) into the
// runner pod for toolboxes where RunMode != RunModeSeparatePod (podspec.go
// ~line 208); an isolated toolbox is filtered out of that set before the pod
// is built, and BuildSidecarPod envFrom's the SAME Secret into the separate
// pod instead (sidecarpod.go). Separately, BuildRunnerRBAC never pins the
// runner ServiceAccount's Role to a sidecar cred Secret name at all (only
// channel and MCP credential Secrets are pinned) -- so the runner cannot
// reach it via the K8s API either.
func TestReconcileFieldIsolatedSidecar_CredentialStaysOutOfRunnerPod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	backing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "grep-backing", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("super-secret-search-token")},
	}
	require.NoError(t, env.Client.Create(ctx, backing), "create backing Secret")

	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-grep", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "grep-creds", // = <ref>-creds, matches authkind SuggestedName
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "grep-backing", Key: "token"},
				},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity")

	// Isolation=isolated with NO SecretInputs -- the field alone drives
	// RunModeSeparatePod here.
	tb := sidecarToolbox(t, "grep", "grep", "SEARCH_TOKEN")
	tb.Spec.Isolation = spiceboxv1alpha1.SidecarToolboxIsolationIsolated
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	ac := classWithSidecars("ac-grep", spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "grep", Ref: "grep"})
	ac.Spec.AgentIdentity = "ai-grep"
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-grep", "ac-grep")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	reconcileToWork(t, ctx, r, "s-grep")

	// Precondition: RunMode really is separate-pod for this toolbox (else the
	// assertions below would be vacuous).
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-grep"}, &got), "Get AgentSession")
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1)
	require.Equal(t, agentsession.RunModeSeparatePod, got.Status.ResolvedSidecarToolboxes[0].RunMode,
		"precondition: isolation=isolated with no secretInputs must resolve to separate-pod")

	// The credential Secret exists and carries the resolved token.
	credSecretName := "agentsession-s-grep-toolbox-grep"
	var credSec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: credSecretName}, &credSec),
		"per-session sidecar cred Secret should exist")
	assert.Equal(t, "super-secret-search-token", string(credSec.Data["SEARCH_TOKEN"]),
		"resolved static credential should be projected into the cred Secret")

	// Confirms the credential is not simply lost -- it is relocated to the
	// separate sidecar pod, which DOES reference it.
	sidecarPodKey := types.NamespacedName{Namespace: "default", Name: "s-grep-sidecar-grep"}
	var sidecarPod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, sidecarPodKey, &sidecarPod),
		"separate sidecar pod should exist")
	assert.True(t, podReferencesSecret(&sidecarPod, credSecretName),
		"the separate sidecar pod must envFrom its own upstreamAuth credential")

	// The runner pod is HELD until the separate sidecar pod reports Ready
	// (controller.go's sidecarPodNotReady gate) — envtest has no kubelet, so
	// simulate readiness the same way
	// TestReconcileSecretGatedSidecar_SeparatePodCreatedAndPodIPReflected does,
	// then reconcile again so the runner pod actually gets built.
	sidecarPod.Status.PodIP = "10.9.9.4"
	sidecarPod.Status.Phase = corev1.PodRunning
	sidecarPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, env.Client.Status().Update(ctx, &sidecarPod), "set separate sidecar pod Ready+PodIP")
	reconcileToWork(t, ctx, r, "s-grep")

	// Claim under test: the runner pod exists but references credSecretName
	// NOWHERE in its spec -- no container's EnvFrom/Env, no volume.
	var runnerPod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-grep-runner"}, &runnerPod),
		"runner pod should exist")
	assert.False(t, podReferencesSecret(&runnerPod, credSecretName),
		"the isolated toolbox's upstreamAuth credential must NOT be reachable from the runner pod's spec (claim 3)")
}
