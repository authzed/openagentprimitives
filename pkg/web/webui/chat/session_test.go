package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
)

// --- watchSessionHealth ---------------------------------------------------
//
// These tests exercise the built-in chat's no-silent-hang guardian directly:
// a wedged/failed/deleted session must SURFACE a reason to the attached tabs
// (send_error and/or a terminal notice that drives the session_ended frame),
// never leave the browser's throbber spinning forever.

// healthRec records every emit + onTerminal the watcher produces.
type healthRec struct {
	mu        sync.Mutex
	emitted   []any
	terminals []terminalNotice
}

func (r *healthRec) emit(m any) { r.mu.Lock(); r.emitted = append(r.emitted, m); r.mu.Unlock() }
func (r *healthRec) onTerminal(tn terminalNotice) {
	r.mu.Lock()
	r.terminals = append(r.terminals, tn)
	r.mu.Unlock()
}
func (r *healthRec) snapshot() ([]any, []terminalNotice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.emitted...), append([]terminalNotice(nil), r.terminals...)
}

// sendErrorsOf keeps only the channel-health warnings out of everything the
// watcher emitted. The startup line rides the same emit on every pre-start
// tick, so "no warning" is a claim about MsgSendError, never about emit
// having stayed silent.
func sendErrorsOf(emitted []any) []browser.MsgSendError {
	var out []browser.MsgSendError
	for _, e := range emitted {
		if se, ok := e.(browser.MsgSendError); ok {
			out = append(out, se)
		}
	}
	return out
}

// startupsOf keeps only the startup-line frames, in emit order.
func startupsOf(emitted []any) []sessionStartupMsg {
	var out []sessionStartupMsg
	for _, e := range emitted {
		if m, ok := e.(sessionStartupMsg); ok {
			out = append(out, m)
		}
	}
	return out
}

// fastPoll shrinks the watcher's timing knobs so a test runs in milliseconds,
// restoring them on cleanup. Callers must NOT run in parallel (package vars).
func fastPoll(t *testing.T, poll, grace time.Duration) {
	t.Helper()
	op, og := sessionPollInterval, startupGrace
	sessionPollInterval, startupGrace = poll, grace
	t.Cleanup(func() { sessionPollInterval, startupGrace = op, og })
}

// setRunnerGrace overrides the runner bring-up ceiling for one test, restoring
// it on cleanup. Like fastPoll, callers must NOT run in parallel (package var).
func setRunnerGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := runnerStartupGrace
	runnerStartupGrace = d
	t.Cleanup(func() { runnerStartupGrace = old })
}

func chatSession(name, phase string, conds ...metav1.Condition) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: newChatSessionNamespace}}
	s.Status.Phase = phase
	s.Status.Conditions = conds
	return s
}

func invalidChannel(name, reason, message string) *spiceboxv1alpha1.Channel {
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: newChatSessionNamespace}}
	ch.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.ChannelConditionValid, Status: metav1.ConditionFalse,
		Reason: reason, Message: message,
	}}
	return ch
}

// runHealth launches watchSessionHealth against k8s in a goroutine and returns
// the recorder plus a stop the test defers. stop cancels the context AND blocks
// until the goroutine has fully exited — so the goroutine can no longer read the
// package timing vars (sessionPollInterval / startupGrace) that fastPoll's
// cleanup restores after the test returns, which would otherwise race.
func runHealth(t *testing.T, k8s client.Client, name string) (*healthRec, context.CancelFunc) {
	t.Helper()
	rec := &healthRec{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSessionHealth(ctx, &fakeDeps{k8s: k8s}, newChatSessionNamespace, name, name+"-chan", rec.emit, rec.onTerminal)
	}()
	return rec, func() {
		cancel()
		<-done
	}
}

func TestWatchSessionHealth_FailedPhase_FiresTerminalWithReason(t *testing.T) {
	fastPoll(t, 5*time.Millisecond, time.Hour)
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhaseFailed,
		metav1.Condition{Type: spiceboxv1alpha1.AgentSessionConditionFailed, Status: metav1.ConditionTrue, Message: "ran out of budget"})
	sess.Status.FailureReason = "BudgetExceeded"
	k8s := newFakeK8sClient(t, sess)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { _, terms := rec.snapshot(); return len(terms) == 1 }, time.Second, 5*time.Millisecond)
	_, terms := rec.snapshot()
	assert.Equal(t, "failed", terms[0].Reason)
	assert.Equal(t, "BudgetExceeded", terms[0].FailureReason)
	assert.Equal(t, "ran out of budget", terms[0].FailureMessage, "the Failed condition message must reach the terminal notice")
}

func TestWatchSessionHealth_Succeeded_FiresTerminal(t *testing.T) {
	fastPoll(t, 5*time.Millisecond, time.Hour)
	k8s := newFakeK8sClient(t, chatSession("s1", spiceboxv1alpha1.AgentSessionPhaseSucceeded))

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { _, terms := rec.snapshot(); return len(terms) == 1 }, time.Second, 5*time.Millisecond)
	_, terms := rec.snapshot()
	assert.Equal(t, "succeeded", terms[0].Reason)
}

func TestWatchSessionHealth_Deleted_FiresTerminalSessionRemoved(t *testing.T) {
	// No session seeded: the first Get returns NotFound, standing in for a
	// session deleted out from under the chat (operator GC of a session whose
	// Channel never validated). Must surface, not spin on NotFound forever.
	fastPoll(t, 5*time.Millisecond, time.Hour)
	k8s := newFakeK8sClient(t)

	rec, cancel := runHealth(t, k8s, "gone")
	defer cancel()

	require.Eventually(t, func() bool { _, terms := rec.snapshot(); return len(terms) == 1 }, time.Second, 5*time.Millisecond)
	_, terms := rec.snapshot()
	assert.Equal(t, "failed", terms[0].Reason)
	assert.Equal(t, "SessionRemoved", terms[0].FailureReason)
	assert.NotEmpty(t, terms[0].FailureMessage)
}

