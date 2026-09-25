package agentsession_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// startRefusedBodyText mirrors the package's own startRefusedBody, spelled out
// because this is the EXTERNAL test package: writing it here is what makes a
// silent edit to the copy a test failure rather than a change nobody notices.
const startRefusedBodyText = "This agent only runs for the people on its list, and you're not on it yet. Ask an administrator of this agent to add you."

type fakeStartChecker struct {
	allow bool
	err   error
	calls []string // "<ns>/<name>#<permission>@<canonical>"
}

func (f *fakeStartChecker) CheckAgentClassStart(_ context.Context, ns, name, permission string, id identity.CanonicalUserID) (bool, error) {
	f.calls = append(f.calls, ns+"/"+name+"#"+permission+"@"+id.String())
	return f.allow, f.err
}

// gatedClass is a class with an allowlist whose starter set is CONFIRMED in
// the authorization store. The StartersLinked condition is not decoration: an
// unconfirmed set makes the gate indeterminate (the checker is never asked),
// so every case about what the checker answers has to establish it first —
// see TestEnforceStartGate_UnconfirmedStarterLinkRequeues for the other half.
func gatedClass(name string, adminsMayStart *bool) *spiceboxv1alpha1.AgentClass {
	ac := unlinkedGatedClass(name, adminsMayStart)
	ac.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentClassConditionStartersLinked,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonStartersLinked,
		LastTransitionTime: metav1.Now(),
	}}
	return ac
}

// unlinkedGatedClass is the same class BEFORE its reconciler has confirmed the
// starter set — the state a session can genuinely observe, since the class and
// session controllers race.
func unlinkedGatedClass(name string, adminsMayStart *bool) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
				AllowedStarters:        []string{"user:listed"},
				PlatformAdminsMayStart: adminsMayStart,
			}},
		},
	}
}

func gatedSession(class string, startedBy string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: class},
	}
	if startedBy != "" {
		s.Annotations = map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:" + startedBy}
	}
	return s
}

func newGateReconciler(t *testing.T, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass, chk *fakeStartChecker) (*agentsession.Reconciler, *[]string) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(sess, ac).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	var notices []string
	r := &agentsession.Reconciler{
		Client:       c,
		StartChecker: chk,
		StartRefusedNoticePublish: func(_ context.Context, ns, name, requester, body string) error {
			notices = append(notices, ns+"/"+name+"|"+requester+"|"+body)
			return nil
		},
	}
	return r, &notices
}

func TestEnforceStartGate_NoAllowlistIsNoGate(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "open", Namespace: "default"}}
	sess := gatedSession("open", "anyone")
	chk := &fakeStartChecker{}
	r, _ := newGateReconciler(t, sess, ac, chk)
	_, halted, err := r.EnforceStartGate(context.Background(), sess, ac)
	require.NoError(t, err)
	assert.False(t, halted)
	assert.Empty(t, chk.calls, "no allowlist: SpiceDB is never asked")
}

