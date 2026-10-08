//go:build integration

// pkg/controllers/agentsession/controller_test.go
package agentsession_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// testReaders builds guarded Secret/ConfigMap readers for tests: Warn mode
// (never panic in tests) over the envtest client, with a permissive allowlist
// (test objects are operator-created+labeled or allowlisted infra).
func testReaders(env *testenv.Env) (*adoptguard.SecretReader, *adoptguard.ConfigMapReader) {
	allow := func(types.NamespacedName) bool { return true }
	return adoptguard.NewSecretReader(env.Client, env.Client, adoptguard.Warn, allow),
		adoptguard.NewConfigMapReader(env.Client, env.Client, adoptguard.Warn, allow)
}

func newReconciler(t *testing.T, env *testenv.Env) *agentsession.Reconciler {
	t.Helper()
	sr, cmr := testReaders(env)
	r := &agentsession.Reconciler{
		Client:          env.Client,
		APIReader:       env.Client,
		Tokens:          tokens.NewRegistry(),
		Memory:          memory.NewLocal(inmem.NewBackend()),
		SecretReader:    sr,
		ConfigMapReader: cmr,
	}
	r.RunnerFactory = &agentsession.PodRunnerFactory{
		Client:       env.Client,
		RunnerImage:  "agentprimitives-runner:dev",
		OperatorURL:  "http://op:8082",
		NATSURL:      "nats://spicebox-nats.agentprimitives-system.svc:4222",
		SecretReader: sr,
	}
	return r
}

func newReconcilerWithArchive(t *testing.T, env *testenv.Env, archiveAfter time.Duration) *agentsession.Reconciler {
	t.Helper()
	r := newReconciler(t, env)
	r.DefaultChannelArchiveAfter = archiveAfter
	return r
}

func validClass(name string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
		},
	}
}

func validSession(name, class string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  class,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
		},
	}
}

func markValid(t *testing.T, env *testenv.Env, ac *spiceboxv1alpha1.AgentClass) {
	t.Helper()
	ac.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentClassConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAllReferencesResolve,
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(memory.WithSystemApproval(context.Background(), "test"), ac),
		"stamp AgentClass %s Valid=True", ac.Name)
}

func TestReconcileCreatesRBACAndPod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	ac := validClass("ac1")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s1", "ac1")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Two reconciles: first installs finalizer, second does the work.
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}})
	}

	// SA exists.
	var sa corev1.ServiceAccount
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s1-runner-sa"}, &sa),
		"runner ServiceAccount should exist")
	// Role + RB exist.
	var role rbacv1.Role
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s1-runner"}, &role),
		"runner Role should exist")
	// Memory-token Secret exists.
	var sec corev1.Secret
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s1-memory-token"}, &sec),
		"memory-token Secret should exist")
	assert.NotEmpty(t, sec.Data["token"], "memory-token Secret should contain a token")
	// Runner Pod exists.
	var pod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s1-runner"}, &pod),
		"runner Pod should exist")
	// AgentSession status: ClassResolved=True, BundlesReady=True.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s1"}, &got),
		"Get AgentSession")
	classResolved := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionClassResolved)
	require.NotNil(t, classResolved, "ClassResolved condition should be set")
	assert.Equal(t, metav1.ConditionTrue, classResolved.Status, "ClassResolved status")
	bundlesReady := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionBundlesReady)
	require.NotNil(t, bundlesReady, "BundlesReady condition should be set")
	assert.Equal(t, metav1.ConditionTrue, bundlesReady.Status, "BundlesReady status")
}

func TestReconcileWaitsForClassValid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	ac := validClass("ac2") // not marked valid
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass (not yet Valid)")
	sess := validSession("s2", "ac2")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s2"}})
	}
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s2"}, &got),
		"Get AgentSession")
	// ClassResolved should be False because the class is not Valid.
	c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionClassResolved)
	require.NotNil(t, c, "ClassResolved condition")
	assert.Equal(t, metav1.ConditionFalse, c.Status, "ClassResolved should be False until AgentClass is Valid")
}

func TestFinalizerCleansUpMemory(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	ac := validClass("ac3")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	sess := validSession("s3", "ac3")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Capture the actual token; assert it resolves now.
	var sec corev1.Secret
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s3-memory-token"}, &sec),
		"Get memory-token Secret")
	tok := string(sec.Data["token"])
	require.NotEmpty(t, tok, "memory-token Secret data should be populated")
	_, ok := r.Tokens.Lookup(tok)
	require.True(t, ok, "token should be in registry after setup reconciles")

	// Delete the AgentSession; reconcile through the finalizer path.
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	require.NoError(t, env.Client.Delete(ctx, &fresh), "delete AgentSession")
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Token should now be revoked.
	_, ok = r.Tokens.Lookup(tok)
	assert.False(t, ok, "token should be revoked from the registry after finalize")
}

func TestRunnerCrashThresholdMarksFailed(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	ac := validClass("ac4")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)
	sess := validSession("s4", "ac4")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Simulate a runner Pod with 5 container restarts (above threshold of 3).
	var pod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s4-runner"}, &pod),
		"Get runner Pod")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runner", Ready: false, RestartCount: 5,
	}}
	require.NoError(t, env.Client.Status().Update(ctx, &pod), "update runner Pod status to crash-looping")

	// Trigger another reconcile pass.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "phase after runner crash")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionRunnerCrash, got.Status.FailureReason, "failureReason")
}

func TestReconcileCreatesBundleSpiceboxSessionAndWaits(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	// Pre-create the upstream toolspec/class/identity referenced by the bundle.
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "toolbelt"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "echo", Command: []string{"/bin/echo"}}},
		},
	}), "create SpiceboxClass")
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id1", Namespace: "default"},
	}), "create AgentIdentity")
	ac := validClass("ac-b")
	ac.Spec.AgentIdentity = "id1"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "toolbelt", Toolspecs: []string{}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("sb", "ac-b")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	for i := 0; i < 5; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Bundle SpiceboxSession was created.
	var bs spiceboxv1alpha1.SpiceboxSession
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-code"}, &bs),
		"bundle SpiceboxSession should be created")
	// AgentSession's BundlesReady is False until the SpiceboxSession is Ready.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &got),
		"Get AgentSession")
	c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionBundlesReady)
	require.NotNil(t, c, "BundlesReady condition")
	assert.Equal(t, metav1.ConditionFalse, c.Status,
		"BundlesReady should be False before bundle SpiceboxSession is Ready")

	// Now mark the SpiceboxSession Ready and re-reconcile.
	bs.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.SpiceboxSessionConditionReady, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonPodReady, LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, &bs), "stamp SpiceboxSession Ready=True")
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &got),
		"Get AgentSession after bundle Ready")
	c = meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionBundlesReady)
	require.NotNil(t, c, "BundlesReady condition (after bundle Ready)")
	assert.Equal(t, metav1.ConditionTrue, c.Status,
		"BundlesReady should be True once bundle SpiceboxSession is Ready")
	require.Len(t, got.Status.BundleSessions, 1, "BundleSessions: %+v", got.Status.BundleSessions)
	assert.Equal(t, "sb-code", got.Status.BundleSessions[0].SpiceboxSessionName, "BundleSession name")
}

// eventually polls fn every 50ms until it returns true or d elapses, failing
// the test on timeout. Mirrors pkg/controllers/agentidentity's helper of the
// same name/signature.
func eventually(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition never became true within %v", d)
}

func conditionByType(s *spiceboxv1alpha1.AgentSession, condType string) *metav1.Condition {
	for i := range s.Status.Conditions {
		if s.Status.Conditions[i].Type == condType {
			return &s.Status.Conditions[i]
		}
	}
	return nil
}

// stampBundleFailed marks the named bundle SpiceboxSession Failed=PodCrashed,
// mirroring what the SpiceboxSession controller writes when its sandbox
// backend reports sandboxkinds.PhaseFailed. Returns the stamped object's UID
// so tests can distinguish the original instance from its replacement.
func stampBundleFailed(t *testing.T, env *testenv.Env, name string) types.UID {
	t.Helper()
	ctx := context.Background()
	var bs spiceboxv1alpha1.SpiceboxSession
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &bs),
		"bundle SpiceboxSession %s should exist", name)
	bs.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.SpiceboxSessionConditionFailed, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonPodCrashed, Message: "Pod entered Failed phase",
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, &bs), "stamp bundle Failed=True")
	return bs.UID
}

