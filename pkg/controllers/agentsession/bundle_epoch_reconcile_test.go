// pkg/controllers/agentsession/bundle_epoch_reconcile_test.go
//
// Reconcile-level (fake-client) tests that pin the bundle/detector readiness
// deadline EPOCH: the deadline is measured from when provisioning actually
// started (the bundle SpiceboxSession / detector pod CreationTimestamp), not
// from the AgentSession's CreationTimestamp. The bug these guard against:
// a userPassthrough session whose user spent many minutes linking credentials
// (the reconcile parks in AwaitingCredentials before any bundle/detector pod is
// created) was declared BundleFailed / ContentGuardHalt even though the pods
// came up healthy in seconds — because the deadline was charged from session
// creation, which includes the interactive credential-link wait.
//
// The fake client lets us set the session's CreationTimestamp far in the past
// while a freshly-provisioned bundle/detector carries a recent CreationTimestamp
// — a separation envtest cannot express (it stamps both to "now" on Create and
// CreationTimestamp is immutable). The injected clock (r.Now) plays the role of
// reconcile-time.
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
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"

	// Registers the prompt-injection inspector (a DetectorProvider) so the
	// detector block resolves it from EffectiveSettings during reconcile.
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection"
)

// fakeReconciler builds a Reconciler over a fake client pre-loaded with objs,
// with AgentSession + SpiceboxSession status subresources registered and the
// reconcile clock pinned to clock(). RunnerFactory is wired (unused before the
// bundle/detector gates the tests exercise).
func fakeReconciler(t *testing.T, clock func() time.Time, objs ...client.Object) (*agentsession.Reconciler, client.Client) {
	t.Helper()
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme, networkingv1.AddToScheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SpiceboxSession{}).
		// The detector path server-side-applies its deny-all-egress
		// NetworkPolicy (ensureDetectorNetworkPolicy), like every other
		// per-session policy. The fake client's DEFAULT converter pair rejects
		// that apply with "expected objects with types from the same schema";
		// the deduced converter handles it. A fake-client limitation only — the
		// same applies work against a real apiserver (netpol_envtest_test.go).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		Build()
	r := &agentsession.Reconciler{
		Client:    c,
		APIReader: c,
		Tokens:    tokens.NewRegistry(),
		Memory:    memory.NewLocal(inmem.NewBackend()),
		Now:       clock,
	}
	r.RunnerFactory = &agentsession.PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}
	return r, c
}

// classWithBundle returns a Valid AgentClass (identityMode=agent) with one tool
// bundle, so the reconcile provisions a bundle SpiceboxSession.
func classWithBundle(name string, bundles ...spiceboxv1alpha1.ToolBundle) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeAgent,
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 50, MaxTokens: 100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
			ToolBundles: bundles,
		},
		Status: spiceboxv1alpha1.AgentClassStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAllReferencesResolve, LastTransitionTime: metav1.Now(),
		}}},
	}
}

// sessionCreatedAt returns an AgentSession whose CreationTimestamp is fixed in
// the past, simulating a session that existed long before its bundles/detectors
// began provisioning (e.g. a passthrough session that waited in
// AwaitingCredentials while the user linked accounts).
func sessionCreatedAt(name, class string, created time.Time) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  class,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
		},
	}
}

// notReadyBundle returns a bundle SpiceboxSession (named "<session>-<bundle>")
// with a fixed CreationTimestamp and no Ready condition — the not-ready,
// not-failed state that drives the deadline backstop.
func notReadyBundle(sessName, bundleName string, created time.Time) *spiceboxv1alpha1.SpiceboxSession {
	return &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: sessName + "-" + bundleName, Namespace: "default",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{Class: "toolbelt"},
	}
}

func runReconciles(t *testing.T, r *agentsession.Reconciler, name string, n int) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}); err != nil {
			t.Fatalf("reconcile pass %d: %v", i, err)
		}
	}
}