func TestEnforceStartGate_RefusesAndTellsThePerson(t *testing.T) {
	no := false
	ac := gatedClass("gatebot", &no)
	sess := gatedSession("gatebot", "stranger")
	chk := &fakeStartChecker{allow: false}
	r, notices := newGateReconciler(t, sess, ac, chk)

	_, halted, err := r.EnforceStartGate(context.Background(), sess, ac)
	require.NoError(t, err)
	assert.True(t, halted, "a refusal halts the reconcile")
	assert.Equal(t, []string{"default/gatebot#start_explicit@stranger"}, chk.calls,
		"platformAdminsMayStart:false checks start_explicit")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "s1"}, &got))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter, got.Status.FailureReason)
	require.NotNil(t, got.Status.StartFailure)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter, got.Status.StartFailure.Reason)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStarterAllowed)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)

	// The Failed condition's Message is rendered VERBATIM to the person in the
	// browser chat (pkg/web/webui/chat's failureMessageOf), so it carries the
	// fixed person-facing copy — not the operator detail, which names a
	// permission and a status field nobody outside the cluster can act on.
	failed := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, failed, "the refusal must land on the Failed condition the browser reads")
	assert.Equal(t, startRefusedBodyText, failed.Message)
	for _, internal := range []string{"start_explicit", "start_session", "started_by", "SpiceDB", "allowedStarters"} {
		assert.NotContains(t, failed.Message, internal,
			"internal vocabulary must not reach the person's screen")
	}

	// The operator detail is still recorded — on status.startFailure and the
	// StarterAllowed condition — so nothing was lost by keeping it off screen.
	assert.Contains(t, got.Status.StartFailure.Message, "start_explicit",
		"the operator-facing cause must survive somewhere an operator reads")
	assert.Contains(t, cond.Message, "start_explicit")

	require.Len(t, *notices, 1, "the person is told exactly once")
	assert.Contains(t, (*notices)[0], "default/s1|user:stranger|")
	assert.Contains(t, (*notices)[0], "not on it")
	for _, banned := range []string{"SpiceDB", "namespace", "YAML", "kubectl", "agentclass"} {
		assert.NotContains(t, (*notices)[0], banned, "no internal vocabulary in the notice")
	}
}

func TestEnforceStartGate_AllowsAndRecordsTheVerdictOnce(t *testing.T) {
	ac := gatedClass("gatebot", nil)
	sess := gatedSession("gatebot", "listed")
	chk := &fakeStartChecker{allow: true}
	r, notices := newGateReconciler(t, sess, ac, chk)

	ctx := context.Background()
	_, halted, err := r.EnforceStartGate(ctx, sess, ac)
	require.NoError(t, err)
	assert.False(t, halted)
	assert.Equal(t, []string{"default/gatebot#start_session@listed"}, chk.calls,
		"admins may start by default: start_session")
	assert.Empty(t, *notices)

	// The verdict must be DURABLE, not just set on the caller's in-memory
	// copy: re-Get through the client, the way a fresh reconcile would.
	var persisted spiceboxv1alpha1.AgentSession
	require.NoError(t, r.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s1"}, &persisted))
	cond := meta.FindStatusCondition(persisted.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStarterAllowed)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	// Decided once: a second pass over a FRESH object built from that same
	// persisted read (an operator restart, or any later reconcile, has no
	// other copy to work from) does not ask again — the verdict lives on
	// status, not on the pointer the first call happened to mutate.
	second := persisted.DeepCopy()
	_, halted, err = r.EnforceStartGate(ctx, second, ac)
	require.NoError(t, err)
	assert.False(t, halted)
	assert.Len(t, chk.calls, 1, "the checker is asked exactly once across both passes")
}

func TestEnforceStartGate_NoHumanStarterIsRefused(t *testing.T) {
	ac := gatedClass("gatebot", nil)
	sess := gatedSession("gatebot", "") // cron/webhook: nobody started it
	chk := &fakeStartChecker{allow: true}
	r, _ := newGateReconciler(t, sess, ac, chk)
	_, halted, err := r.EnforceStartGate(context.Background(), sess, ac)
	require.NoError(t, err)
	assert.True(t, halted)
	assert.Empty(t, chk.calls, "nothing to check: the refusal is structural")
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "s1"}, &got))
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter, got.Status.FailureReason)
}

func TestEnforceStartGate_IndeterminateRequeuesAndDoesNotFail(t *testing.T) {
	ac := gatedClass("gatebot", nil)
	sess := gatedSession("gatebot", "listed")
	chk := &fakeStartChecker{err: errors.New("spicedb unreachable")}
	r, notices := newGateReconciler(t, sess, ac, chk)
	_, halted, err := r.EnforceStartGate(context.Background(), sess, ac)
	require.Error(t, err, "an unanswered check is returned so controller-runtime requeues with backoff")
	assert.True(t, halted)
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "s1"}, &got))
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "indeterminate is NOT a refusal")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStarterAllowed)
	require.NotNil(t, cond)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionStartAuthzUnavailable, cond.Reason)
	assert.Empty(t, *notices)
}