// TestReconcileBundleFailureRetriesOnceThenTerminal exercises the retry
// ladder end to end against a real apiserver: first bundle failure →
// delete + recreate (session stays alive, restarts=1 recorded); second
// failure on the REPLACEMENT instance → terminal BundleFailed.
func TestReconcileBundleFailureRetriesOnceThenTerminal(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id-retry", Namespace: "default"},
	}), "create AgentIdentity")
	ac := validClass("ac-retry")
	ac.Spec.AgentIdentity = "id-retry"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "toolbelt", Toolspecs: []string{}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("sb-retry", "ac-retry")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	key := client.ObjectKeyFromObject(sess)
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	// --- Failure #1: retry, not terminal. ---
	firstUID := stampBundleFailed(t, env, "sb-retry-code")
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &got), "Get AgentSession after first failure")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"first bundle failure must NOT terminally fail the session")
	require.Len(t, got.Status.BundleSessions, 1, "bundle status entry")
	assert.Equal(t, int32(1), got.Status.BundleSessions[0].Restarts, "retry recorded")
	assert.Equal(t, string(firstUID), got.Status.BundleSessions[0].RetriedSessionUID,
		"retried instance UID recorded")

	// The loop's SSA apply recreates the bundle CR — wait for the fresh
	// instance (new UID). No spiceboxsession reconciler runs here, so no
	// finalizer delays the delete.
	var fresh spiceboxv1alpha1.SpiceboxSession
	eventually(t, 10*time.Second, func() bool {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-retry-code"}, &fresh); err != nil {
			return false
		}
		return fresh.UID != firstUID
	})

	// --- Failure #2 (replacement instance): terminal. ---
	stampBundleFailed(t, env, "sb-retry-code")
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	require.NoError(t, env.Client.Get(ctx, key, &got), "Get AgentSession after second failure")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"second bundle failure must terminally fail the session")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBundleFail, got.Status.FailureReason, "failureReason")
	failedCond := conditionByType(&got, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, failedCond, "Failed condition present")
	assert.Contains(t, failedCond.Message, "PodCrashed", "message carries the real cause")
	assert.Contains(t, failedCond.Message, "after 1 retry", "message notes the retry happened")
}

// TestReconcileBundleFailureRecoversAfterRetry: failure #1 → retry; the
// replacement goes Ready → session proceeds, no terminal failure, the
// retry count stays recorded.
func TestReconcileBundleFailureRecoversAfterRetry(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id-recover", Namespace: "default"},
	}), "create AgentIdentity")
	ac := validClass("ac-recover")
	ac.Spec.AgentIdentity = "id-recover"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "toolbelt", Toolspecs: []string{}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("sb-recover", "ac-recover")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	key := client.ObjectKeyFromObject(sess)
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	firstUID := stampBundleFailed(t, env, "sb-recover-code")
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	// Wait for the replacement CR, then mark it Ready.
	var fresh spiceboxv1alpha1.SpiceboxSession
	eventually(t, 10*time.Second, func() bool {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-recover-code"}, &fresh); err != nil {
			return false
		}
		return fresh.UID != firstUID
	})
	fresh.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.SpiceboxSessionConditionReady, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonPodReady, LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, &fresh), "mark replacement Ready")

	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &got), "Get AgentSession after recovery")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"recovered session must not be Failed")
	require.Len(t, got.Status.BundleSessions, 1)
	assert.Equal(t, int32(1), got.Status.BundleSessions[0].Restarts,
		"retry count persists after recovery")
}

func TestReconcileIdleNoRespawn(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	// AgentClass with channel config.
	ac := validClass("ac-idle")
	ac.Spec.Channels = &spiceboxv1alpha1.ChannelsConfig{
		IdleTTL: metav1.Duration{Duration: 5 * time.Minute},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	// Channel-attached session, already in phase=Idle (set via status subresource
	// after creation).
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "idle1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "ac-idle",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name:              "slack-ch",
				Kind:              "slack",
				Key:               "dm:U123",
				Capabilities:      []string{"text"},
				NATSSubjectPrefix: "ap.session.default.idle1",
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create channel-attached AgentSession")

	// First reconcile pass: installs finalizer + RBAC + creates a pod
	// (session is not yet Idle at this point — status is empty).
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Simulate channelsd/runner writing phase=Idle to status.
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	fresh.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
	require.NoError(t, env.Client.Status().Update(ctx, &fresh), "set phase=Idle")

	// Delete the existing runner pod to simulate it having exited.
	var pod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "idle1-runner"}, &pod),
		"Get runner Pod")
	require.NoError(t, env.Client.Delete(ctx, &pod), "delete runner Pod")

	// Reconcile: must NOT recreate the pod because phase=Idle.
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Assert: no pod was recreated.
	var newPod corev1.Pod
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "idle1-runner"}, &newPod)
	assert.Error(t, err, "runner pod should NOT be recreated when phase=Idle")

	// Assert: phase remains Idle.
	var result spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &result), "Get AgentSession")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, result.Status.Phase, "phase remains Idle")
}

func TestReconcileWakeAnnotationRespawns(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	// AgentClass with channel config.
	ac := validClass("ac-wake")
	ac.Spec.Channels = &spiceboxv1alpha1.ChannelsConfig{
		IdleTTL: metav1.Duration{Duration: 5 * time.Minute},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	// Channel-attached session.
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "wake1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "ac-wake",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name:              "slack-ch",
				Kind:              "slack",
				Key:               "dm:U456",
				Capabilities:      []string{"text"},
				NATSSubjectPrefix: "ap.session.default.wake1",
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create channel-attached AgentSession")

	// Bootstrap: install finalizer + RBAC + initial pod.
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Transition session to Idle (simulate runner exiting cleanly).
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	fresh.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
	require.NoError(t, env.Client.Status().Update(ctx, &fresh), "set phase=Idle")

	// Capture current LastWakeAt so we can assert wake processing ran.
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh),
		"refresh AgentSession to capture LastWakeAt")
	lastWakeBefore := fresh.Status.LastWakeAt

	// Delete the existing runner pod (it exited cleanly → kubelet removed it, or
	// the test needs a clean slate for the pod-creation assertion).
	var pod corev1.Pod
	_ = env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "wake1-runner"}, &pod)
	_ = env.Client.Delete(ctx, &pod)

	// Reconcile with Idle+no annotation → pod must NOT be recreated.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	var podCheck corev1.Pod
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "wake1-runner"}, &podCheck)
	assert.Error(t, err, "pod should not be recreated without wake annotation")

	// Now patch the wake annotation.
	wakeTime := time.Now().UTC().Format(time.RFC3339Nano)
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh),
		"refresh AgentSession before patching wake annotation")
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = wakeTime
	require.NoError(t, env.Client.Update(ctx, &fresh), "patch wake annotation")

	// Reconcile: shouldWake should detect the annotation and transition to Pending.
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got),
		"Get AgentSession after wake")

	// Phase must have advanced past Idle (Pending or further if reconcile ran
	// the pod-creation path too).
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase,
		"phase should advance past Idle after wake")

	// LastWakeAt must be set.
	require.NotNil(t, got.Status.LastWakeAt, "status.lastWakeAt should be set after wake")

	// LastWakeAt must have advanced (wake-from-Idle stamps it).
	if lastWakeBefore != nil {
		assert.True(t, got.Status.LastWakeAt.After(lastWakeBefore.Time),
			"LastWakeAt did not advance: before=%v after=%v", lastWakeBefore, got.Status.LastWakeAt)
	}

	// A fresh runner pod must be created.
	var newPod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "wake1-runner"}, &newPod),
		"runner Pod should be recreated after wake")
}

// channelSession builds a channel-attached AgentSession for archive tests.
func channelSession(name, class string) *spiceboxv1alpha1.AgentSession {
	s := validSession(name, class)
	s.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name:              "slack-ch",
		Kind:              "slack",
		Key:               "dm:U999",
		Capabilities:      []string{"text"},
		NATSSubjectPrefix: "ap.session.default." + name,
	}
	return s
}

// classWithArchiveAfter returns a valid AgentClass with ArchiveAfter configured.
func classWithArchiveAfter(name string, archiveAfter time.Duration) *spiceboxv1alpha1.AgentClass {
	ac := validClass(name)
	ac.Spec.Channels = &spiceboxv1alpha1.ChannelsConfig{
		IdleTTL:      metav1.Duration{Duration: 5 * time.Minute},
		ArchiveAfter: metav1.Duration{Duration: archiveAfter},
	}
	return ac
}

// setIdlePhaseWithTimestamp seeds the session's status to phase=Idle with a
// specific LastIdleAt timestamp so that archive deadline tests are reproducible.
// Seeds via SSA under the operator field manager ("agentsession") so the
// operator owns LastIdleAt and can clear it by omission on wake.
func setIdlePhaseWithTimestamp(t *testing.T, env *testenv.Env, sess *spiceboxv1alpha1.AgentSession, idleAt time.Time) {
	t.Helper()
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t,
		env.Client.Get(memory.WithSystemApproval(context.Background(), "test"), client.ObjectKeyFromObject(sess), &fresh),
		"get session %s for Idle stamp", sess.Name)
	ts := metav1.NewTime(idleAt)
	apply := &spiceboxv1alpha1.AgentSession{
		TypeMeta:   metav1.TypeMeta{APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(), Kind: "AgentSession"},
		ObjectMeta: metav1.ObjectMeta{Name: fresh.Name, Namespace: fresh.Namespace},
	}
	apply.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
	apply.Status.LastIdleAt = &ts
	require.NoError(t, env.Client.Status().Patch(memory.WithSystemApproval(context.Background(), "test"), apply,
		client.Apply, client.ForceOwnership, client.FieldOwner("agentsession")),
		"seed Idle+LastIdleAt via operator field manager on %s", sess.Name)
}

// bootstrapSession creates the class + session, marks the class valid, and runs
// enough reconciles to install the finalizer + RBAC + initial pod.
func bootstrapSession(t *testing.T, env *testenv.Env, r *agentsession.Reconciler, ac *spiceboxv1alpha1.AgentClass, sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass %s", ac.Name)
	markValid(t, env, ac)
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession %s", sess.Name)
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}
}

