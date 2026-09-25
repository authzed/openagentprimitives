// pkg/controllers/agentsession/parkedidle_detector_test.go
//
// Reconcile-level (fake-client) regression test for the 4c fall-through bug:
// step 4c (idle-sleep + archive sweep) only returned early when
// requeueAfter > 0. With archival disabled (DefaultChannelArchiveAfter == 0)
// and idle-sleep also disabled (DefaultSessionSleepAfter == 0), both
// reconcileArchive and reconcileSleep return requeueAfter==0, so the block
// fell through to steps 4d (sidecars) and 4e (content-guard detector pods)
// — re-creating pods the sleep reaper had just deleted. This is invisible
// with the operator's default archiveAfter (a few hours: requeueAfter > 0
// always), which is why the whole-branch review is what caught it, not a
// single-task review.
//
// TestParkedIdleSessionDoesNotRecreateDetectorPod pins the fix directly on
// the observable side effect the bug produces: a real Pod object (labeled
// agentprimitives.authzed.com/sidecartoolbox, the same label step 4d's
// separate-pod sidecars use) landing in the fake API store. A content-guard
// detector (step 4e) was chosen over a secret-gated sidecar (step 4d)
// because it reaches pod-creation on a single fake-client reconcile with no
// secret-output gating machinery — the existing sidecar/detector Reconcile
// tests all require envtest for that reason (see sidecar_reconcile_test.go,
// contentguarddetector_test.go), which this fixture avoids.
//
// TestPendingSessionDoesRecreateDetectorPod is the positive contrast: the
// same class/settings, but Phase=Pending (a fresh/woken boot), which does
// reach step 4e and creates the detector pod — proving the fix only gates
// the parked-Idle path, not provisioning in general.
package agentsession

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"

	// Blank import registers the prompt-injection inspector (a
	// contentguard.DetectorProvider) in the global contentguard registry via
	// init(), so contentguardregistry.Get("prompt-injection") resolves during
	// reconcile and step 4e has something to provision.
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection"
)

// detectorInspectorCfg returns the raw prompt-injection inspector config
// JSON (detectorImage + port are the only required fields).
func detectorInspectorCfg(t *testing.T) apiextv1.JSON {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"detectorImage": "ghcr.io/example/promptinjection-detector:v1",
		"port":          9080,
	})
	require.NoError(t, err, "marshal prompt-injection config")
	return apiextv1.JSON{Raw: raw}
}

// detectorClusterSettings returns the singleton ClusterAgentSettings CR
// (name "cluster") with the prompt-injection content inspector configured as
// a ceiling. settingswiring.ResolveForSession folds this into
// sess.Status.EffectiveSettings.ContentInspectors on every reconcile, which
// step 4e reads to decide whether to provision a detector pod.
func detectorClusterSettings(t *testing.T) *spiceboxv1alpha1.ClusterAgentSettings {
	t.Helper()
	return &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				ContentInspectors: &[]spiceboxv1alpha1.ContentInspectorConfig{
					{ID: "prompt-injection", Config: detectorInspectorCfg(t)},
				},
			},
		},
	}
}

// detectorPodName mirrors the naming step 4e computes for a resolved
// detector: "sidecar-<session>-<inspector id>".
func detectorPodName(sessName string) string {
	return "sidecar-" + sessName + "-prompt-injection"
}

// noBundleDetectorClass returns a Valid AgentClass with zero tool bundles and
// zero sidecar toolboxes. Zero bundles means step 2 (bundle provisioning)
// takes the vacuous NoBundles path (BundlesReady=True immediately) instead
// of gating on bundle-pod readiness the fake client can never simulate,
// which would otherwise strand a Pending-phase reconcile before it ever
// reaches step 4e — the positive-contrast case needs a clean run all the
// way through.
func noBundleDetectorClass() *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-nobundle", Namespace: "default"},
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
		},
		Status: spiceboxv1alpha1.AgentClassStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAllReferencesResolve, LastTransitionTime: metav1.Now(),
		}}},
	}
}

// newParkedIdleDetectorSession returns a channel-attached AgentSession bound
// to class, already Idle, with LastIdleAt stamped in the past and no wake
// annotation — the exact shape step 4c gates on.
func newParkedIdleDetectorSession(name, class string, now time.Time) *spiceboxv1alpha1.AgentSession {
	lastIdleAt := metav1.NewTime(now.Add(-2 * time.Minute))
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID("uid-" + name),
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  class,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "chan-1", Kind: "fake", Key: "thread-1",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:      spiceboxv1alpha1.AgentSessionPhaseIdle,
			LastIdleAt: &lastIdleAt,
		},
	}
}

