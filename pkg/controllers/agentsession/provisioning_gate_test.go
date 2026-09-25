// pkg/controllers/agentsession/provisioning_gate_test.go
//
// Reconcile-level (fake-client) tests for the lazy bundle-provisioning gate:
// a parked Idle channel session (reaped or merely warm) must not have its
// bundle SpiceboxSessions re-created by the reconcile — the bundle-
// provisioning block used to run unconditionally on every non-terminal
// reconcile, so a reaped Idle session would immediately bounce back to full
// pod count. Only a wake (Idle -> Pending) re-provisions, and only on the
// reconcile AFTER the one that applies WakeRequested: the bundle-provisioning
// block runs earlier in Reconcile than the wake-annotation detection, so the
// wake pass itself still observes the stale Idle phase and skips
// provisioning; the phase persisted by that pass is what the next reconcile
// sees. See sandboxProvisioningDesired (sleep.go) for the pure gate.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// provisioningGateClass returns a Valid AgentClass (identityMode=agent) with
// two tool bundles, so a reconcile that reaches the bundle-provisioning block
// would create two bundle SpiceboxSessions. Shaped like
// bundle_epoch_reconcile_test.go's classWithBundle so the full Reconcile path
// (settings resolution, RBAC/Secret minting, …) runs cleanly end to end.
func provisioningGateClass() *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
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
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
			ToolBundles: []spiceboxv1alpha1.ToolBundle{
				{Name: "git", Class: "toolbelt"},
				{Name: "code", Class: "toolbelt"},
			},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAllReferencesResolve, LastTransitionTime: metav1.Now(),
		}}},
	}
}

// provisioningGateFixture wires a reconciler over a fake client pre-loaded
// with an Idle, channel-attached AgentSession and its resolvable AgentClass —
// but deliberately WITHOUT the bundle SpiceboxSessions themselves, simulating
// a reaped/parked session sitting at zero pods.
type provisioningGateFixture struct {
	r       *Reconciler
	c       client.Client
	sess    *spiceboxv1alpha1.AgentSession
	bundles []string // bundle SpiceboxSession names a (re)provision would create
	now     time.Time
}

func newProvisioningGateFixture(t *testing.T) provisioningGateFixture {
	t.Helper()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	lastIdleAt := metav1.NewTime(now.Add(-2 * time.Minute))

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
	gitName := BundleSessionName(sess, spiceboxv1alpha1.ToolBundle{Name: "git"})
	codeName := BundleSessionName(sess, spiceboxv1alpha1.ToolBundle{Name: "code"})
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase:      spiceboxv1alpha1.AgentSessionPhaseIdle,
		LastIdleAt: &lastIdleAt,
		BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
			{Name: "git", SpiceboxSessionName: gitName},
			{Name: "code", SpiceboxSessionName: codeName},
		},
	}

	class := provisioningGateClass()
	c := buildFakeClient(t, sess, class)
	r := &Reconciler{
		Client:    c,
		APIReader: c,
		Tokens:    tokens.NewRegistry(),
		Memory:    memory.NewLocal(inmem.NewBackend()),
		Now:       func() time.Time { return now },
	}
	r.RunnerFactory = &PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}
	return provisioningGateFixture{r: r, c: c, sess: sess, bundles: []string{gitName, codeName}, now: now}
}

func (f provisioningGateFixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := f.r.Reconcile(memory.WithSystemApproval(context.Background(), "test"),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}})
	require.NoError(t, err, "Reconcile")
	return res
}

func (f provisioningGateFixture) bundlesGone(t *testing.T) bool {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	for _, name := range f.bundles {
		err := f.c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &spiceboxv1alpha1.SpiceboxSession{})
		if !apierrors.IsNotFound(err) {
			return false
		}
	}
	return true
}

func (f provisioningGateFixture) getSession(t *testing.T) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(memory.WithSystemApproval(context.Background(), "test"), client.ObjectKey{Namespace: "default", Name: "s1"}, &got))
	return &got
}

// TestIdleSessionDoesNotReprovision pins the bug this task fixes: a parked
// Idle session whose bundle SpiceboxSessions were reaped (e.g. by
// reconcileSleep) must stay at zero pods on the next reconcile — not
// immediately bounce back to full pod count — as long as nothing woke it.
func TestIdleSessionDoesNotReprovision(t *testing.T) {
	f := newProvisioningGateFixture(t) // Idle, no wake annotation, bundles already absent

	f.reconcile(t)

	assert.True(t, f.bundlesGone(t), "Idle session must NOT re-create bundle SpiceboxSessions")
	got := f.getSession(t)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "stays Idle")
}

// TestWakeReprovisions is the positive twin: a slept, parked session that
// receives a fresh wake annotation transitions Idle -> Pending on the first
// reconcile (WakeRequested, clearing SleptAt) and re-provisions its bundle
// SpiceboxSessions on the following reconcile, once the persisted phase is
// Pending. The second assertion (SleptAt nil) protects the wake-clears-
// SleptAt line in the wake-annotation block — without it, idleSleepDue would
// treat the session as "already slept" forever and it could never sleep
// again.
func TestWakeReprovisions(t *testing.T) {
	f := newProvisioningGateFixture(t)
	sleptAt := metav1.NewTime(f.now.Add(-30 * time.Minute))
	f.sess.Status.SleptAt = &sleptAt
	require.NoError(t, f.c.Status().Update(memory.WithSystemApproval(context.Background(), "test"), f.sess), "seed already-slept status")

	// Fresh wake annotation: no prior LastWakeAt, so shouldWake is true.
	updated := f.getSession(t)
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = f.now.Format(time.RFC3339Nano)
	require.NoError(t, f.c.Update(memory.WithSystemApproval(context.Background(), "test"), updated), "seed wake annotation")

	f.reconcile(t) // wake: Idle -> Pending, clears SleptAt
	f.reconcile(t) // Pending: (re)provisions bundle SpiceboxSessions

	assert.False(t, f.bundlesGone(t), "woken session must re-provision its bundle SpiceboxSessions")
	got := f.getSession(t)
	assert.Nil(t, got.Status.SleptAt, "wake must clear SleptAt so the session can sleep again later")
}