// TestStampsLastIdleAtOnIdleEntry verifies that the reconciler stamps
// LastIdleAt when the session first enters phase=Idle.
func TestStampsLastIdleAtOnIdleEntry(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	// Use archiveAfter=0 so the archive sweep is disabled and won't interfere.
	r := newReconcilerWithArchive(t, env, 0)

	ac := classWithArchiveAfter("ac-stamp", 0)
	sess := channelSession("stamp1", "ac-stamp")
	bootstrapSession(t, env, r, ac, sess)

	// Simulate runner writing phase=Idle (LastIdleAt not yet set by runner).
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	fresh.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
	fresh.Status.LastIdleAt = nil
	require.NoError(t, env.Client.Status().Update(ctx, &fresh), "set phase=Idle without LastIdleAt")

	// Reconcile: should stamp LastIdleAt.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	assert.NotNil(t, got.Status.LastIdleAt, "LastIdleAt should be stamped on Idle entry")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "phase remains Idle")
}

// TestStampsLastIdleAtIsIdempotent verifies that the stamp is not overwritten
// on subsequent reconciles when the session stays in phase=Idle.
func TestStampsLastIdleAtIsIdempotent(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconcilerWithArchive(t, env, 0)

	ac := classWithArchiveAfter("ac-idem", 0)
	sess := channelSession("idem1", "ac-idem")
	bootstrapSession(t, env, r, ac, sess)

	// Set Idle with a known LastIdleAt in the past.
	past := time.Now().Add(-10 * time.Minute)
	setIdlePhaseWithTimestamp(t, env, sess, past)

	// Reconcile twice: stamp must not change.
	for i := 0; i < 2; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	require.NotNil(t, got.Status.LastIdleAt, "LastIdleAt should remain set")
	// Timestamp must not have advanced: the stamp must stay at (roughly) past.
	assert.False(t, got.Status.LastIdleAt.Time.After(past.Add(time.Second)),
		"LastIdleAt moved forward: was %v, got %v", past, got.Status.LastIdleAt.Time)
}

// TestClearsLastIdleAtOnIdleExit verifies that LastIdleAt is cleared when a
// wake annotation transitions the session from Idle to Pending.
func TestClearsLastIdleAtOnIdleExit(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconcilerWithArchive(t, env, 0)

	ac := classWithArchiveAfter("ac-clear", 0)
	sess := channelSession("clear1", "ac-clear")
	bootstrapSession(t, env, r, ac, sess)

	// Set Idle with a LastIdleAt.
	setIdlePhaseWithTimestamp(t, env, sess, time.Now().Add(-5*time.Minute))

	// Delete the existing pod so the wake reconcile won't trip on pod-create logic.
	var pod corev1.Pod
	_ = env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "clear1-runner"}, &pod)
	_ = env.Client.Delete(ctx, &pod)

	// Add wake annotation.
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().UTC().Format(time.RFC3339Nano)
	require.NoError(t, env.Client.Update(ctx, &fresh), "patch wake annotation")

	// Reconcile: shouldWake fires, LastIdleAt must be cleared in the same update.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	assert.Nil(t, got.Status.LastIdleAt, "LastIdleAt should be nil after wake")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase,
		"phase should advance from Idle after wake")
}

// TestArchiveSweepTransitionsAfterDeadline verifies that a session idle longer
// than archiveAfter is transitioned to Succeeded with Reason=Archived.
func TestArchiveSweepTransitionsAfterDeadline(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	// Operator default 4h; class will also specify 4h.
	r := newReconcilerWithArchive(t, env, 4*time.Hour)

	ac := classWithArchiveAfter("ac-arch", 4*time.Hour)
	sess := channelSession("arch1", "ac-arch")
	bootstrapSession(t, env, r, ac, sess)

	// LastIdleAt = now-5h → past the 4h deadline.
	setIdlePhaseWithTimestamp(t, env, sess, time.Now().Add(-5*time.Hour))

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, got.Status.Phase, "phase after archive")
	c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionIdle)
	require.NotNil(t, c, "Idle condition should be set after archive")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionArchived, c.Reason, "Idle condition reason")
	assert.NotNil(t, got.Status.FinishedAt, "FinishedAt should be set after archive")
	assert.Nil(t, got.Status.LastIdleAt, "LastIdleAt should be cleared after archive")
}

// TestArchiveSweepRequeuesBeforeDeadline verifies that a session idle for less
// than archiveAfter is NOT archived and the reconciler returns a RequeueAfter.
func TestArchiveSweepRequeuesBeforeDeadline(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconcilerWithArchive(t, env, 4*time.Hour)

	ac := classWithArchiveAfter("ac-rq", 4*time.Hour)
	sess := channelSession("rq1", "ac-rq")
	bootstrapSession(t, env, r, ac, sess)

	// LastIdleAt = now → well before the 4h deadline.
	setIdlePhaseWithTimestamp(t, env, sess, time.Now())

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	require.NoError(t, err, "Reconcile (idle, before archive deadline)")
	// Should requeue somewhere around 4h (allow ±10s for test execution jitter).
	assert.GreaterOrEqual(t, result.RequeueAfter, 3*time.Hour+59*time.Minute,
		"RequeueAfter should be ~4h (lower bound)")
	assert.LessOrEqual(t, result.RequeueAfter, 4*time.Hour+time.Second,
		"RequeueAfter should be ~4h (upper bound)")

	// Phase must stay Idle.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase,
		"phase remains Idle (not yet past archive deadline)")
}

// TestArchiveAfterZeroDisables verifies that setting archiveAfter=0 (operator
// default 0, no class override) disables archival entirely.
func TestArchiveAfterZeroDisables(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	// Operator default 0 → disabled.
	r := newReconcilerWithArchive(t, env, 0)

	// Class also has ArchiveAfter=0 (zero value = not set).
	ac := classWithArchiveAfter("ac-zero", 0)
	sess := channelSession("zero1", "ac-zero")
	bootstrapSession(t, env, r, ac, sess)

	// Set Idle with a very old LastIdleAt.
	setIdlePhaseWithTimestamp(t, env, sess, time.Now().Add(-72*time.Hour))

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	require.NoError(t, err, "Reconcile (archive disabled)")
	// archive sweep is a no-op (archiveAfter=0); session falls through to
	// the terminal-pod-lifecycle short-circuit which returns Result{} with
	// no RequeueAfter.
	assert.Equal(t, time.Duration(0), result.RequeueAfter,
		"RequeueAfter should be 0 when archive is disabled")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase,
		"phase remains Idle (archive disabled)")
}

// TestArchiveSweepUsesClassOverride verifies that AgentClass.spec.channels.archiveAfter
// takes precedence over the operator default when non-zero.
func TestArchiveSweepUsesClassOverride(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	// Operator default is 4h, but class specifies 1m.
	r := newReconcilerWithArchive(t, env, 4*time.Hour)

	ac := classWithArchiveAfter("ac-override", time.Minute)
	sess := channelSession("override1", "ac-override")
	bootstrapSession(t, env, r, ac, sess)

	// LastIdleAt = now-2m → past the class-overridden 1m deadline but NOT past the 4h default.
	setIdlePhaseWithTimestamp(t, env, sess, time.Now().Add(-2*time.Minute))

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, got.Status.Phase,
		"phase should be Succeeded (class 1m override past deadline)")
}

// TestWakeAnnotationOverridesArchiveSweep verifies that an Idle session with
// a fresh wake annotation is woken (Idle → Pending) on the same reconcile —
// the archive sweep MUST NOT short-circuit with a far-future RequeueAfter
// when shouldWake is true. Pre-fix, the archive sweep ran first and returned
// RequeueAfter=archiveAfter (e.g., 4h), so the wake-annotation block never
// executed and the runner never respawned: the session sat on "starting…"
// forever from the user's POV until the deadline.
func TestWakeAnnotationOverridesArchiveSweep(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconcilerWithArchive(t, env, 4*time.Hour)

	ac := classWithArchiveAfter("ac-wake-arch", 4*time.Hour)
	sess := channelSession("wake-arch", "ac-wake-arch")
	bootstrapSession(t, env, r, ac, sess)

	// Idle for 1m — well within archiveAfter (4h). Pre-fix this would push
	// the reconciler down the archive path with a ~4h requeue.
	setIdlePhaseWithTimestamp(t, env, sess, time.Now().Add(-1*time.Minute))

	// Fresh wake annotation arrives.
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().UTC().Format(time.RFC3339Nano)
	require.NoError(t, env.Client.Update(ctx, &fresh), "patch wake annotation")

	// Reconcile a few times — first triggers the wake transition, subsequent
	// ones may run the pod-creation path.
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase,
		"phase should advance past Idle after wake — archive sweep must yield to wake")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, got.Status.Phase,
		"archive sweep must not archive a session with a fresh wake annotation")
	assert.NotNil(t, got.Status.LastWakeAt, "LastWakeAt should be set after wake transition")
}