func TestWatchSessionHealth_InvalidChannel_EmitsSendError(t *testing.T) {
	// Long grace so the terminal stuck-timeout doesn't fire during the test —
	// we're asserting the non-terminal channel-invalid send_error only.
	fastPoll(t, 5*time.Millisecond, time.Hour)
	k8s := newFakeK8sClient(t,
		chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending),
		invalidChannel("s1-chan", spiceboxv1alpha1.ReasonChannelSecretMissing, "secret s1-chan-creds not found"),
	)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool {
		emitted, _ := rec.snapshot()
		return len(sendErrorsOf(emitted)) >= 1
	}, time.Second, 5*time.Millisecond)

	emitted, terms := rec.snapshot()
	assert.Empty(t, terms, "an invalid channel within the grace window must NOT (yet) be terminal")
	se := sendErrorsOf(emitted)[0]
	assert.Contains(t, se.Err, "channel invalid")
	assert.Contains(t, se.Err, "SecretMissing")
	assert.Contains(t, se.Err, "secret s1-chan-creds not found")

	// Deduped: a persistently-invalid channel warns once, not every tick.
	time.Sleep(40 * time.Millisecond)
	emitted, _ = rec.snapshot()
	assert.Len(t, sendErrorsOf(emitted), 1, "the channel-invalid warning must be emitted exactly once")
}

func TestWatchSessionHealth_StuckPending_FiresTerminalCouldntStart(t *testing.T) {
	// Pending forever with an invalid channel: past the (tiny) grace the watcher
	// must fail the session with the channel reason folded in, so the throbber
	// stops with a real explanation.
	fastPoll(t, 5*time.Millisecond, 30*time.Millisecond)
	k8s := newFakeK8sClient(t,
		chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending),
		invalidChannel("s1-chan", spiceboxv1alpha1.ReasonChannelSpecInvalid, "role must be one of input|output|both"),
	)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { _, terms := rec.snapshot(); return len(terms) == 1 }, time.Second, 5*time.Millisecond)
	_, terms := rec.snapshot()
	assert.Equal(t, "failed", terms[0].Reason)
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelSpecInvalid, terms[0].FailureReason)
	assert.Contains(t, terms[0].FailureMessage, "couldn't start")
	assert.Contains(t, terms[0].FailureMessage, "SpecInvalid")
}

func TestWatchSessionHealth_StuckPending_NoChannelDetail_GenericReason(t *testing.T) {
	// Pending with a readable, valid channel and no False conditions: the stuck
	// timeout still fires, with a generic StartupTimeout reason.
	fastPoll(t, 5*time.Millisecond, 30*time.Millisecond)
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "s1-chan", Namespace: newChatSessionNamespace}}
	ch.Status.Conditions = []metav1.Condition{{Type: spiceboxv1alpha1.ChannelConditionValid, Status: metav1.ConditionTrue}}
	k8s := newFakeK8sClient(t, chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending), ch)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { _, terms := rec.snapshot(); return len(terms) == 1 }, time.Second, 5*time.Millisecond)
	emitted, terms := rec.snapshot()
	assert.Empty(t, sendErrorsOf(emitted), "a valid channel must not produce a channel-invalid warning")
	assert.Equal(t, "StartupTimeout", terms[0].FailureReason)
	assert.True(t, strings.Contains(terms[0].FailureMessage, "couldn't start"))
}

func TestWatchSessionHealth_StuckPending_NegativePolarityConditionNotReportedAsReason(t *testing.T) {
	// A session slow to leave Pending (e.g. a sidecar still starting) commonly has
	// its negative-polarity conditions ALREADY resolved — ScopeReviewPending=False
	// (Resolved), ToolCallGated=False (NoDenials) — while the positive readiness
	// gates (RunnerReady, ...) are simply absent, not False. The stuck timeout must
	// NOT surface one of those healthy conditions as the failure reason.
	//
	// Regression: firstNotReadyCondition's old catch-all "any False condition"
	// fallback reported "couldn't start (Resolved)" for a session that then went on
	// to run fine — a false failure with a nonsensical reason. With no positive
	// readiness gate False, it must fall back to the generic StartupTimeout.
	fastPoll(t, 5*time.Millisecond, 30*time.Millisecond)
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "s1-chan", Namespace: newChatSessionNamespace}}
	ch.Status.Conditions = []metav1.Condition{{Type: spiceboxv1alpha1.ChannelConditionValid, Status: metav1.ConditionTrue}}
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending,
		metav1.Condition{Type: spiceboxv1alpha1.AgentSessionConditionClassResolved, Status: metav1.ConditionTrue, Reason: "AllReferencesResolve"},
		metav1.Condition{Type: spiceboxv1alpha1.AgentSessionConditionScopeReviewPending, Status: metav1.ConditionFalse, Reason: "Resolved"},
	)
	k8s := newFakeK8sClient(t, sess, ch)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { _, terms := rec.snapshot(); return len(terms) == 1 }, time.Second, 5*time.Millisecond)
	_, terms := rec.snapshot()
	assert.Equal(t, "StartupTimeout", terms[0].FailureReason,
		"a healthy negative-polarity condition (ScopeReviewPending=Resolved) must never be the startup-failure reason")
	assert.NotContains(t, terms[0].FailureMessage, "Resolved",
		"the browser must not be told the session couldn't start because a review was Resolved")
}

