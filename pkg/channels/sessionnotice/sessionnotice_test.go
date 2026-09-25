package sessionnotice_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
)

func sess(phase string, conds ...metav1.Condition) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{}
	s.Status.Phase = phase
	s.Status.Conditions = conds
	return s
}

func cond(t string, st metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: t, Status: st, Reason: reason, Message: msg, LastTransitionTime: metav1.Now()}
}

func kinds(ns []sessionnotice.Notice) []sessionnotice.Kind {
	out := make([]sessionnotice.Kind, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Kind)
	}
	return out
}

// A session parked in AwaitingRetry is waiting on the USER, and must say so
// AND say what to do — "paused" with no way out leaves them stuck.
func TestDerive_AwaitingRetryNamesTheWayOut(t *testing.T) {
	got := sessionnotice.Derive(sess(spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry,
		cond(spiceboxv1alpha1.AgentSessionConditionAwaitingRetry, metav1.ConditionTrue,
			"ProviderErr", "anthropic: stream: connection reset by peer")))

	require.Len(t, got, 1)
	assert.Equal(t, sessionnotice.KindAwaitingRetry, got[0].Kind)
	assert.Equal(t, sessionnotice.ToneDegraded, got[0].Tone, "a state needing the user is not routine")
	assert.NotEmpty(t, got[0].NextStep, "a paused session must say how to resume")
	assert.Contains(t, got[0].Body, "connection reset", "the underlying cause travels for the curious")
}

// A session that ends in Failed died WITHOUT the agent ever delivering a
// closing message — an eviction, a memory outage, a crash at boot. The
// transcript just stops, so the "terminal sessions yield nothing" rule (which
// assumes the agent's own final message already closed the thread) would leave
// the user staring at silence. The failure must surface, with a friendly lead
// mapped from the reason token and the underlying detail carried along.
func TestDerive_TerminalFailure_SurfacesFriendlyNotice(t *testing.T) {
	s := sess(spiceboxv1alpha1.AgentSessionPhaseFailed,
		cond(spiceboxv1alpha1.AgentSessionConditionFailed, metav1.ConditionTrue,
			spiceboxv1alpha1.ReasonAgentSessionBundleFail,
			"bundle SpiceboxSession failed: The node was low on resource: ephemeral-storage"))
	s.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionBundleFail

	got := sessionnotice.Derive(s)
	require.Len(t, got, 1)
	assert.Equal(t, sessionnotice.KindFailed, got[0].Kind)
	assert.Equal(t, sessionnotice.ToneFailed, got[0].Tone, "a terminal failure is neither routine nor merely degraded")
	assert.NotEmpty(t, got[0].Lead, "a failed session must state that it failed")
	assert.NotContains(t, got[0].Lead, "BundleFailed", "the lead is friendly text, not the raw reason token")
	assert.Contains(t, got[0].Body, "ephemeral-storage", "the underlying detail travels for the curious")
}

// A session that ends in Succeeded closed itself with the agent's own final
// message, so a banner repeating it would say the same thing twice. Terminal
// SUCCESS stays silent — the exception is failure, not every terminal state.
func TestDerive_TerminalSuccessStaysSilent(t *testing.T) {
	got := sessionnotice.Derive(sess(spiceboxv1alpha1.AgentSessionPhaseSucceeded))
	assert.Empty(t, got, "a graceful completion needs no status banner")
}

// A benign startup wait is ROUTINE and has no next step — there is nothing for
// the user to do but wait, and inventing advice a surface cannot honour is
// worse than silence.
func TestDerive_StartupWaitIsRoutineWithNoAction(t *testing.T) {
	got := sessionnotice.Derive(sess(spiceboxv1alpha1.AgentSessionPhasePending,
		cond(spiceboxv1alpha1.AgentSessionConditionRunnerReady, metav1.ConditionFalse,
			spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector, "")))

	require.Len(t, got, 1)
	assert.Equal(t, sessionnotice.KindStartup, got[0].Kind)
	assert.Equal(t, sessionnotice.ToneRoutine, got[0].Tone)
	assert.Empty(t, got[0].NextStep)
	assert.Contains(t, got[0].Lead, "security scanner", "the specific reason beats the generic text when known")
}

// A RunnerReady=False reason that is NOT a benign startup wait (a crash) must
// yield nothing here — it keeps its normal failure handling instead of being
// dressed up as "starting up".
func TestDerive_NonStartupRunnerFailureIsNotAWaitNotice(t *testing.T) {
	got := sessionnotice.Derive(sess(spiceboxv1alpha1.AgentSessionPhaseRunning,
		cond(spiceboxv1alpha1.AgentSessionConditionRunnerReady, metav1.ConditionFalse,
			"RunnerCrashed", "the runner exited")))
	assert.Empty(t, got, "a crash is not a bounded startup wait")
}

// Scheduling pressure only matters while something is in flight. A parked or
// terminal session has nothing for the user to be waiting on.
func TestDerive_SchedulingOnlyWhileInFlight(t *testing.T) {
	sched := cond(spiceboxv1alpha1.AgentSessionConditionSandboxScheduling,
		metav1.ConditionFalse, "Unschedulable", "0/1 nodes available")

	for _, phase := range []string{
		spiceboxv1alpha1.AgentSessionPhasePending,
		spiceboxv1alpha1.AgentSessionPhaseRunning,
	} {
		assert.Contains(t, kinds(sessionnotice.Derive(sess(phase, sched))),
			sessionnotice.KindScheduling, "phase %s has work in flight", phase)
	}
	for _, phase := range []string{
		spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		spiceboxv1alpha1.AgentSessionPhaseIdle,
	} {
		assert.NotContains(t, kinds(sessionnotice.Derive(sess(phase, sched))),
			sessionnotice.KindScheduling, "phase %s has nothing in flight", phase)
	}
}

// The actionable notice leads. A session both parked and mid-startup should not
// bury the one thing the user can act on under a wait they can only watch.
func TestDerive_ActionableNoticeComesFirst(t *testing.T) {
	got := sessionnotice.Derive(sess(spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry,
		cond(spiceboxv1alpha1.AgentSessionConditionAwaitingRetry, metav1.ConditionTrue, "ProviderErr", "reset"),
		cond(spiceboxv1alpha1.AgentSessionConditionRunnerReady, metav1.ConditionFalse,
			spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector, "")))

	require.NotEmpty(t, got)
	assert.Equal(t, sessionnotice.KindAwaitingRetry, got[0].Kind)
}

// A healthy session says nothing. A surface that always shows a banner trains
// people to skim past the one that matters.
func TestDerive_HealthySessionIsSilent(t *testing.T) {
	assert.Empty(t, sessionnotice.Derive(sess(spiceboxv1alpha1.AgentSessionPhaseRunning)))
	assert.Empty(t, sessionnotice.Derive(nil), "a nil session is not a panic")
}