// TestArchivedSessionWakeResumesToPending proves the core invariant: a session
// the archive sweep parked at Succeeded is asleep, not finished, so a fresh
// inbound resumes it to Pending — it does NOT stay terminal. This exercises the
// real archive path (Idle → Succeeded with the Archived condition reason +
// FinishedAt) and then a wake annotation, asserting the folded lifecycle actually
// projects Pending back onto the CR (no direct phase write in the controller).
func TestArchivedSessionWakeResumesToPending(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	// archiveAfter=1m so a session idle 10m ago sweeps to Succeeded this reconcile.
	r := newReconcilerWithArchive(t, env, 1*time.Minute)

	ac := classWithArchiveAfter("ac-arch-resume", 1*time.Minute)
	sess := channelSession("arch-resume", "ac-arch-resume")
	bootstrapSession(t, env, r, ac, sess)

	// Park it: idle 10m ago is well past the 1m archive deadline.
	setIdlePhaseWithTimestamp(t, env, sess, time.Now().Add(-10*time.Minute))
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	// Precondition: the sweep parked the session at Succeeded, recorded itself as
	// the Idle condition reason, and stamped FinishedAt.
	var archived spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &archived), "Get after archive sweep")
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, archived.Status.Phase, "archive sweep parks at Succeeded")
	require.True(t, spiceboxv1alpha1.ArchivedBySweep(&archived), "sweep records the Archived condition reason")
	require.NotNil(t, archived.Status.FinishedAt, "archive stamps FinishedAt")

	// A fresh inbound writes the wake-requested annotation onto the parked session.
	if archived.Annotations == nil {
		archived.Annotations = map[string]string{}
	}
	archived.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().UTC().Format(time.RFC3339Nano)
	require.NoError(t, env.Client.Update(ctx, &archived), "patch wake annotation onto archived session")

	// Reconcile: the archived-Succeeded session must resume rather than stay terminal.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get after wake")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, got.Status.Phase,
		"an archive-swept session must resume to Pending on a fresh inbound, not stay Succeeded")
	assert.Nil(t, got.Status.FinishedAt,
		"un-archiving clears FinishedAt so a resumed session no longer looks completed")
}

// TestWakeDeletesStaleCompletedRunnerPod verifies that the wake handler
// deletes a Completed runner pod from the previous turn before transitioning
// to Pending. Without this, the pod-creation path returns AlreadyExists for
// the stale pod and a fresh runner is never spawned.
func TestWakeDeletesStaleCompletedRunnerPod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconcilerWithArchive(t, env, 4*time.Hour)

	ac := classWithArchiveAfter("ac-wake-stale", 4*time.Hour)
	sess := channelSession("wake-stale", "ac-wake-stale")
	bootstrapSession(t, env, r, ac, sess)

	// Mark the existing runner pod as Completed (simulates a prior turn that
	// agent_work_complete'd cleanly).
	var pod corev1.Pod
	podKey := types.NamespacedName{Namespace: "default", Name: "wake-stale-runner"}
	require.NoError(t, env.Client.Get(ctx, podKey, &pod), "Get prior runner Pod")
	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, env.Client.Status().Update(ctx, &pod), "set Pod Succeeded")

	// Move session to Idle with a recent LastIdleAt + fire wake annotation.
	setIdlePhaseWithTimestamp(t, env, sess, time.Now().Add(-1*time.Minute))
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().UTC().Format(time.RFC3339Nano)
	require.NoError(t, env.Client.Update(ctx, &fresh), "patch wake annotation")

	// Reconcile: wake handler should delete the Completed pod.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	// The previous pod should be gone (or marked for deletion).
	var afterPod corev1.Pod
	err := env.Client.Get(ctx, podKey, &afterPod)
	stillStaleSucceeded := err == nil && afterPod.DeletionTimestamp == nil && afterPod.Status.Phase == corev1.PodSucceeded
	assert.False(t, stillStaleSucceeded, "Completed runner Pod should have been deleted on wake")

	// Subsequent reconciles should create a fresh pod.
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}
	var newPod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, podKey, &newPod), "fresh runner Pod should be created after wake")
	assert.NotEqual(t, corev1.PodSucceeded, newPod.Status.Phase,
		"fresh pod should not be in Succeeded phase immediately after creation")
}

// TestAgentSession_AwaitingRetry_WakesToPending verifies that a session in
// phase=AwaitingRetry with a fresh wake annotation transitions to Pending on
// reconcile (the same path as Idle→Pending when the user clicks Retry).
func TestAgentSession_AwaitingRetry_WakesToPending(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconcilerWithArchive(t, env, 0)

	ac := validClass("ac-retry")
	ac.Spec.Channels = &spiceboxv1alpha1.ChannelsConfig{
		IdleTTL: metav1.Duration{Duration: 5 * time.Minute},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	// Channel-attached session — same shape as the Idle-wake tests.
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-sess", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "ac-retry",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the thing"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name:              "slack-ch",
				Kind:              "slack",
				Key:               "dm:U789",
				Capabilities:      []string{"text"},
				NATSSubjectPrefix: "ap.session.default.retry-sess",
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create channel-attached AgentSession")

	// Bootstrap: installs finalizer + RBAC + initial pod (status is empty here).
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Transition session to AwaitingRetry (simulate runner writing this after a
	// provider error — the runner pod has exited).
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	fresh.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry
	fresh.Status.RetryAttempts = 1
	fresh.Status.Conditions = append(fresh.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionAwaitingRetry,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAgentSessionProviderErr,
		LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, env.Client.Status().Update(ctx, &fresh), "set phase=AwaitingRetry")

	// Delete the existing runner pod (it exited after the provider error).
	var pod corev1.Pod
	_ = env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "retry-sess-runner"}, &pod)
	_ = env.Client.Delete(ctx, &pod)

	// Reconcile without wake annotation — must stay parked.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	var check spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &check), "Get after no-wake reconcile")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, check.Status.Phase,
		"phase must stay AwaitingRetry without wake annotation")

	// User clicks Retry: channelsd writes the wake annotation.
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "refresh before patching wake")
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().UTC().Format(time.RFC3339Nano)
	require.NoError(t, env.Client.Update(ctx, &fresh), "patch wake annotation")

	// Reconcile: shouldWake must fire from AwaitingRetry and transition to Pending.
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession after retry-wake")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, got.Status.Phase,
		"AwaitingRetry + wake annotation must transition to Pending")
	assert.NotNil(t, got.Status.LastWakeAt, "LastWakeAt should be set after wake")
}

// TestArchiveSweepIgnoresKubectlSessions verifies that sessions without a
// spec.channel (kubectl-driven) are never archived by the sweep.
func TestArchiveSweepIgnoresKubectlSessions(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconcilerWithArchive(t, env, time.Minute)

	// Class has a short archiveAfter — would archive if channel is set.
	ac := classWithArchiveAfter("ac-kubectl", time.Minute)
	// kubectl-driven session: no Channel.
	sess := validSession("kubectl1", "ac-kubectl")
	bootstrapSession(t, env, r, ac, sess)

	// Force phase=Idle with an old LastIdleAt directly (no channel binding).
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	ts := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	fresh.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
	fresh.Status.LastIdleAt = &ts
	require.NoError(t, env.Client.Status().Update(ctx, &fresh), "set Idle+LastIdleAt")

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get AgentSession")
	// kubectl-driven: spec.channel == nil → archive sweep must be skipped.
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, got.Status.Phase,
		"kubectl-driven session should NOT be archived by the sweep")
}

// TestReconcileMCPMissingAgentIdentityRequeues verifies that an AgentSession
// whose AgentClass references an MCPServer + an AgentIdentity that does not
// exist makes the reconciler requeue (non-zero RequeueAfter) rather than
// falling back to an unpinned secrets rule. The runner Role must not be
// created with a namespace-wide secrets get.
func TestReconcileMCPMissingAgentIdentityRequeues(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	// MCPServer CR the class references — it exists; the AgentIdentity does not.
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "hubspot-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.example.com"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
		},
	}), "create MCPServer")

	ac := validClass("ac-mcp-missing")
	ac.Spec.AgentIdentity = "does-not-exist"
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: "hubspot", Ref: "hubspot-mcp"},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("mcp-missing", "ac-mcp-missing")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// First reconcile installs the finalizer; subsequent ones reach the
	// MCP-identity resolve step and must requeue.
	var lastResult ctrl.Result
	for i := 0; i < 4; i++ {
		var err error
		lastResult, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
		require.NoError(t, err, "reconcile iteration %d must not hard-fail", i)
	}
	assert.Positive(t, lastResult.RequeueAfter,
		"reconcile must requeue (non-zero RequeueAfter) when the MCP AgentIdentity does not exist")

	// The runner Role must NOT have been created — the reconcile requeued
	// before the RBAC-build step, so there is no chance of an unpinned rule.
	var role rbacv1.Role
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "mcp-missing-runner"}, &role)
	assert.True(t, apierrors.IsNotFound(err),
		"runner Role must not be created while the MCP AgentIdentity is unresolved; got err=%v", err)
}

// TestReconcileMCPPinsSecretsToAgentIdentityCredentials verifies the happy
// path: an AgentClass with an MCPServer + an AgentIdentity carrying two
// secret-backed credentials produces a runner Role whose agentidentities
// and secrets rules are pinned by name.
func TestReconcileMCPPinsSecretsToAgentIdentityCredentials(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "hubspot-mcp", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.example.com"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
		},
	}), "create MCPServer")

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-id", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "static-cred", Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "hubspot-static", Key: "token"},
					},
				},
				{
					Name: "oauth-cred", Type: "oauth",
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretRef{Name: "hubspot-oauth"},
					},
				},
			},
		},
	}), "create AgentIdentity")

	ac := validClass("ac-mcp-ok")
	ac.Spec.AgentIdentity = "mcp-id"
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: "hubspot", Ref: "hubspot-mcp"},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("mcp-ok", "ac-mcp-ok")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	var role rbacv1.Role
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "mcp-ok-runner"}, &role),
		"runner Role should exist")

	idRule := findRuleForResource(role.Rules, "agentprimitives.authzed.com", "agentidentities")
	require.NotNil(t, idRule, "agentidentities rule must be present for an MCP class")
	assert.Equal(t, []string{"mcp-id"}, idRule.ResourceNames,
		"agentidentities rule pinned to the class's AgentIdentity")

	secRule := findRuleForResource(role.Rules, "", "secrets")
	require.NotNil(t, secRule, "secrets rule must be present for an MCP class with secret-backed creds")
	assert.Equal(t, []string{"hubspot-oauth", "hubspot-static"}, secRule.ResourceNames,
		"secrets rule pinned to the deduplicated, sorted Secret names")
}