func TestWatchSessionHealth_StuckPending_GateReasonRendersFriendly(t *testing.T) {
	// A session wedged on a positive readiness gate must explain itself in
	// human terms: the machine token stays in FailureReason (for programmatic
	// consumers) but must not appear in the message a browser user reads —
	// the friendly phrase does instead.
	fastPoll(t, 5*time.Millisecond, 30*time.Millisecond)
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "s1-chan", Namespace: newChatSessionNamespace}}
	ch.Status.Conditions = []metav1.Condition{{Type: spiceboxv1alpha1.ChannelConditionValid, Status: metav1.ConditionTrue}}
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending,
		metav1.Condition{Type: spiceboxv1alpha1.AgentSessionConditionSettingsAccepted, Status: metav1.ConditionFalse,
			Reason: "ModelMissingModel", Message: "no model resolved from any settings tier"},
	)
	k8s := newFakeK8sClient(t, sess, ch)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { _, terms := rec.snapshot(); return len(terms) == 1 }, time.Second, 5*time.Millisecond)
	_, terms := rec.snapshot()
	assert.Equal(t, "ModelMissingModel", terms[0].FailureReason, "the machine code stays available programmatically")
	assert.Contains(t, terms[0].FailureMessage, "couldn't start")
	assert.Contains(t, terms[0].FailureMessage, "no model is configured", "the friendly phrase is what the user reads")
	assert.NotContains(t, terms[0].FailureMessage, "ModelMissingModel", "raw machine token stays out of the user-facing message")
}

func TestWatchSessionHealth_RunnerCreating_NotTerminalWithinBringupGrace(t *testing.T) {
	// The runner pod has been spawned (RunnerReady=False/RunnerCreating) and is
	// coming up — image pull, scheduling, container start. Past the short
	// startupGrace but within the longer runner bring-up ceiling, the watcher
	// must NOT declare the session failed: a healthy cold start legitimately
	// exceeds 45s, and tearing it down here is the exact false "couldn't start
	// (RunnerCreating: runner spawned)" the two-tier grace fixes.
	fastPoll(t, 5*time.Millisecond, 20*time.Millisecond) // short grace tiny
	setRunnerGrace(t, time.Hour)                         // bring-up ceiling far away
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending,
		metav1.Condition{
			Type: spiceboxv1alpha1.AgentSessionConditionRunnerReady, Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ReasonRunnerCreating, Message: "runner spawned",
		})
	k8s := newFakeK8sClient(t, sess)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	time.Sleep(80 * time.Millisecond) // well past the 20ms short grace
	emitted, terms := rec.snapshot()
	assert.Empty(t, terms, "a spawned, still-coming-up runner must not be declared 'couldn't start' at the short grace")
	assert.Empty(t, sendErrorsOf(emitted), "no bound channel is invalid, so no warning is expected")
}

func TestWatchSessionHealth_RunnerCreating_FiresHonestTimeoutPastBringupGrace(t *testing.T) {
	// Past the (long) bring-up ceiling a runner that STILL hasn't come up is
	// surfaced — no silent hang — but with an honest "taking unusually long"
	// message, never the benign "RunnerCreating: runner spawned" progress reason
	// parroted back as a fatal cause.
	fastPoll(t, 5*time.Millisecond, 5*time.Millisecond) // short grace tiny
	setRunnerGrace(t, 30*time.Millisecond)              // ceiling small but > short grace
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending,
		metav1.Condition{
			Type: spiceboxv1alpha1.AgentSessionConditionRunnerReady, Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ReasonRunnerCreating, Message: "runner spawned",
		})
	k8s := newFakeK8sClient(t, sess)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { _, terms := rec.snapshot(); return len(terms) == 1 }, time.Second, 5*time.Millisecond)
	_, terms := rec.snapshot()
	assert.Equal(t, "failed", terms[0].Reason)
	assert.Equal(t, "RunnerStartupTimeout", terms[0].FailureReason)
	assert.Contains(t, terms[0].FailureMessage, "taking unusually long")
	assert.NotContains(t, terms[0].FailureMessage, "RunnerCreating",
		"the benign progress reason must never be surfaced as the failure cause")
	assert.NotContains(t, terms[0].FailureMessage, "runner spawned")
}

func TestWatchSessionHealth_PreStart_StartupLineCarriesTheGenericLead(t *testing.T) {
	// Nothing is False yet: the line says the platform's generic lead, so the
	// shell never shows a bare spinner, and it is not "still trying".
	fastPoll(t, 5*time.Millisecond, time.Hour)
	k8s := newFakeK8sClient(t, chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending))
	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { e, _ := rec.snapshot(); return len(startupsOf(e)) >= 1 }, time.Second, 5*time.Millisecond)
	e, _ := rec.snapshot()
	first := startupsOf(e)[0]
	assert.Equal(t, sessionnotice.GenericStartupLead, first.Text)
	assert.False(t, first.StillTrying)
	assert.False(t, first.Started)
	assert.Equal(t, browser.SessionRef{Namespace: newChatSessionNamespace, Name: "s1"}, first.Session)
}

func TestWatchSessionHealth_PreStart_StartupLineNamesTheBlocker(t *testing.T) {
	// A False gate: the line is the same friendly caption channelsd puts on a
	// channel thread, with the cluster's own words in it.
	fastPoll(t, 5*time.Millisecond, time.Hour)
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionSandboxScheduling, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonSandboxUnschedulable, Message: "0/1 nodes are available: 1 Insufficient memory.",
	})
	k8s := newFakeK8sClient(t, sess)
	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { e, _ := rec.snapshot(); return len(startupsOf(e)) >= 1 }, time.Second, 5*time.Millisecond)
	e, _ := rec.snapshot()
	assert.Contains(t, startupsOf(e)[0].Text, "Insufficient memory")
	assert.NotEmpty(t, startupsOf(e)[0].Short)
}

