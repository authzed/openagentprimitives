package agentsession

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// runDurationSession builds an AgentSession in phase whose runner reports the
// given enforced maxDuration (0 = none) and runDuration (nil = never flushed).
// The live-resolved effectiveSettings cap is set to the same value; cases that
// need the two to diverge overwrite it.
func runDurationSession(phase string, enforced time.Duration, ran *time.Duration) *spiceboxv1alpha1.AgentSession {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "cls"},
	}
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase: phase,
		EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{
			Budget: spiceboxv1alpha1.BudgetConfig{MaxDuration: metav1.Duration{Duration: enforced}},
		},
	}
	if enforced > 0 {
		sess.Status.EnforcedMaxDuration = &metav1.Duration{Duration: enforced}
	}
	if ran != nil {
		sess.Status.RunDuration = &metav1.Duration{Duration: *ran}
	}
	return sess
}

// withLiveCap sets the re-resolved effectiveSettings cap, which the backstop
// must ignore in favour of the runner's enforced one.
func withLiveCap(sess *spiceboxv1alpha1.AgentSession, live time.Duration) *spiceboxv1alpha1.AgentSession {
	sess.Status.EffectiveSettings.Budget.MaxDuration = metav1.Duration{Duration: live}
	return sess
}

func newRunDurationReconciler(t *testing.T, now time.Time, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	c := buildFakeClient(t, objs...)
	return &Reconciler{
		Client:    c,
		APIReader: c,
		Tokens:    tokens.NewRegistry(),
		Memory:    memory.NewLocal(inmem.NewBackend()),
		Now:       func() time.Time { return now },
	}, c
}

// TestReconcileRunDuration covers the operator's maxDuration backstop directly:
// only a Running session reported well past the cap its runner holds is failed.
func TestReconcileRunDuration(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	running := spiceboxv1alpha1.AgentSessionPhaseRunning
	cases := []struct {
		name       string
		sess       *spiceboxv1alpha1.AgentSession
		wantFailed bool
	}{
		{
			name:       "Running, run-time 31h vs 2h cap (the wedged runner): Failed/BudgetExceeded",
			sess:       runDurationSession(running, 2*time.Hour, ptr.To(31*time.Hour)),
			wantFailed: true,
		},
		{
			name:       "Running, past cap but within grace (runner still enforcing): untouched",
			sess:       runDurationSession(running, 2*time.Hour, ptr.To(2*time.Hour+runDurationGrace/2)),
			wantFailed: false,
		},
		{
			name:       "Running, under cap: untouched",
			sess:       runDurationSession(running, 2*time.Hour, ptr.To(time.Hour)),
			wantFailed: false,
		},
		{
			name:       "Running, no maxDuration cap: untouched however long it ran",
			sess:       runDurationSession(running, 0, ptr.To(1000*time.Hour)),
			wantFailed: false,
		},
		{
			name:       "Running, runDuration never reported: untouched",
			sess:       runDurationSession(running, 2*time.Hour, nil),
			wantFailed: false,
		},
		{
			name:       "Idle past cap (runner fails it on its next turn): untouched",
			sess:       runDurationSession(spiceboxv1alpha1.AgentSessionPhaseIdle, 2*time.Hour, ptr.To(31*time.Hour)),
			wantFailed: false,
		},
		{
			name:       "cap lowered after pod start: session under the original cap is untouched",
			sess:       withLiveCap(runDurationSession(running, 4*time.Hour, ptr.To(2*time.Hour)), time.Hour),
			wantFailed: false,
		},
		{
			name:       "cap raised after pod start: measured against the runner's own cap, Failed past it",
			sess:       withLiveCap(runDurationSession(running, 2*time.Hour, ptr.To(31*time.Hour)), 100*time.Hour),
			wantFailed: true,
		},
		{
			name: "runner reports no enforced cap (predates the field): untouched even far past the live cap",
			sess: func() *spiceboxv1alpha1.AgentSession {
				s := runDurationSession(running, 2*time.Hour, ptr.To(31*time.Hour))
				s.Status.EnforcedMaxDuration = nil
				return s
			}(),
			wantFailed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newRunDurationReconciler(t, now, tc.sess)
			ctx := memory.WithSystemApproval(context.Background(), "test")

			failed, err := r.reconcileRunDuration(ctx, tc.sess)
			require.NoError(t, err)
			assert.Equal(t, tc.wantFailed, failed)

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(tc.sess), &got))
			if !tc.wantFailed {
				assert.Equal(t, tc.sess.Status.Phase, got.Status.Phase, "phase unchanged")
				return
			}
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBudget, got.Status.FailureReason)
			assert.NotNil(t, got.Status.FinishedAt, "FinishedAt stamped so the Failed reap grace runs from now")
			cond := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
			require.NotNil(t, cond, "Failed condition set")
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBudget, cond.Reason)
			assert.Contains(t, cond.Message, "maxDuration")
		})
	}
}