// newPendingDetectorSession is the positive-contrast twin: same class/wiring
// but Phase=Pending, no LastIdleAt — the shape of a fresh or just-woken boot,
// which step 4c's `sess.Status.Phase == Idle` guard does not match at all.
func newPendingDetectorSession(name, class string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID("uid-" + name),
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  class,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "chan-1", Kind: "fake", Key: "thread-1",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhasePending,
		},
	}
}

// newDetectorReconciler wires a Reconciler over a fake client pre-loaded with
// sess, class, and the ClusterAgentSettings enabling the prompt-injection
// detector. DefaultChannelArchiveAfter/DefaultSessionSleepAfter are left at
// their zero value on purpose (matches the operator's --default-channel-
// archive-after=0 / sleep-after=0 configuration this bug requires).
func newDetectorReconciler(t *testing.T, now time.Time, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	c := buildFakeClient(t, objs...)
	r := &Reconciler{
		Client:    c,
		APIReader: c,
		Tokens:    tokens.NewRegistry(),
		Memory:    memory.NewLocal(inmem.NewBackend()),
		Now:       func() time.Time { return now },
		// Explicit zero: archival + idle-sleep both disabled, so both
		// reconcileArchive and reconcileSleep return requeueAfter==0 — the
		// exact precondition the 4c fall-through bug requires.
		DefaultChannelArchiveAfter: 0,
		DefaultSessionSleepAfter:   0,
	}
	r.RunnerFactory = &PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}
	return r, c
}

// TestParkedIdleSessionDoesNotRecreateDetectorPod pins the bug this task
// fixes: a parked Idle, channel-attached, not-woken session with archival
// AND idle-sleep both disabled (requeueAfter==0) must return from step 4c
// WITHOUT falling through to step 4e — the sleep reaper already reaped this
// session's pods, and re-creating the content-guard detector pod here would
// be a partial-sleep leak (the detector pod stays up forever while the
// runner/bundle pods stay reaped).
func TestParkedIdleSessionDoesNotRecreateDetectorPod(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	class := noBundleDetectorClass()
	sess := newParkedIdleDetectorSession("s-parked", class.Name, now)
	r, c := newDetectorReconciler(t, now, sess, class, detectorClusterSettings(t))
	ctx := memory.WithSystemApproval(context.Background(), "test")

	res, err := r.Reconcile(ctx,
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: sess.Name}})
	require.NoError(t, err, "Reconcile")
	assert.Equal(t, ctrl.Result{RequeueAfter: 0}, res,
		"a parked Idle session with archival+sleep disabled must return requeueAfter==0 from step 4c, not fall through")

	var pod corev1.Pod
	err = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: detectorPodName(sess.Name)}, &pod)
	assert.True(t, apierrors.IsNotFound(err),
		"detector pod must NOT be created for a parked Idle session (step 4c must return before step 4e)")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: sess.Name}, &got))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "stays Idle")
}

// TestPendingSessionDoesRecreateDetectorPod is the positive twin: the same
// class + ClusterAgentSettings, but Phase=Pending (a fresh/woken boot) does
// reach step 4e and creates the content-guard detector pod — proving the fix
// only gates the parked-Idle path (step 4c), not provisioning in general.
func TestPendingSessionDoesRecreateDetectorPod(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	class := noBundleDetectorClass()
	sess := newPendingDetectorSession("s-pending", class.Name)
	r, c := newDetectorReconciler(t, now, sess, class, detectorClusterSettings(t))
	ctx := memory.WithSystemApproval(context.Background(), "test")

	_, err := r.Reconcile(ctx,
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: sess.Name}})
	require.NoError(t, err, "Reconcile")

	var pod corev1.Pod
	err = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: detectorPodName(sess.Name)}, &pod)
	require.NoError(t, err, "detector pod must be created for a Pending (actively provisioning) session")
	assert.Equal(t, "prompt-injection", pod.Labels["agentprimitives.authzed.com/sidecartoolbox"],
		"detector pod carries the sidecartoolbox label by inspector ref")
}