func TestWatchSessionHealth_PreStart_RepeatsTheLineEveryTick(t *testing.T) {
	// The socket has no replay and the browser fold nulls `startup` on
	// reconnect (RECONNECT_RESET, sessionSignals.ts), so a tab that reattaches
	// mid pre-start must get the line again on the very next tick, without
	// waiting for the blocker itself to change — hence every tick, not once.
	fastPoll(t, 5*time.Millisecond, time.Hour)
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionSandboxScheduling, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonSandboxUnschedulable, Message: "0/1 nodes are available: 1 Insufficient memory.",
	})
	k8s := newFakeK8sClient(t, sess)
	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	time.Sleep(30 * time.Millisecond) // at least three 5ms poll intervals
	e, _ := rec.snapshot()
	starts := startupsOf(e)
	require.GreaterOrEqual(t, len(starts), 2, "the line must repeat, not fire once and stop")
	for _, s := range starts[1:] {
		assert.Equal(t, starts[0].Text, s.Text, "an unchanged blocker repeats the same line, not a new one")
	}
}

func TestWatchSessionHealth_RunnerRefused_StillTryingPastShortGrace_NotTerminal(t *testing.T) {
	// A refused pod the operator keeps retrying is a runner coming up, from the
	// person's side: past the short grace the line says "still trying" and the
	// session is NOT declared dead — the long ceiling bounds it instead.
	//
	// The short grace is 50ms, not the 20ms a tight timing budget would prefer:
	// the FIRST tick's assertion below (starts[0].StillTrying must be false)
	// needs the watcher's own goroutine to actually run within the grace
	// window, and a scheduling stall on a loaded box can eat a 4x margin (20ms
	// grace against a 5ms poll) before that goroutine gets its first turn.
	fastPoll(t, 5*time.Millisecond, 50*time.Millisecond)
	setRunnerGrace(t, time.Hour)
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionRunnerReady, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonRunnerPodRefused, Message: "exceeded quota: workshop-limits",
	})
	k8s := newFakeK8sClient(t, sess)
	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	time.Sleep(80 * time.Millisecond) // well past the 50ms short grace
	e, terms := rec.snapshot()
	assert.Empty(t, terms, "a refused runner still being retried must not be declared 'couldn't start' at the short grace")
	starts := startupsOf(e)
	require.NotEmpty(t, starts)
	last := starts[len(starts)-1]
	assert.True(t, last.StillTrying, "past the short grace the line says still trying")
	assert.Contains(t, last.Text, "exceeded quota")
	assert.False(t, starts[0].StillTrying, "the first tick is within the short grace")
}

func TestWatchSessionHealth_RunnerRefused_TerminalPastCeilingNamesTheBlocker(t *testing.T) {
	// Past the long ceiling the session is surfaced as taking too long, and the
	// last line names what was in the way — unlike RunnerCreating, whose
	// benign progress reason is never parroted as a cause.
	fastPoll(t, 5*time.Millisecond, 5*time.Millisecond)
	setRunnerGrace(t, 30*time.Millisecond)
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionRunnerReady, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonRunnerPodRefused, Message: "exceeded quota: workshop-limits",
	})
	k8s := newFakeK8sClient(t, sess)
	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { _, terms := rec.snapshot(); return len(terms) == 1 }, time.Second, 5*time.Millisecond)
	_, terms := rec.snapshot()
	assert.Equal(t, "RunnerStartupTimeout", terms[0].FailureReason)
	assert.Contains(t, terms[0].FailureMessage, "taking unusually long")
	assert.Contains(t, terms[0].FailureMessage, "exceeded quota")
}

func TestWatchSessionHealth_Started_EmitsStartedOnceAndNoMoreLines(t *testing.T) {
	// The tick that first sees a started phase sends the one frame that ends
	// the shell's line; nothing startup-shaped follows, including across a
	// later wake back through Pending — the watcher has already latched
	// "started" for this session, so a Pending sighting after that is a wake
	// (Idle→Pending on a new inbound message or an artifact-view annotation),
	// not a fresh initial startup, and must not restart the line.
	fastPoll(t, 5*time.Millisecond, time.Hour)
	sess := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending)
	k8s := newFakeK8sClient(t, sess)
	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	require.Eventually(t, func() bool { e, _ := rec.snapshot(); return len(startupsOf(e)) >= 1 }, time.Second, 5*time.Millisecond)
	// Updated via the same fake client the watcher reads, Get-then-Update as
	// the wake test in this file does (the fake has no status subresource,
	// so Update carries the phase).
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, k8s.Get(context.Background(), client.ObjectKey{Namespace: newChatSessionNamespace, Name: "s1"}, &live))
	live.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	require.NoError(t, k8s.Update(context.Background(), &live))

	started := func() int {
		e, _ := rec.snapshot()
		n := 0
		for _, m := range startupsOf(e) {
			if m.Started {
				n++
			}
		}
		return n
	}
	require.Eventually(t, func() bool { return started() >= 1 }, time.Second, 5*time.Millisecond)
	e, _ := rec.snapshot()
	before := len(startupsOf(e))

	// The wake: back to Pending, well after the session has started.
	require.NoError(t, k8s.Get(context.Background(), client.ObjectKey{Namespace: newChatSessionNamespace, Name: "s1"}, &live))
	live.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
	require.NoError(t, k8s.Update(context.Background(), &live))

	time.Sleep(60 * time.Millisecond) // many more ticks, spanning the wake
	e, _ = rec.snapshot()
	assert.Equal(t, 1, started(), "started is sent exactly once")
	assert.Equal(t, before, len(startupsOf(e)), "a wake back through Pending does not restart the line")
}

func TestWatchSessionHealth_Running_DoesNotFireStuckOrChannelWarning(t *testing.T) {
	// A Running session is past the startup window: even with an invalid channel
	// record lingering, the watcher must not warn or declare it stuck.
	fastPoll(t, 5*time.Millisecond, 20*time.Millisecond)
	k8s := newFakeK8sClient(t,
		chatSession("s1", spiceboxv1alpha1.AgentSessionPhaseRunning),
		invalidChannel("s1-chan", spiceboxv1alpha1.ReasonChannelSecretMissing, "stale"),
	)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	time.Sleep(80 * time.Millisecond) // well past grace
	emitted, terms := rec.snapshot()
	assert.Empty(t, sendErrorsOf(emitted), "a started session must not surface the pre-Running channel warning")
	assert.Empty(t, terms, "a Running session must not be declared stuck")
}