// TestReconcileToolBundlePinsAgentIdentity is the regression test for the
// runner-RBAC gap a sandbox-tool agent hit: an AgentClass with a `toolBundles`
// entry (and NO MCPServers) that names an AgentIdentity got no
// `get agentidentities` rule on its per-session Role, so the runner was
// forbidden to read the identity when resolving a tool credential
// (e.g. GITHUB_TOKEN for gh) and failed with MCPAuthResolutionFailed. The
// identity/secret pin must fire for toolBundles too, not only MCPServers.
func TestReconcileToolBundlePinsAgentIdentity(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-bundle-class"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "gh", Command: []string{"/usr/bin/gh"}}},
		},
	}), "create SpiceboxClass")

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "tool-id", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "github-token", Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "gh-token-secret", Key: "token"},
					},
				},
			},
		},
	}), "create AgentIdentity")

	ac := validClass("ac-tool-id")
	ac.Spec.AgentIdentity = "tool-id"
	// A sandbox toolBundle, and explicitly NO MCPServers — the case the fix covers.
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "github", Class: "gh-bundle-class", Toolspecs: []string{}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("tool-sess", "ac-tool-id")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	for i := 0; i < 5; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// A toolBundle runner waits on its bundle SpiceboxSession, so the
	// per-session runner Role/Secret are built only once the bundle is Ready.
	// Mark it Ready and reconcile again (mirrors the bundle-readiness flow).
	var bs spiceboxv1alpha1.SpiceboxSession
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "tool-sess-github"}, &bs),
		"bundle SpiceboxSession should be created")
	bs.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.SpiceboxSessionConditionReady, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonPodReady, LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, &bs), "stamp bundle SpiceboxSession Ready=True")
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	var role rbacv1.Role
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "tool-sess-runner"}, &role),
		"runner Role should exist")

	idRule := findRuleForResource(role.Rules, "agentprimitives.authzed.com", "agentidentities")
	require.NotNil(t, idRule,
		"agentidentities rule must be present for a toolBundle class that names an AgentIdentity")
	assert.Equal(t, []string{"tool-id"}, idRule.ResourceNames,
		"agentidentities rule pinned to the class's AgentIdentity")

	secRule := findRuleForResource(role.Rules, "", "secrets")
	require.NotNil(t, secRule,
		"secrets rule must be present for a toolBundle identity with a secret-backed credential")
	assert.Contains(t, secRule.ResourceNames, "gh-token-secret",
		"secrets rule pinned to the credential's Secret")
}

// TestCollectCredentialSecretNamesDedup verifies that collectCredentialSecretNames
// deduplicates Secret names across an AgentIdentity's credentials, excludes empty
// names, and returns a sorted slice.
func TestCollectCredentialSecretNamesDedup(t *testing.T) {
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-dedup", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				// Two credentials sharing the same Secret name — dedup must collapse to one.
				{
					Name: "static-a", Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "shared-secret", Key: "token"},
					},
				},
				{
					Name: "oauth-a", Type: "oauth",
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretRef{Name: "shared-secret"},
					},
				},
				// A distinct second name — must appear once, sorted after "shared-secret".
				{
					Name: "static-b", Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "zebra-secret", Key: "token"},
					},
				},
				// An empty SecretRef.Name — must be excluded from the result.
				{
					Name: "static-empty", Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "", Key: "token"},
					},
				},
			},
		},
	}

	got := agentsession.CollectCredentialSecretNamesForTest(context.Background(), ai)
	assert.Equal(t, []string{"shared-secret", "zebra-secret"}, got,
		"deduplicated, sorted Secret names with empty name excluded")
}

// fakeSpiceDBDeleter records both halves of SpiceDB session teardown.
type fakeSpiceDBDeleter struct {
	called          bool
	lastNS          string
	lastName        string
	slotGrantsNS    string
	slotGrantsName  string
	slotGrantsCalls int
	dataSlotsNS     string
	dataSlotsName   string
	dataSlotsCalls  int
	// order records which teardown call arrived first.
	order []string

	// stalePresent simulates slot_pin/slot_grant_* tuples already sitting in
	// SpiceDB for a given ns/name — e.g. a straggling write from a dead
	// predecessor session that reused this name. seedStaleSlotTuples marks an
	// entry present; DeleteSlotGrants clears it, so a test can assert the
	// stale tuples are actually GONE rather than merely that the call fired.
	stalePresent map[string]bool

	// slotGrantsFailures makes the next N DeleteSlotGrants calls fail, before
	// they clear anything — a SpiceDB blip during the admission sweep.
	slotGrantsFailures int
}

func (f *fakeSpiceDBDeleter) seedStaleSlotTuples(ns, name string) {
	if f.stalePresent == nil {
		f.stalePresent = map[string]bool{}
	}
	f.stalePresent[ns+"/"+name] = true
}

func (f *fakeSpiceDBDeleter) hasStaleSlotTuples(ns, name string) bool {
	return f.stalePresent[ns+"/"+name]
}

func (f *fakeSpiceDBDeleter) DeleteAgentSessionRelationships(_ context.Context, ns, name string) error {
	f.called = true
	f.lastNS = ns
	f.lastName = name
	f.order = append(f.order, "session")
	return nil
}

func (f *fakeSpiceDBDeleter) DeleteSlotGrants(_ context.Context, ns, name string) error {
	f.slotGrantsCalls++
	if f.slotGrantsFailures > 0 {
		f.slotGrantsFailures--
		return errors.New("spicedb unavailable")
	}
	f.slotGrantsNS = ns
	f.slotGrantsName = name
	f.order = append(f.order, "slots")
	delete(f.stalePresent, ns+"/"+name)
	return nil
}

func (f *fakeSpiceDBDeleter) DeleteDataSlotGrants(_ context.Context, ns, name string) error {
	f.dataSlotsCalls++
	f.dataSlotsNS = ns
	f.dataSlotsName = name
	f.order = append(f.order, "dataSlots")
	return nil
}

func TestFinalize_DeletesSpiceDBRelationships(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	deleter := &fakeSpiceDBDeleter{}
	r := newReconciler(t, env)
	r.SpiceDBDeleter = deleter

	ac := validClass("ac-spicedb")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("sess-spicedb", "ac-spicedb")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Bootstrap: add finalizer + set up resources.
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Delete the AgentSession and reconcile through the finalizer path.
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get AgentSession")
	require.NoError(t, env.Client.Delete(ctx, &fresh), "delete AgentSession")
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	require.NoError(t, err, "Reconcile finalize")

	assert.True(t, deleter.called,
		"SpiceDBDeleter.DeleteAgentSessionRelationships should be called on finalize")
	assert.Equal(t, "default", deleter.lastNS, "SpiceDBDeleter called with namespace")
	assert.Equal(t, "sess-spicedb", deleter.lastName, "SpiceDBDeleter called with name")

	// Slot grants sit on external resources with the session as SUBJECT, so the
	// agentsession-as-RESOURCE wipe above cannot reach them. Missing this call
	// leaks live authority on somebody else's resource for as long as the
	// retained AgentSession CR keeps slot_grant->interact resolving.
	//
	// TWO calls, not one: the admission sweep (during bootstrap, on the
	// reconcile that adds the finalizer) ALSO calls DeleteSlotGrants, to wipe
	// any stale slot_pin/slot_grant_* left behind by a dead predecessor that
	// reused this session's name — see TestReconcile_AdmissionSweepsStaleSlotTuples.
	assert.Equal(t, 2, deleter.slotGrantsCalls,
		"the admission sweep and finalize must each collect the session's slot grants")
	assert.Equal(t, "default", deleter.slotGrantsNS, "DeleteSlotGrants called with namespace")
	assert.Equal(t, "sess-spicedb", deleter.slotGrantsName, "DeleteSlotGrants called with name")
	// Data slot grants are a THIRD sweep and neither of the others reaches
	// them: `pt_tag:<T>#granted_to@agentsession:<ns/name>` has the session as
	// SUBJECT (so the agentsession-as-resource filter misses it) and
	// `granted_to` is not a `slot_grant_<perm>` relation (so the slot sweep
	// drops it client-side). Without this call a child that was handed data
	// keeps `access` on those tags for as long as the retained CR exists.
	assert.Equal(t, 1, deleter.dataSlotsCalls, "finalize must also collect the session's data slot grants")
	assert.Equal(t, "default", deleter.dataSlotsNS, "DeleteDataSlotGrants called with namespace")
	assert.Equal(t, "sess-spicedb", deleter.dataSlotsName, "DeleteDataSlotGrants called with name")

	assert.Equal(t, []string{"slots", "slots", "dataSlots", "session"}, deleter.order,
		"the admission sweep's slot-grant call lands first (during bootstrap); at finalize, grants that sit on "+
			"OTHER objects still go first — authority then data — because they are the half that outlives the session")

	// Finalizer should be removed.
	var gone spiceboxv1alpha1.AgentSession
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &gone); err == nil {
		for _, f := range gone.Finalizers {
			assert.NotEqual(t, spiceboxv1alpha1.FinalizerAgentSession, f,
				"AgentSession finalizer should be removed after successful finalization")
		}
	}
}