// TestEnforceStartGate_UnconfirmedStarterLinkRequeues is the race the two
// controllers genuinely run: the class reconciler writes agentclass#starter,
// the session reconciler checks against it, and nothing orders them. Asking
// SpiceDB before the write lands is asking about a list it was never told, so a
// "no" would refuse a LISTED starter — terminally, since the verdict is decided
// once — and tell them they are not on a list they are on.
//
// The checker must not be asked at all: an answer obtained from an unconfirmed
// set is worse than no answer, because it looks like a verdict.
func TestEnforceStartGate_UnconfirmedStarterLinkRequeues(t *testing.T) {
	cases := []struct {
		name  string
		conds []metav1.Condition
	}{
		{name: "condition absent: not yet reconciled", conds: nil},
		{
			name: "condition False: the write failed or was refused",
			conds: []metav1.Condition{{
				Type:               spiceboxv1alpha1.AgentClassConditionStartersLinked,
				Status:             metav1.ConditionFalse,
				Reason:             spiceboxv1alpha1.ReasonStartersLinkFailed,
				Message:            "spicedb unreachable",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+": requeue, not Failed", func(t *testing.T) {
			ac := unlinkedGatedClass("gatebot", nil)
			ac.Status.Conditions = tc.conds
			sess := gatedSession("gatebot", "listed")
			chk := &fakeStartChecker{allow: true}
			r, notices := newGateReconciler(t, sess, ac, chk)

			_, halted, err := r.EnforceStartGate(context.Background(), sess, ac)
			require.Error(t, err, "an unconfirmed starter set is returned so controller-runtime requeues with backoff")
			assert.True(t, halted)
			assert.Empty(t, chk.calls, "the checker must not be asked about a set the store may not hold")
			assert.Empty(t, *notices, "nobody is told they were refused — nothing was refused")

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "s1"}, &got))
			assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
				"indeterminate is NOT a refusal")
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStarterAllowed)
			require.NotNil(t, cond)
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionStartAuthzUnavailable, cond.Reason)
		})
	}

	t.Run("StartersLinked=True: the gate proceeds to the checker", func(t *testing.T) {
		ac := gatedClass("gatebot", nil)
		sess := gatedSession("gatebot", "listed")
		chk := &fakeStartChecker{allow: true}
		r, _ := newGateReconciler(t, sess, ac, chk)

		_, halted, err := r.EnforceStartGate(context.Background(), sess, ac)
		require.NoError(t, err)
		assert.False(t, halted)
		assert.Equal(t, []string{"default/gatebot#start_session@listed"}, chk.calls,
			"a confirmed set is the precondition, not a substitute for the check")
	})
}

func TestEnforceStartGate_NilCheckerFailsClosed(t *testing.T) {
	ac := gatedClass("gatebot", nil)
	sess := gatedSession("gatebot", "listed")
	r, _ := newGateReconciler(t, sess, ac, nil)
	r.StartChecker = nil
	_, halted, err := r.EnforceStartGate(context.Background(), sess, ac)
	require.NoError(t, err)
	assert.True(t, halted, "a gate with no checker refuses; it never waves through")
}

func TestEnforceStartGate_SkipsARunningSession(t *testing.T) {
	ac := gatedClass("gatebot", nil)
	sess := gatedSession("gatebot", "stranger")
	// podAlreadyStarted (controller.go:3350) is true for phase Running or a
	// RunnerReady condition of any status.
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	chk := &fakeStartChecker{allow: false}
	r, _ := newGateReconciler(t, sess, ac, chk)
	_, halted, err := r.EnforceStartGate(context.Background(), sess, ac)
	require.NoError(t, err)
	assert.False(t, halted, "the gate is about STARTING; a running session's standing is the Interact gate's job")
	assert.Empty(t, chk.calls)
}