func TestWatchSessionHealth_ParkedAwaitingUser_DoesNotFireStuckOrChannelWarning(t *testing.T) {
	// A session parked awaiting the user (AwaitingCredentials /
	// AwaitingIdentityChoice) has legitimately STARTED — it is waiting on a
	// human, not stuck in startup — so the watcher must never declare it
	// "couldn't start", even well past the startup grace and even with a stale
	// invalid-channel record lingering. Firing a terminal here is what turns a
	// credential/identity prompt into a false "conversation failed"; staying
	// quiet (and keeping the registry entry alive) is what lets the
	// interaction_request prompt reach the browser. This is the health-watcher
	// leg of the same invariant waitForSessionReady enforces at build time.
	cases := []struct {
		name  string
		phase string
	}{
		{name: "AwaitingCredentials stays alive", phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials},
		{name: "AwaitingIdentityChoice stays alive", phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastPoll(t, 5*time.Millisecond, 20*time.Millisecond)
			k8s := newFakeK8sClient(t,
				chatSession("s1", tc.phase),
				invalidChannel("s1-chan", spiceboxv1alpha1.ReasonChannelSecretMissing, "stale"),
			)

			rec, cancel := runHealth(t, k8s, "s1")
			defer cancel()

			time.Sleep(80 * time.Millisecond) // well past the tiny grace
			emitted, terms := rec.snapshot()
			assert.Empty(t, sendErrorsOf(emitted), "a parked-awaiting-user session must not surface the pre-Running channel warning")
			assert.Empty(t, terms, "a parked-awaiting-user session must never be declared stuck/failed")
		})
	}
}

func TestWatchSessionHealth_WakeAfterStarted_DoesNotFireCouldntStart(t *testing.T) {
	// A session that has ALREADY started (reached Running) and then returns to
	// Pending — woken from Idle by a new message or an artifact-view annotation
	// — must NOT be declared "couldn't start". The startup grace guards only the
	// INITIAL startup; re-entering Pending on a wake is normal. Firing the
	// terminal here is the "couldn't start (still pending)" regression the user
	// hit when annotating in the other view (the grace clock is measured from
	// watcher creation, so on a wake time.Since(start) is already far past it).
	fastPoll(t, 5*time.Millisecond, 20*time.Millisecond)
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "s1-chan", Namespace: newChatSessionNamespace}}
	ch.Status.Conditions = []metav1.Condition{{Type: spiceboxv1alpha1.ChannelConditionValid, Status: metav1.ConditionTrue}}
	k8s := newFakeK8sClient(t, chatSession("s1", spiceboxv1alpha1.AgentSessionPhaseRunning), ch)

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	// Let the watcher observe Running for several ticks (latch "has started"),
	// well past the tiny grace, and confirm it stays quiet.
	time.Sleep(60 * time.Millisecond)
	if _, terms := rec.snapshot(); len(terms) != 0 {
		t.Fatalf("a Running session must not be terminal, got %+v", terms)
	}

	// The session is woken back to Pending (Idle→Pending on a new inbound /
	// annotation), updated via the same fake client the watcher reads.
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, k8s.Get(context.Background(), client.ObjectKey{Namespace: newChatSessionNamespace, Name: "s1"}, &live))
	live.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
	require.NoError(t, k8s.Update(context.Background(), &live))

	// Well past the grace: a woken session must NOT be declared "couldn't start".
	time.Sleep(80 * time.Millisecond)
	_, terms := rec.snapshot()
	assert.Empty(t, terms, "a woken (Running→Pending) session must never be declared 'couldn't start'")
}

func TestWatchSessionHealth_IdlePhase_EmitsTurnCompleteOnce(t *testing.T) {
	// When the session settles into Idle (turn done, awaiting the next message),
	// the watcher emits a turn-complete so the browser clears its "working"
	// throbber — the authoritative backstop for the "session is idle but the UI
	// still shows working" bug. Emitted ONCE per Idle-entry, not every poll.
	fastPoll(t, 5*time.Millisecond, time.Hour) // long grace; not testing startup
	k8s := newFakeK8sClient(t, chatSession("s1", spiceboxv1alpha1.AgentSessionPhaseIdle))

	rec, cancel := runHealth(t, k8s, "s1")
	defer cancel()

	countTurnComplete := func() int {
		emitted, _ := rec.snapshot()
		n := 0
		for _, e := range emitted {
			if ta, ok := e.(browser.MsgTurnActivity); ok && !ta.Active {
				n++
			}
		}
		return n
	}
	require.Eventually(t, func() bool { return countTurnComplete() >= 1 }, time.Second, 5*time.Millisecond,
		"an Idle session must emit a turn-complete to clear the working throbber")
	time.Sleep(60 * time.Millisecond) // many more ticks
	assert.Equal(t, 1, countTurnComplete(), "turn-complete must be emitted once per Idle-entry, not every poll")
}

// --- boundListener ---------------------------------------------------------
//
// boundListener adapts a channelkind Listener (browser.Host.Listener()) to
// sessionListener: the underlying SubmitInterrupt takes ns/name explicitly,
// so boundListener supplies them from the chat session it is bound to and
// exposes the (ctx, requestID) shape Registry.SubmitInterrupt calls through
// sessionListener.

// fakeChannelListener is a scriptable channelListener.
type fakeChannelListener struct {
	gotMessageExt                channelkinds.ExternalIdentity
	gotNS, gotName, gotRequestID string
	gotInterruptExt              channelkinds.ExternalIdentity
	interruptErr                 error

	gotDecisionExt                                  channelkinds.ExternalIdentity
	gotDecisionNS, gotDecisionName                  string
	gotDecisionCategory, gotRequestRef, gotActionID string
	decisionErr                                     error

	gotResurfaceNS, gotResurfaceName string
	resurfaceErr                     error
}