// TestReconcileRunDuration_SessionHold: Failed is terminal-sticky, so a held
// session must never be failed out from under its hold, and a hold lookup that
// fails must not be read as "no hold".
func TestReconcileRunDuration_SessionHold(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("Running past cap+grace with an unreleased SessionHold: untouched, no error", func(t *testing.T) {
		sess := runDurationSession(spiceboxv1alpha1.AgentSessionPhaseRunning, 2*time.Hour, ptr.To(31*time.Hour))
		hold := &spiceboxv1alpha1.SessionHold{
			ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "default"},
			Spec: spiceboxv1alpha1.SessionHoldSpec{
				SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: "s1"},
				Reason:     "evidence", Source: "test",
			},
		}
		r, c := newRunDurationReconciler(t, now, sess, hold)

		failed, err := r.reconcileRunDuration(ctx, sess)
		require.NoError(t, err)
		assert.False(t, failed)
		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &got))
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, got.Status.Phase, "held session not failed")
	})

	t.Run("activeHoldFor lookup fails: error returned, phase unchanged", func(t *testing.T) {
		sess := runDurationSession(spiceboxv1alpha1.AgentSessionPhaseRunning, 2*time.Hour, ptr.To(31*time.Hour))
		base := buildFakeClient(t, sess)
		c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*spiceboxv1alpha1.SessionHoldList); ok {
					return errors.New("apiserver unavailable")
				}
				return cl.List(ctx, list, opts...)
			},
		})
		r := &Reconciler{Client: c, APIReader: c, Now: func() time.Time { return now }}

		failed, err := r.reconcileRunDuration(ctx, sess)
		require.Error(t, err, "a hold lookup error must not be read as no hold")
		assert.False(t, failed)
		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(sess), &got))
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, got.Status.Phase, "phase unchanged")
	})
}

// TestReconcileRunDuration_PersistsEarlierUnwrittenStatus: Reconcile mutates
// sess in memory before the backstop runs (it re-derives the audit key, for
// one) without writing. The backstop's write must carry those changes too,
// because a Failed session's later reconciles return before re-deriving them.
func TestReconcileRunDuration_PersistsEarlierUnwrittenStatus(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	sess := runDurationSession(spiceboxv1alpha1.AgentSessionPhaseRunning, 2*time.Hour, ptr.To(31*time.Hour))
	sess.Status.AuditPublicKey = "stale-key"
	r, c := newRunDurationReconciler(t, now, sess)

	ctx := memory.WithSystemApproval(context.Background(), "test")
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &live))
	ctx = withReconcileOriginal(ctx, live.DeepCopy())
	live.Status.AuditPublicKey = "rederived-key" // an earlier step's unwritten change

	failed, err := r.reconcileRunDuration(ctx, &live)
	require.NoError(t, err)
	require.True(t, failed)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &got))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
	assert.Equal(t, "rederived-key", got.Status.AuditPublicKey, "the earlier in-memory change persisted with the Failed write")
}

// TestReconcile_wedgedRunningSessionPastMaxDuration_failsThenReaps drives the
// wedged-runner case through full Reconcile: a Running session whose runner
// reports 31h of run-time against a 2h budget. Before the backstop nothing in
// the operator read runDuration, so the session stayed Running forever. It must
// land on Failed, and once the Failed reap grace passes, its runner pod must be
// torn down like any other failed session's. The backstop sits above the
// AgentClass gate, so a class that went Valid=False must not keep a wedged
// runner Running.
func TestReconcile_wedgedRunningSessionPastMaxDuration_failsThenReaps(t *testing.T) {
	cases := []struct {
		name       string
		classValid bool
	}{
		{name: "class Valid: Failed/BudgetExceeded, then runner reaped", classValid: true},
		{name: "class not Valid (early-return gate): still Failed/BudgetExceeded, then runner reaped", classValid: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testWedgedSessionFailsThenReaps(t, tc.classValid)
		})
	}
}

func testWedgedSessionFailsThenReaps(t *testing.T, classValid bool) {
	t.Helper()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	clock := now

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "cls",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "chan-1", Kind: "fake", Key: "thread-1",
			},
		},
	}
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase:               spiceboxv1alpha1.AgentSessionPhaseRunning,
		RunDuration:         &metav1.Duration{Duration: 31 * time.Hour},
		EnforcedMaxDuration: &metav1.Duration{Duration: 2 * time.Hour},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeAgent,
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 50, MaxTokens: 100000,
				MaxDuration: metav1.Duration{Duration: 2 * time.Hour},
			},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAllReferencesResolve, LastTransitionTime: metav1.Now(),
		}}},
	}
	if !classValid {
		class.Status.Conditions = nil
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}

	c := buildFakeClient(t, sess, class, runner)
	r := &Reconciler{
		Client:                 c,
		APIReader:              c,
		Tokens:                 tokens.NewRegistry(),
		Memory:                 memory.NewLocal(inmem.NewBackend()),
		Now:                    func() time.Time { return clock },
		FailedSandboxReapGrace: time.Minute,
	}
	r.RunnerFactory = &PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}

	ctx := memory.WithSystemApproval(context.Background(), "test")
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "s1"}, &got))
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"a Running session far past maxDuration must be failed by the operator")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBudget, got.Status.FailureReason)

	// Past the Failed reap grace, the terminal path tears the runner down.
	clock = now.Add(2 * time.Minute)
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	var gotRunner corev1.Pod
	err = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: RunnerPodName(sess)}, &gotRunner)
	assert.True(t, apierrors.IsNotFound(err), "the wedged runner pod must be reaped, got err=%v", err)
}