// TestReconcile_AdmissionSweepsStaleSlotTuples: a brand-new AgentSession whose
// name was reused from a dead predecessor (killed without ever reaching
// finalize, or a name recycled by a user) must not inherit that predecessor's
// slot_pin/slot_grant_* tuples. A pin never expires, so without a sweep at
// admission the new session would be refused its own first bind by a pin it
// never earned. The sweep must fire exactly once — on the reconcile that adds
// the finalizer — not on every reconcile of the session.
func TestReconcile_AdmissionSweepsStaleSlotTuples(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	deleter := &fakeSpiceDBDeleter{}
	r := newReconciler(t, env)
	r.SpiceDBDeleter = deleter

	ac := validClass("ac-admission-sweep")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("sess-admission-sweep", "ac-admission-sweep")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Seed the stale tuples a dead predecessor with this same name left
	// behind, BEFORE this session is ever reconciled.
	deleter.seedStaleSlotTuples(sess.Namespace, sess.Name)
	require.True(t, deleter.hasStaleSlotTuples(sess.Namespace, sess.Name), "fixture sanity: the stale tuples must be seeded")

	// First reconcile: adds the finalizer and must run the admission sweep
	// before returning.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	require.NoError(t, err, "Reconcile (admission)")

	assert.Equal(t, 1, deleter.slotGrantsCalls, "the admission sweep must call DeleteSlotGrants exactly once")
	assert.Equal(t, sess.Namespace, deleter.slotGrantsNS)
	assert.Equal(t, sess.Name, deleter.slotGrantsName)
	assert.False(t, deleter.hasStaleSlotTuples(sess.Namespace, sess.Name),
		"the stale slot_pin/slot_grant_* tuples must be gone after admission")

	// Further reconciles of the same session must NOT sweep again — the
	// finalizer is already present, so EnsureFinalizer's added=false branch
	// is taken every time after the first.
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}
	assert.Equal(t, 1, deleter.slotGrantsCalls, "the admission sweep must run exactly once per session, not on every reconcile")
}

// A FAILED admission sweep must be retried, not skipped. The sweep used to run
// after the finalizer write, so the requeue saw the finalizer present and never
// swept again, letting the session proceed with a predecessor's pin live under
// its name. The finalizer is now added only after a sweep succeeds.
func TestReconcile_AdmissionSweepRetriesAfterFailure(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	deleter := &fakeSpiceDBDeleter{slotGrantsFailures: 1}
	r := newReconciler(t, env)
	r.SpiceDBDeleter = deleter

	ac := validClass("ac-admission-sweep-retry")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("sess-admission-sweep-retry", "ac-admission-sweep-retry")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	deleter.seedStaleSlotTuples(sess.Namespace, sess.Name)
	key := client.ObjectKeyFromObject(sess)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.Error(t, err, "a failed sweep must fail the reconcile")
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &got))
	assert.NotContains(t, got.Finalizers, spiceboxv1alpha1.FinalizerAgentSession,
		"the finalizer must not be added until the sweep succeeds")
	assert.True(t, deleter.hasStaleSlotTuples(sess.Namespace, sess.Name), "fixture sanity: the failed sweep cleared nothing")

	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "Reconcile (retry)")
	assert.Equal(t, 2, deleter.slotGrantsCalls, "the requeue must sweep again")
	assert.False(t, deleter.hasStaleSlotTuples(sess.Namespace, sess.Name),
		"the stale tuples must be gone once the retried sweep succeeds")
	require.NoError(t, env.Client.Get(ctx, key, &got))
	assert.Contains(t, got.Finalizers, spiceboxv1alpha1.FinalizerAgentSession)
}

// TestReconcile_AdmissionSweepSparesAForkChildsCopiedTuples: the parent's
// ReconcileRestart copies slot grants + pin onto the fork child's (ns, name)
// BEFORE the child's own first reconcile runs (BuildChildSession creates the
// child with no finalizer). If the admission sweep ran on the child, it would
// be the LAST writer and wipe that copied authority: every clean-path
// non-takeover fork would come back unpinned with its inherited grants
// deleted. A fork child's name is generated (PendingRestart.TargetSessionName),
// so the dead-predecessor name-reuse the sweep guards against cannot happen
// to it — the sweep must skip any session whose spec marks it forked.
func TestReconcile_AdmissionSweepSparesAForkChildsCopiedTuples(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	deleter := &fakeSpiceDBDeleter{}
	r := newReconciler(t, env)
	r.SpiceDBDeleter = deleter

	ac := validClass("ac-fork-no-sweep")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	child := validSession("sess-fork-no-sweep-child", "ac-fork-no-sweep")
	child.Spec.ForkedFrom = "sess-fork-no-sweep-parent" // what BuildChildSession stamps on every restart child
	require.NoError(t, env.Client.Create(ctx, child), "create fork-child AgentSession")

	// These stand in for the grants + pin the parent's ReconcileRestart
	// already copied onto the child's name — legitimate authority, not a
	// stale leftover.
	deleter.seedStaleSlotTuples(child.Namespace, child.Name)

	// The child's first reconcile adds the finalizer; the admission sweep
	// must NOT fire for a fork child.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(child)})
	require.NoError(t, err, "Reconcile (fork child admission)")

	assert.Zero(t, deleter.slotGrantsCalls,
		"the admission sweep must not run for a fork child — it would wipe the authority the parent's restart just copied")
	assert.True(t, deleter.hasStaleSlotTuples(child.Namespace, child.Name),
		"the copied grant+pin tuples must survive the child's first reconcile")
}

// TestReconcile_AdmissionSweepSkippedWhenFinalizerPreStamped: a channelsd mint
// pre-stamps the operator's finalizer at Create and sweeps stale slot tuples
// itself, synchronously, before its mint-time binds. So when the operator first
// reconciles such a session the finalizer is ALREADY present: EnsureFinalizer
// returns added=false, the added=true branch (and its admission sweep) never
// runs, and the operator must NOT wipe the tuples channelsd just wrote. This is
// the restart-variant of the sweep guard — a session that already carries the
// finalizer at first reconcile must take the added=false path.
func TestReconcile_AdmissionSweepSkippedWhenFinalizerPreStamped(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	deleter := &fakeSpiceDBDeleter{}
	r := newReconciler(t, env)
	r.SpiceDBDeleter = deleter

	ac := validClass("ac-prestamped-finalizer")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("sess-prestamped-finalizer", "ac-prestamped-finalizer")
	// The channelsd mint path stamps this at Create (pipeline.go), which is what
	// suppresses the operator's own sweep for a channelsd-minted session.
	sess.Finalizers = append(sess.Finalizers, spiceboxv1alpha1.FinalizerAgentSession)
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Stand-in for the pin + grants channelsd legitimately wrote at mint under
	// this name. The operator must leave them alone.
	deleter.seedStaleSlotTuples(sess.Namespace, sess.Name)

	// First reconcile: finalizer already present → added=false → no admission
	// block, no sweep. Nothing outside the admission block or finalize calls
	// DeleteSlotGrants, so its count must stay zero.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})

	assert.Zero(t, deleter.slotGrantsCalls,
		"a pre-stamped finalizer takes the added=false path, so the operator's admission sweep must not fire for a channelsd mint")
	assert.True(t, deleter.hasStaleSlotTuples(sess.Namespace, sess.Name),
		"the operator must not wipe the tuples channelsd wrote under a session whose finalizer it pre-stamped")
}

// ownerCapturingGranter implements authz.Granter and records TouchOwner calls.
type ownerCapturingGranter struct {
	ownerCalled  bool
	ownerNS      string
	ownerName    string
	ownerSubject string
}

func (f *ownerCapturingGranter) TouchStartedBy(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	return nil
}
func (f *ownerCapturingGranter) TouchOwner(_ context.Context, ns, name, subjectRef string) error {
	f.ownerCalled = true
	f.ownerNS = ns
	f.ownerName = name
	f.ownerSubject = subjectRef
	return nil
}
func (f *ownerCapturingGranter) TouchInteractParticipant(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *ownerCapturingGranter) TouchInteractParticipantUser(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	return nil
}
func (f *ownerCapturingGranter) TouchDeniedUser(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	return nil
}

func (f *ownerCapturingGranter) TouchInteractor(_ context.Context, _, _, _ string) error {
	return nil
}

func TestReconcile_WritesResolvedOwner(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	granter := &ownerCapturingGranter{}
	r := newReconciler(t, env)
	r.AuthzGranter = granter

	ac := validClass("ac-owner")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("sess-owner", "ac-owner")
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:abc",
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	assert.True(t, granter.ownerCalled, "TouchOwner must be called during reconcile")
	assert.Equal(t, "default", granter.ownerNS, "TouchOwner namespace")
	assert.Equal(t, "sess-owner", granter.ownerName, "TouchOwner session name")
	assert.Equal(t, "user:abc", granter.ownerSubject, "TouchOwner subject from annotation")
}