func (f *fakeChannelListener) SubmitUserMessage(_ context.Context, ext channelkinds.ExternalIdentity, _, _ string) (channelkinds.InboundDecision, error) {
	f.gotMessageExt = ext
	return channelkinds.InboundDecision{}, nil
}

func (f *fakeChannelListener) SubmitInterrupt(_ context.Context, ext channelkinds.ExternalIdentity, ns, name, requestID string) error {
	f.gotInterruptExt = ext
	f.gotNS, f.gotName, f.gotRequestID = ns, name, requestID
	return f.interruptErr
}

func (f *fakeChannelListener) SubmitInteractionDecision(_ context.Context, ext channelkinds.ExternalIdentity, ns, name, category, requestRef, actionID string) error {
	f.gotDecisionExt = ext
	f.gotDecisionNS, f.gotDecisionName = ns, name
	f.gotDecisionCategory, f.gotRequestRef, f.gotActionID = category, requestRef, actionID
	return f.decisionErr
}

func (f *fakeChannelListener) SubmitResurface(_ context.Context, ns, name string) error {
	f.gotResurfaceNS, f.gotResurfaceName = ns, name
	return f.resurfaceErr
}

// TestBoundListener_SubmitResurface_SuppliesBoundNSAndName: the resurface
// request carries no session ref of its own (the envelope's ns/name IS the
// session), so boundListener supplying them is the whole contract — a wrong or
// empty pair asks channelsd to re-surface the wrong conversation, or none.
func TestBoundListener_SubmitResurface_SuppliesBoundNSAndName(t *testing.T) {
	fcl := &fakeChannelListener{}
	bl := &boundListener{l: fcl, ns: "default", sessionName: "sess-1"}

	require.NoError(t, bl.SubmitResurface(context.Background()))
	assert.Equal(t, "default", fcl.gotResurfaceNS)
	assert.Equal(t, "sess-1", fcl.gotResurfaceName)
}

func TestBoundListener_SubmitInterrupt_SuppliesBoundNSAndName(t *testing.T) {
	fcl := &fakeChannelListener{}
	bl := &boundListener{l: fcl, ns: "default", sessionName: "sess-1"}

	err := bl.SubmitInterrupt(context.Background(), channelkinds.ExternalIdentity{}, "req-1")
	require.NoError(t, err)
	assert.Equal(t, "default", fcl.gotNS)
	assert.Equal(t, "sess-1", fcl.gotName)
	assert.Equal(t, "req-1", fcl.gotRequestID)
}

func TestBoundListener_SubmitInterrupt_PropagatesError(t *testing.T) {
	fcl := &fakeChannelListener{interruptErr: assert.AnError}
	bl := &boundListener{l: fcl, ns: "default", sessionName: "sess-1"}

	err := bl.SubmitInterrupt(context.Background(), channelkinds.ExternalIdentity{}, "req-1")
	assert.ErrorIs(t, err, assert.AnError)
}

func TestBoundListener_SubmitDecision_SuppliesBoundNSAndName(t *testing.T) {
	fcl := &fakeChannelListener{}
	bl := &boundListener{l: fcl, ns: "default", sessionName: "sess-1"}

	err := bl.SubmitDecision(context.Background(), channelkinds.ExternalIdentity{}, "identity_choice", "req-1", "agent")
	require.NoError(t, err)
	assert.Equal(t, "default", fcl.gotDecisionNS)
	assert.Equal(t, "sess-1", fcl.gotDecisionName)
	assert.Equal(t, "identity_choice", fcl.gotDecisionCategory)
	assert.Equal(t, "req-1", fcl.gotRequestRef)
	assert.Equal(t, "agent", fcl.gotActionID)
}

func TestBoundListener_SubmitDecision_PropagatesError(t *testing.T) {
	fcl := &fakeChannelListener{decisionErr: assert.AnError}
	bl := &boundListener{l: fcl, ns: "default", sessionName: "sess-1"}

	err := bl.SubmitDecision(context.Background(), channelkinds.ExternalIdentity{}, "identity_choice", "req-1", "agent")
	assert.ErrorIs(t, err, assert.AnError)
}

// TestBoundListener_PassesCallersExtThroughUnmodified is part of the C1
// regression: boundListener holds no identity of its own (see its doc) — it
// must forward whatever ext each call supplies straight to the wrapped
// channelListener, never substituting a value it captured at construction
// time. One boundListener value is reused across all three calls below, each
// with a DIFFERENT ext, proving the value flows through per call rather than
// being fixed once.
func TestBoundListener_PassesCallersExtThroughUnmodified(t *testing.T) {
	fcl := &fakeChannelListener{}
	bl := &boundListener{l: fcl, ns: "default", sessionName: "sess-1"}

	alice := channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com"}
	bob := channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "bob@example.com", Email: "bob@example.com"}
	carol := channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "carol@example.com", Email: "carol@example.com"}

	_, err := bl.SubmitUserMessage(context.Background(), alice, "hi", "")
	require.NoError(t, err)
	assert.Equal(t, alice, fcl.gotMessageExt)

	require.NoError(t, bl.SubmitInterrupt(context.Background(), bob, "req-1"))
	assert.Equal(t, bob, fcl.gotInterruptExt)

	require.NoError(t, bl.SubmitDecision(context.Background(), carol, "identity_choice", "req-1", "agent"))
	assert.Equal(t, carol, fcl.gotDecisionExt)
}

// --- waitForSessionReady ---------------------------------------------------
//
// adoptRealSession blocks on waitForSessionReady until the operator has minted
// the per-session `<name>-memory-token` Secret. Its presence (not its contents)
// is the readiness signal; a cancelled/expired caller context must surface the
// wrapped ctx error rather than return nil (a false "ready") or spin to the 60s
// hard timeout.