// TestReconcileBundleDeadlineEpoch covers both halves of the bundle backstop's
// new epoch: a young (recently-provisioned) bundle on an old session must NOT
// fail, while a bundle whose own provisioning has genuinely exceeded the
// deadline still fails closed (the backstop is preserved, not removed).
func TestReconcileBundleDeadlineEpoch(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	// Reconcile-time. The session is created 12m before this; bundles vary.
	nowT := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	sessCreated := nowT.Add(-12 * time.Minute)

	cases := []struct {
		name          string
		bundleCreated time.Time
		wantFailed    bool
	}{
		{
			name:          "12m-old session, bundle provisioning 10s ago: NOT BundleFailed (requeues)",
			bundleCreated: nowT.Add(-10 * time.Second),
			wantFailed:    false,
		},
		{
			name:          "bundle provisioning itself exceeded the deadline: BundleFailed (backstop preserved)",
			bundleCreated: nowT.Add(-9 * time.Minute), // > bundleReadyDeadline (8m)
			wantFailed:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
			sess := sessionCreatedAt("s1", "ac1", sessCreated)
			bundle := notReadyBundle("s1", "code", tc.bundleCreated)
			r, c := fakeReconciler(t, clock, ac, sess, bundle)

			runReconciles(t, r, "s1", 5)

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s1"}, &got))

			failed := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
			if !tc.wantFailed {
				assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
					"session must NOT be Failed when its bundle only began provisioning seconds ago")
				if failed != nil {
					assert.NotEqual(t, metav1.ConditionTrue, failed.Status, "Failed condition must not be True")
				}
				bundlesReady := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionBundlesReady)
				require.NotNil(t, bundlesReady, "BundlesReady condition must be set while waiting")
				assert.Equal(t, metav1.ConditionFalse, bundlesReady.Status)
				assert.Equal(t, "BundlesProvisioning", bundlesReady.Reason)
				return
			}
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "session must be Failed once bundle provisioning exceeds the deadline")
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBundleFail, got.Status.FailureReason)
			require.NotNil(t, failed, "Failed condition must be set")
			assert.Equal(t, metav1.ConditionTrue, failed.Status)
			// Honest, actionable message: names the bundle, blames image-pull/crash
			// (NOT scheduling — provablyUnschedulableBundle already ruled that out).
			assert.Contains(t, failed.Message, "s1-code", "message names the offending bundle")
			assert.Contains(t, failed.Message, "image pull", "message points at the likely cause (image pull / crash)")
			assert.NotContains(t, failed.Message, "unschedulable", "message must not blame scheduling (already fast-failed)")
		})
	}
}

// detectorSettings returns a singleton ClusterAgentSettings with a
// prompt-injection content inspector (a DetectorProvider), so reconcile
// resolves a detector pod into status.
func detectorSettings(t *testing.T) *spiceboxv1alpha1.ClusterAgentSettings {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{
		"detectorImage": "ghcr.io/example/promptinjection-detector:v1",
		"port":          int32(9080),
	})
	require.NoError(t, err, "marshal prompt-injection config")
	inspectors := []spiceboxv1alpha1.ContentInspectorConfig{
		{ID: "prompt-injection", Config: apiextv1.JSON{Raw: cfg}},
	}
	return &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{ContentInspectors: &inspectors},
		},
	}
}

// notReadyDetectorPod returns the detector pod the reconcile would create
// ("sidecar-<session>-<inspector>"), pre-seeded with a fixed CreationTimestamp
// and no Ready/PodIP — the not-ready, non-terminal state that drives the
// detector deadline backstop.
func notReadyDetectorPod(sessName, inspector string, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sidecar-" + sessName + "-" + inspector, Namespace: "default",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "detector", Image: "ghcr.io/example/promptinjection-detector:v1"}}},
	}
}

// TestReconcileDetectorDeadlineEpoch is the detector twin of
// TestReconcileBundleDeadlineEpoch: a detector pod provisioned seconds ago on a
// 12m-old session must NOT be failed closed (ContentGuardHalt), while a detector
// whose own provisioning exceeded detectorReadyDeadline still fails closed.
func TestReconcileDetectorDeadlineEpoch(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	nowT := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }
	sessCreated := nowT.Add(-12 * time.Minute)

	cases := []struct {
		name            string
		detectorCreated time.Time
		wantHalt        bool
	}{
		{
			name:            "12m-old session, detector provisioning 10s ago: NOT ContentGuardHalt (awaits)",
			detectorCreated: nowT.Add(-10 * time.Second),
			wantHalt:        false,
		},
		{
			name:            "detector provisioning itself exceeded the deadline: ContentGuardHalt (backstop preserved)",
			detectorCreated: nowT.Add(-6 * time.Minute), // > detectorReadyDeadline (5m)
			wantHalt:        true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No bundles, so the reconcile proceeds straight to the detector gate.
			ac := classWithBundle("acd")
			sess := sessionCreatedAt("sd", "acd", sessCreated)
			det := notReadyDetectorPod("sd", "prompt-injection", tc.detectorCreated)
			r, c := fakeReconciler(t, clock, ac, sess, detectorSettings(t), det)

			runReconciles(t, r, "sd", 5)

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "sd"}, &got))

			if !tc.wantHalt {
				assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
					"session must NOT be Failed when its detector only began provisioning seconds ago")
				runnerReady := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
				require.NotNil(t, runnerReady, "RunnerReady condition must be set while awaiting detector")
				assert.Equal(t, metav1.ConditionFalse, runnerReady.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector, runnerReady.Reason)
				return
			}
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "session must be Failed once detector provisioning exceeds the deadline")
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionContentGuardHalt, got.Status.FailureReason)
			failed := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
			require.NotNil(t, failed, "Failed condition must be set")
			assert.Equal(t, metav1.ConditionTrue, failed.Status)
			assert.Contains(t, failed.Message, "prompt-injection", "message names the offending inspector")
		})
	}
}