func TestReconcile_ProvisionsWorkspacePVC_AndStampsBundleSessions(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)
	r.WorkspaceStorageClass = "rwx-test"

	// Two referenced SpiceboxClasses (one per bundle).
	for _, cn := range []string{"git-class", "code-class"} {
		require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
			ObjectMeta: metav1.ObjectMeta{Name: cn},
			Spec: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:     "spicebox-sandbox:dev",
				Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
				Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "echo", Command: []string{"/bin/echo"}}},
			},
		}), "create SpiceboxClass %s", cn)
	}
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id1", Namespace: "default"},
	}), "create AgentIdentity")
	ac := validClass("ac-ws")
	ac.Spec.AgentIdentity = "id1"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "git", Class: "git-class", Toolspecs: []string{}},
		{Name: "code", Class: "code-class", Toolspecs: []string{}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("rb", "ac-ws")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	for i := 0; i < 6; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// PVC provisioned, owned, RWX.
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, env.Client.Get(ctx,
		client.ObjectKey{Namespace: "default", Name: "rb-workspace"}, &pvc),
		"workspace PVC should be created")
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pvc.Spec.AccessModes)
	require.Len(t, pvc.OwnerReferences, 1, "PVC must be owned by AgentSession")
	assert.Equal(t, "rb", pvc.OwnerReferences[0].Name)
	require.NotNil(t, pvc.Spec.StorageClassName)
	assert.Equal(t, "rwx-test", *pvc.Spec.StorageClassName)

	// Both bundle SpiceboxSessions reference the claim and stamp Mode: shared.
	for _, bn := range []string{"git", "code"} {
		var sbox spiceboxv1alpha1.SpiceboxSession
		require.NoError(t, env.Client.Get(ctx,
			client.ObjectKey{Namespace: "default", Name: "rb-" + bn}, &sbox),
			"bundle session %q should exist", bn)
		assert.Equal(t, spiceboxv1alpha1.WorkspaceShared, sbox.Spec.Workspace.Mode,
			"bundle %q workspace mode", bn)
		assert.Equal(t, "rb-workspace", sbox.Spec.Workspace.SharedClaimName,
			"bundle %q claim name", bn)
	}

	// Git bundle picked up DefaultEnv hardening; code bundle did not.
	var gitSess spiceboxv1alpha1.SpiceboxSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "rb-git"}, &gitSess))
	assert.Equal(t, "/dev/null", gitSess.Spec.DefaultEnv["GIT_CONFIG_GLOBAL"])

	var codeSess spiceboxv1alpha1.SpiceboxSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "rb-code"}, &codeSess))
	assert.Empty(t, codeSess.Spec.DefaultEnv, "non-git bundle has no DefaultEnv")
}

func TestReconcile_NoStorageClass_IsolatedBundleSessions(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env) // WorkspaceStorageClass left ""

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "code-class"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "echo", Command: []string{"/bin/echo"}}},
		},
	}), "create SpiceboxClass")
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id1", Namespace: "default"},
	}), "create AgentIdentity")
	ac := validClass("ac-iso")
	ac.Spec.AgentIdentity = "id1"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "code", Class: "code-class", Toolspecs: []string{}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("iso", "ac-iso")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	for i := 0; i < 6; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// No PVC when StorageClass unset.
	var pvc corev1.PersistentVolumeClaim
	err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "iso-workspace"}, &pvc)
	assert.True(t, apierrors.IsNotFound(err), "no PVC when StorageClass unset; got: %v", err)

	var sbox spiceboxv1alpha1.SpiceboxSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "iso-code"}, &sbox))
	assert.Equal(t, spiceboxv1alpha1.WorkspaceIsolated, sbox.Spec.Workspace.Mode)
	assert.Empty(t, sbox.Spec.Workspace.SharedClaimName)
}

// TestChannelSessionWithoutNATSIdentityHasNoCredsMounts pins the
// invariant that the runner pod's nats-creds/nats-ca SubPath mounts
// track the per-session Secret's actual keys. With NATSIdentity nil
// (operator started without the spicebox-nats-identity Secret) the
// controller writes a per-session Secret WITHOUT the nats.creds/
// nats.ca keys; the runner pod must therefore NOT declare those
// SubPath mounts, or the pod hangs in ContainerCreating. The
// non-creds channel env (NATS_URL / CHANNEL_ATTACHED) stays present.
func TestChannelSessionWithoutNATSIdentityHasNoCredsMounts(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)
	// NATSIdentity left nil — operator without the install-time NATS Secret.

	ac := validClass("ac-nonats")
	sess := channelSession("nonats1", "ac-nonats")
	bootstrapSession(t, env, r, ac, sess)

	// Ground truth: the per-session Secret has no nats.creds key.
	var sec corev1.Secret
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "nonats1-memory-token"}, &sec),
		"per-session Secret should exist")
	require.Empty(t, sec.Data["nats.creds"],
		"Secret must NOT carry nats.creds when NATSIdentity is nil")

	// The runner pod must agree: no nats SubPath mounts, no creds env.
	var pod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "nonats1-runner"}, &pod),
		"runner Pod should exist")

	mountPaths := map[string]bool{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		mountPaths[m.MountPath] = true
	}
	assert.False(t, mountPaths["/var/run/agent/nats-creds"],
		"pod must NOT mount nats.creds when the Secret lacks the key; mounts=%+v", pod.Spec.Containers[0].VolumeMounts)
	assert.False(t, mountPaths["/var/run/agent/nats-ca"],
		"pod must NOT mount nats.ca when the Secret lacks the key; mounts=%+v", pod.Spec.Containers[0].VolumeMounts)

	envByName := map[string]bool{}
	for _, e := range pod.Spec.Containers[0].Env {
		envByName[e.Name] = true
	}
	assert.False(t, envByName["NATS_CREDS_PATH"], "NATS_CREDS_PATH must be absent without minted creds")
	assert.False(t, envByName["NATS_CA_PATH"], "NATS_CA_PATH must be absent without minted creds")
	// Non-creds channel env is gated on InputChannel, not the creds flag.
	assert.True(t, envByName["NATS_URL"], "NATS_URL must still be set for a channel-attached session")
	assert.True(t, envByName["CHANNEL_ATTACHED"], "CHANNEL_ATTACHED must still be set for a channel-attached session")
}

// TestSession_SettingsAccepted_BudgetClamped verifies that when a
// ClusterAgentSettings sets a lower budget ceiling than the class, the
// reconciler stamps SettingsAccepted=True/SettingsClamped and the
// effectiveSettings reflects the clamped maxTurns.
func TestSession_SettingsAccepted_BudgetClamped(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	// ClusterAgentSettings ceiling: maxTurns=5, class/session want 50.
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				Budget: &spiceboxv1alpha1.SettingsBudgetCeiling{MaxTurns: 5},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings")

	ac := validClass("ac-budget-clamp")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-budget-clamp", "ac-budget-clamp")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Two passes: first installs the finalizer (returns Requeue), second runs the
	// settings gate. Check each error explicitly so a persistent failure is visible
	// rather than silently swallowed.
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s-budget-clamp"}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err, "Reconcile pass 1 must not error")
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err, "Reconcile pass 2 must not error")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-budget-clamp"}, &got),
		"Get AgentSession after reconcile")

	// SettingsAccepted must be True/SettingsClamped (budget clamped, non-fatal).
	settingsCond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted)
	require.NotNil(t, settingsCond, "SettingsAccepted condition must be present")
	assert.Equal(t, metav1.ConditionTrue, settingsCond.Status, "SettingsAccepted should be True (clamped is non-fatal)")
	assert.Equal(t, spiceboxv1alpha1.ReasonSettingsClamped, settingsCond.Reason, "reason should be SettingsClamped")

	// effectiveSettings.budget.maxTurns must be clamped to the ceiling (5).
	require.NotNil(t, got.Status.EffectiveSettings, "effectiveSettings must be stamped")
	assert.Equal(t, int32(5), got.Status.EffectiveSettings.Budget.MaxTurns,
		"effectiveSettings.budget.maxTurns should be clamped to the ceiling")
}

// TestSession_SettingsAccepted_ModelForbidden_NoPodCreated verifies that when
// a ClusterAgentSettings catalog denies the class model, the reconciler
// stamps SettingsAccepted=False and does NOT create a runner pod (non-terminal
// refusal so a ceiling relaxation can re-reconcile and proceed).
func TestSession_SettingsAccepted_ModelForbidden_NoPodCreated(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	// ClusterAgentSettings with a catalog that denies the class model.
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			ModelCatalog: &[]spiceboxv1alpha1.ModelCatalogEntry{{
				Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
				TokenRef: &spiceboxv1alpha1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
			}},
			Limits: &spiceboxv1alpha1.SettingsLimits{DeniedModels: []string{"claude-opus-4-7"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings")

	// Class uses claude-opus-4-7 which is denied.
	ac := validClass("ac-model-disallowed")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-model-disallowed", "ac-model-disallowed")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Two passes: first installs the finalizer (returns Requeue), second runs the
	// settings gate. Check each error explicitly so a persistent failure is visible
	// rather than silently swallowed.
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s-model-disallowed"}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err, "Reconcile pass 1 must not error")
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err, "Reconcile pass 2 must not error")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-model-disallowed"}, &got),
		"Get AgentSession after reconcile")

	// SettingsAccepted must be False/ModelForbidden.
	settingsCond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted)
	require.NotNil(t, settingsCond, "SettingsAccepted condition must be present")
	assert.Equal(t, metav1.ConditionFalse, settingsCond.Status, "SettingsAccepted should be False")
	assert.Equal(t, "ModelForbidden", settingsCond.Reason, "reason should be ModelForbidden")

	// No runner pod should have been created.
	var pod corev1.Pod
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-model-disallowed-runner"}, &pod)
	assert.True(t, apierrors.IsNotFound(err),
		"runner Pod must NOT be created when settings are fatally violated; got err=%v", err)
}