func TestWaitForSessionReady_SecretPresentWithToken_ReturnsNil(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s1-memory-token", Namespace: newChatSessionNamespace},
		Data:       map[string][]byte{"token": []byte("read-only-token")},
	}
	k8s := newFakeK8sClient(t, sec)

	// The first poll finds the token Secret and returns immediately — no tick.
	require.NoError(t, waitForSessionReady(context.Background(), k8s, newChatSessionNamespace, "s1"))
}

func TestWaitForSessionReady_SecretPresentButTokenEmpty_NotReady(t *testing.T) {
	// The Secret exists but its `token` key is empty (operator created the object
	// before writing the token): NOT ready. With an already-cancelled context the
	// poll loop must surface the ctx error, never treat an empty token as ready.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s1-memory-token", Namespace: newChatSessionNamespace},
		Data:       map[string][]byte{"token": {}},
	}
	k8s := newFakeK8sClient(t, sec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForSessionReady(ctx, k8s, newChatSessionNamespace, "s1")
	require.Error(t, err, "an empty token must not be treated as ready")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWaitForSessionReady_CancelledContext_ReturnsWrappedCtxError(t *testing.T) {
	// No Secret seeded: the first Get misses, so the loop reaches its select and
	// observes the already-cancelled context — returning the wrapped ctx error
	// (not nil, and without waiting out the 60s hard timeout).
	k8s := newFakeK8sClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForSessionReady(ctx, k8s, newChatSessionNamespace, "s1")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "the caller's ctx error must be wrapped, not swallowed")
	assert.ErrorContains(t, err, "become ready", "the wrap must name what it was waiting on")
}

func TestWaitForSessionReady_ParkedAwaitingUserPhase_IsReady(t *testing.T) {
	// A session that parks awaiting the user (AwaitingCredentials /
	// AwaitingIdentityChoice) has, BY DESIGN, no runner and therefore no
	// <name>-memory-token Secret: the operator returns from reconcile at the
	// credential gate long before it mints one. The browser must still attach so
	// the credential / identity prompt renders — so a started-but-parked session
	// is ready. The already-cancelled context proves the phase is consulted
	// BEFORE the ctx: a session that IS ready must not be turned into a spurious
	// "context canceled" (which is what deleted the session, pre-fix).
	cases := []struct {
		name  string
		phase string
	}{
		{name: "AwaitingCredentials is ready", phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials},
		{name: "AwaitingIdentityChoice is ready", phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: newChatSessionNamespace},
			}
			sess.Status.Phase = tc.phase
			k8s := newFakeK8sClient(t, sess) // deliberately NO memory-token Secret
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			require.NoError(t, waitForSessionReady(ctx, k8s, newChatSessionNamespace, "s1"),
				"a started-but-parked session must be ready even with no memory-token Secret")
		})
	}
}

func TestWaitForSessionReady_PendingNoToken_StillNotReady(t *testing.T) {
	// The phase-aware readiness must NOT short-circuit a genuinely not-yet-
	// started session. Pending with no token is still "wait" — proven by the
	// cancelled context surfacing as the ctx error rather than a false ready.
	// This is the guard that keeps the AwaitingCredentials fix from over-
	// broadening into "any session is ready".
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: newChatSessionNamespace},
	}
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
	k8s := newFakeK8sClient(t, sess)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForSessionReady(ctx, k8s, newChatSessionNamespace, "s1")
	require.Error(t, err, "a Pending session with no token is not ready")
	assert.ErrorIs(t, err, context.Canceled)
}

// --- firstNotReadyCondition ------------------------------------------------
//
// firstNotReadyCondition returns the reason/message of the most informative
// POSITIVE readiness gate that is False, ordered most-actionable first. It must
// map each positive gate to its own reason, let the highest-priority gate win
// when several are False, and return "" for a session whose only False
// conditions are healthy negative-polarity ones (so stuckDetail falls back to
// the generic StartupTimeout instead of parroting a benign reason).

func TestFirstNotReadyCondition_PositiveGateMappingAndOrdering(t *testing.T) {
	falseCond := func(typ, reason, msg string) metav1.Condition {
		return metav1.Condition{Type: typ, Status: metav1.ConditionFalse, Reason: reason, Message: msg}
	}
	cases := []struct {
		name       string
		conds      []metav1.Condition
		wantReason string
		wantMsg    string
	}{
		{
			name:       "CredentialsReady=False/SecretMissing → SecretMissing + its message",
			conds:      []metav1.Condition{falseCond(spiceboxv1alpha1.AgentSessionConditionCredentialsReady, spiceboxv1alpha1.ReasonSecretMissing, "secret s1-chan-creds not found")},
			wantReason: spiceboxv1alpha1.ReasonSecretMissing,
			wantMsg:    "secret s1-chan-creds not found",
		},
		{
			name:       "RunnerReady=False with a non-RunnerCreating reason → that reason surfaced",
			conds:      []metav1.Condition{falseCond(spiceboxv1alpha1.AgentSessionConditionRunnerReady, spiceboxv1alpha1.ReasonAgentSessionRunnerCrash, "runner crashed")},
			wantReason: spiceboxv1alpha1.ReasonAgentSessionRunnerCrash,
			wantMsg:    "runner crashed",
		},
		{
			name: "several gates False → highest-priority (CredentialsReady) wins over later BundlesReady/RunnerReady",
			conds: []metav1.Condition{
				// Deliberately inserted out of priority order: FindStatusCondition
				// is type-keyed, so only the gates[] ordering may decide the winner.
				falseCond(spiceboxv1alpha1.AgentSessionConditionRunnerReady, spiceboxv1alpha1.ReasonAgentSessionRunnerCrash, "runner"),
				falseCond(spiceboxv1alpha1.AgentSessionConditionBundlesReady, "BundlesPending", "bundles"),
				falseCond(spiceboxv1alpha1.AgentSessionConditionCredentialsReady, spiceboxv1alpha1.ReasonSecretMissing, "creds"),
			},
			wantReason: spiceboxv1alpha1.ReasonSecretMissing,
			wantMsg:    "creds",
		},
		{
			name:       "only a healthy negative-polarity condition is False → empty (no false positive)",
			conds:      []metav1.Condition{{Type: spiceboxv1alpha1.AgentSessionConditionScopeReviewPending, Status: metav1.ConditionFalse, Reason: "Resolved"}},
			wantReason: "",
			wantMsg:    "",
		},
		{
			name:       "no conditions at all → empty",
			conds:      nil,
			wantReason: "",
			wantMsg:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := chatSession("s1", spiceboxv1alpha1.AgentSessionPhasePending, tc.conds...)
			gotReason, gotMsg := firstNotReadyCondition(s)
			assert.Equal(t, tc.wantReason, gotReason)
			assert.Equal(t, tc.wantMsg, gotMsg)
		})
	}
}

// --- deleteChatK8sObjects --------------------------------------------------
//
// deleteChatK8sObjects best-effort deletes a chat conversation's AgentSession,
// Channel, and creds Secret on teardown. A NotFound is legitimately suppressed
// (the object's already gone), but any OTHER delete error must be SURFACED
// (logged), never silently dropped (AGENTS.md §Never silently drop errors) —
// mirroring the rollback-delete guard above.

func TestDeleteChatK8sObjects_PresentObjects_DeletedQuietly(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: newChatSessionNamespace}}
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "s1-chan", Namespace: newChatSessionNamespace}}
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s1-chan-creds", Namespace: newChatSessionNamespace}}
	k8s := newFakeK8sClient(t, sess, ch, creds)

	var mu sync.Mutex
	var logs []string
	rec := funcr.New(func(_, args string) { mu.Lock(); logs = append(logs, args); mu.Unlock() }, funcr.Options{})
	d := &fakeDeps{k8s: k8s, logger: rec}

	deleteChatK8sObjects(context.Background(), d, sess, ch, creds)

	mu.Lock()
	assert.Empty(t, logs, "a clean teardown of present objects must not log any failure")
	mu.Unlock()

	assertGone := func(obj client.Object, name string) {
		err := k8s.Get(context.Background(), client.ObjectKey{Namespace: newChatSessionNamespace, Name: name}, obj)
		assert.True(t, apierrors.IsNotFound(err), "%s must be deleted, got err=%v", name, err)
	}
	assertGone(&spiceboxv1alpha1.AgentSession{}, "s1")
	assertGone(&spiceboxv1alpha1.Channel{}, "s1-chan")
	assertGone(&corev1.Secret{}, "s1-chan-creds")
}

func TestDeleteChatK8sObjects_NonNotFoundDeleteError_IsLogged(t *testing.T) {
	// Every Delete fails with a non-NotFound error: all three failures must be
	// logged with the error attached, not swallowed.
	delErr := errors.New("injected delete failure")
	k8s := newFakeInterceptedK8sClient(t, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return delErr
		},
	})

	var mu sync.Mutex
	var logs []string
	rec := funcr.New(func(_, args string) { mu.Lock(); logs = append(logs, args); mu.Unlock() }, funcr.Options{})
	d := &fakeDeps{k8s: k8s, logger: rec}

	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: newChatSessionNamespace}}
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "s1-chan", Namespace: newChatSessionNamespace}}
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s1-chan-creds", Namespace: newChatSessionNamespace}}

	deleteChatK8sObjects(context.Background(), d, sess, ch, creds)

	mu.Lock()
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	assert.Contains(t, joined, "delete AgentSession failed during teardown")
	assert.Contains(t, joined, "delete Channel failed during teardown")
	assert.Contains(t, joined, "delete creds Secret failed during teardown")
	assert.Contains(t, joined, delErr.Error(),
		"the delete error must reach the log so an operator can locate the failure")
}

// --- audit-forgery regression guard ---------------------------------------
//
// webd's memory token is READ-ONLY by design (pkg/memory/tokens/tokens.go:
// 146-155). Before this change, chat read the per-session Ed25519
// audit-signing seed out of the <name>-memory-token Secret and signed
// inbound turns as the session publisher — meaning a browser-facing process
// could forge a session's audit chain. These two tests are a durable
// guarantee that webd can never sign again.

func TestChatPackageNeverImportsProvenance(t *testing.T) {
	// NeedDeps + packages.Visit walks the FULL transitive import graph, not just
	// pkgs[0].Imports (direct imports only). A direct-only check would miss
	// provenance reintroduced one hop away — e.g. a new helper package under
	// pkg/web/webui/chat/... that imports provenance and is in turn imported by
	// session.go. This package is browser-facing: it must not pull in the
	// audit-signing package at ANY depth.
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps},
		"github.com/authzed/openagentprimitives/pkg/web/webui/chat")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)

	const forbidden = "github.com/authzed/openagentprimitives/pkg/memory/provenance"
	packages.Visit(pkgs, func(p *packages.Package) bool {
		for imp := range p.Imports {
			assert.NotEqual(t, forbidden, imp,
				"pkg/web/webui/chat transitively imports provenance via %s: a browser-facing "+
					"package must not pull in the audit-signing package at any depth", p.PkgPath)
		}
		return true
	}, nil)
}

func TestChatNeverReadsTheAuditSigningKey(t *testing.T) {
	// Glob every non-test .go file in the package directory rather than
	// hardcoding session.go: a future file that reads the Secret's
	// audit-signing-key literal must not be able to escape this guard just by
	// living somewhere else in the package. _test.go files are excluded
	// because this test file necessarily contains the literal in its own
	// assertion string.
	matches, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected at least one .go file in pkg/web/webui/chat")

	checked := 0
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.NotContains(t, string(b), "audit-signing-key",
			"webd must not read a session's Ed25519 audit-signing seed (found literal in %s)", name)
		checked++
	}
	require.Positive(t, checked, "expected at least one non-test .go file to be checked")
}