// TestSettingsRetroactive proves retroactive enforcement + grandfathering of
// already-running sessions when a ClusterAgentSettings is created that excludes
// the class's model after the session has already started.
//
// Scenario:
//  1. AgentClass + AgentSession resolve cleanly with NO Settings CRs ⇒ runner pod
//     created (pod exists in API server).
//  2. A ClusterAgentSettings is created whose catalog/deniedModels EXCLUDES the class model.
//  3. Fan-out re-reconcile flips SettingsAccepted=False (ModelForbidden) on the
//     existing session — but the session is grandfathered: the pod is NOT torn down
//     (the reconciler has no pod-deletion code path on settings violations; it only
//     refuses to start new runners for not-yet-started sessions).
//  4. A FRESH session of the same class is refused: SettingsAccepted=False, no pod.
//
// Note on RunnerReady in envtest: the PodRunnerFactory creates the pod
// synchronously (cache sees it immediately) but envtest has no kubelet to
// advance the pod to Running phase, so reflectRunnerPod never stamps
// RunnerReady=True. podAlreadyStarted therefore returns false even after the
// pod exists. Grandfathering is still observable: the controller returns early
// on the settings gate (SettingsAccepted=False) but has no code to delete an
// already-created pod, so it persists regardless.
func TestSettingsRetroactive(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	// --- Step 1: Bootstrap a session with no Settings CRs present ----------------
	// Class uses claude-opus-4-7 (the validClass default).
	ac := validClass("ac-retro")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-retro", "ac-retro")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Four reconciles: first installs finalizer (Requeue), subsequent passes
	// run settings resolution, RBAC, and pod creation.
	sessKey := types.NamespacedName{Namespace: "default", Name: "s-retro"}
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: sessKey})
	}

	// The runner pod must exist — RunnerFactory.Start was called successfully.
	var startedPod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-retro-runner"}, &startedPod),
		"runner pod must exist before the settings ceiling is imposed")

	// Verify no ceiling is blocking the session at this point.
	var afterStart spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &afterStart), "Get AgentSession after bootstrap")
	settingsCondBefore := conditions.Find(afterStart.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted)
	if settingsCondBefore != nil {
		assert.Equal(t, metav1.ConditionTrue, settingsCondBefore.Status,
			"SettingsAccepted must be True (or absent) before the ceiling is imposed")
	}

	// --- Step 2: Create a ClusterAgentSettings that denies the class's model ---
	// claude-opus-4-7 (the class model) is denied.
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			ModelCatalog: &[]spiceboxv1alpha1.ModelCatalogEntry{{
				Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
				TokenRef: &spiceboxv1alpha1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
			}},
			Limits: &spiceboxv1alpha1.SettingsLimits{DeniedModels: []string{"claude-opus-4-7"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings")

	// --- Step 3: Fan-out re-reconcile the already-started session ----------------
	// In this suite the manager is NOT running, so watches don't auto-fire.
	// Drive the re-reconcile manually, as the plan specifies.
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: sessKey})
	}

	var afterCeiling spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &afterCeiling),
		"Get AgentSession after ceiling imposed")

	// SettingsAccepted must flip to False/ModelForbidden.
	settingsCond := conditions.Find(afterCeiling.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted)
	require.NotNil(t, settingsCond, "SettingsAccepted condition must be present after re-reconcile")
	assert.Equal(t, metav1.ConditionFalse, settingsCond.Status,
		"SettingsAccepted must be False once the ceiling denies the class model")
	assert.Equal(t, "ModelForbidden", settingsCond.Reason,
		"SettingsAccepted reason must be ModelForbidden")

	// Grandfathering: the pod must NOT have been torn down. The reconciler has
	// no code path to delete an already-created pod on a settings violation —
	// it only blocks creation of NEW runner pods. The existing pod persists.
	var grandfatheredPod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-retro-runner"}, &grandfatheredPod),
		"runner pod must still exist — already-started sessions are grandfathered (pod not torn down)")

	// --- Step 4: A FRESH session of the same class is refused -------------------
	freshSess := validSession("s-retro-fresh", "ac-retro")
	require.NoError(t, env.Client.Create(ctx, freshSess), "create fresh AgentSession")

	freshKey := types.NamespacedName{Namespace: "default", Name: "s-retro-fresh"}
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: freshKey})
	}

	var freshGot spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, freshKey, &freshGot),
		"Get fresh AgentSession after reconcile under ceiling")

	// SettingsAccepted=False for the fresh session too.
	freshSettingsCond := conditions.Find(freshGot.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted)
	require.NotNil(t, freshSettingsCond, "SettingsAccepted condition must be present on fresh session")
	assert.Equal(t, metav1.ConditionFalse, freshSettingsCond.Status,
		"fresh session must be refused: SettingsAccepted=False while ceiling stands")
	assert.Equal(t, "ModelForbidden", freshSettingsCond.Reason,
		"fresh session SettingsAccepted reason must be ModelForbidden")

	// No runner pod for the fresh session.
	var freshPod corev1.Pod
	freshPodErr := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-retro-fresh-runner"}, &freshPod)
	assert.True(t, apierrors.IsNotFound(freshPodErr),
		"runner pod must NOT be created for a fresh session while the ceiling stands; got err=%v", freshPodErr)
}

// TestSettingsGrandfathersRunningSession exercises the podAlreadyStarted==true
// fall-through that TestSettingsRetroactive cannot reach in envtest.
//
// In envtest there is no kubelet, so the runner pod never advances to Running
// phase, reflectRunnerPod never stamps RunnerReady=True, and podAlreadyStarted
// returns false — TestSettingsRetroactive therefore takes the refusal branch
// even for the "already started" session. This test bypasses that limitation
// by stamping the session's status directly (phase=Running + RunnerReady=True)
// before imposing the ceiling, making podAlreadyStarted return true and
// exercising the grandfather (fall-through) code path: the reconciler stamps
// SettingsAccepted=False but does NOT flip the session to Pending and does NOT
// tear down the runner pod.
func TestSettingsGrandfathersRunningSession(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	r := newReconciler(t, env)

	// --- Step 1: Bootstrap a session with no Settings CRs ----------------------
	// Class uses claude-opus-4-7 (the validClass default).
	ac := validClass("ac-gf")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-gf", "ac-gf")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Four reconciles: installs finalizer, runs settings resolution + RBAC + pod creation.
	sessKey := types.NamespacedName{Namespace: "default", Name: "s-gf"}
	for i := 0; i < 4; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: sessKey})
	}

	// The runner pod must exist — RunnerFactory.Start was called successfully.
	var startedPod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-gf-runner"}, &startedPod),
		"runner pod must exist before simulating running state")

	// --- Step 2: Simulate a running session so podAlreadyStarted returns true --
	// envtest has no kubelet, so the pod never reaches Running and
	// reflectRunnerPod never stamps RunnerReady. Set both signals that
	// podAlreadyStarted checks: phase=Running AND RunnerReady=True condition.
	var runSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &runSess), "Get AgentSession before simulating running")
	runSess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	conditions.SetTrue(&runSess, &runSess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRunnerReady, spiceboxv1alpha1.ReasonRunnerReady)
	require.NoError(t, env.Client.Status().Update(ctx, &runSess),
		"stamp phase=Running + RunnerReady=True to make podAlreadyStarted return true")

	// Verify the stamp took.
	var check spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &check), "Get AgentSession after simulating running")
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, check.Status.Phase, "phase should be Running after stamp")
	require.NotNil(t, conditions.Find(check.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady),
		"RunnerReady condition should be present after stamp")

	// --- Step 3: Create a ClusterAgentSettings that denies the class's model ---
	// claude-opus-4-7 (the class model) is denied.
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			ModelCatalog: &[]spiceboxv1alpha1.ModelCatalogEntry{{
				Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
				TokenRef: &spiceboxv1alpha1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
			}},
			Limits: &spiceboxv1alpha1.SettingsLimits{DeniedModels: []string{"claude-opus-4-7"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings")

	// --- Step 4: Re-reconcile the running session under the new ceiling ----------
	// In this suite the manager is NOT running, so watches don't auto-fire.
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: sessKey})
	}

	var afterCeiling spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &afterCeiling),
		"Get AgentSession after ceiling imposed")

	// --- Step 5: Assert the grandfather outcome ----------------------------------
	// SettingsAccepted must flip to False/ModelForbidden — the violation is
	// detected and stamped even for running sessions.
	settingsCond := conditions.Find(afterCeiling.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted)
	require.NotNil(t, settingsCond, "SettingsAccepted condition must be present after re-reconcile")
	assert.Equal(t, metav1.ConditionFalse, settingsCond.Status,
		"SettingsAccepted must be False once the ceiling denies the class model")
	assert.Equal(t, "ModelForbidden", settingsCond.Reason,
		"SettingsAccepted reason must be ModelForbidden")

	// Grandfather: the session was NOT refused — phase must remain Running, not
	// flipped to Pending. This is the key assertion distinguishing grandfather
	// (podAlreadyStarted==true fall-through) from refusal (Pending flip).
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, afterCeiling.Status.Phase,
		"running session must stay Running on a retroactive fatal ceiling (grandfathered)")

	// The runner pod must still exist — no teardown on a settings violation.
	var grandfatheredPod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-gf-runner"}, &grandfatheredPod),
		"runner pod must still exist — a grandfathered running session is not torn down")
}
