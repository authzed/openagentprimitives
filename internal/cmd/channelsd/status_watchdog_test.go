package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/watchdog"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// The timing/deadline behaviour (Touch advances, Extend's 2× fudge + cap,
// forward-only, warn/timeout thresholds, alarm re-arm) is owned by the pure
// machine and tested in pkg/channels/channelsd/watchdog. These tests pin the cmd-side
// glue: the status→wait classification and the signal delegation.

// TestWaitFromStatus is the single mapping from AgentSession status to watchdog
// policy. It encodes the visibility rule: an info-leakage approval is invisible
// to the requester (announce), while tool/permission approvals and idle/await
// phases are visible in-channel (suppress).
func TestWaitFromStatus(t *testing.T) {
	// leak builds n info_leakage entries on the generic PendingInteractions list
	// (the single home for every approval family since Slice C2).
	leak := func(n int) []spiceboxv1alpha1.PendingInteraction {
		out := make([]spiceboxv1alpha1.PendingInteraction, n)
		for i := range out {
			out[i] = spiceboxv1alpha1.PendingInteraction{Category: categories.InfoLeakage}
		}
		return out
	}
	cases := []struct {
		name string
		sess *spiceboxv1alpha1.AgentSession
		want watchdog.Wait
	}{
		{"empty/running → active", &spiceboxv1alpha1.AgentSession{}, watchdog.WaitActive},
		{"explicit Running → active", sessWithPhase(spiceboxv1alpha1.AgentSessionPhaseRunning), watchdog.WaitActive},
		{
			"pending info_leakage interaction → leakage (invisible; announce)",
			&spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{PendingInteractions: leak(1)}},
			watchdog.WaitLeakage,
		},
		{
			"pending tool_approval interaction → visible (in-thread; suppress)",
			&spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{
				PendingInteractions: []spiceboxv1alpha1.PendingInteraction{{RequestID: "tg1", Category: categories.ToolApproval}},
			}},
			watchdog.WaitVisible,
		},
		{
			"pending requester → visible (in-thread; suppress)",
			&spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{
				PendingRequesters: []spiceboxv1alpha1.PendingRequester{{RequestRef: "r1"}},
			}},
			watchdog.WaitVisible,
		},
		{
			// content_inspection parks on the generic PendingInteractions list
			// (same visibility class as tool_approval): an in-thread/DM-fallback
			// approval prompt the surface already shows, so WaitVisible.
			"pending content_inspection interaction (generic list) → visible (in-thread; suppress)",
			&spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{
				PendingInteractions: []spiceboxv1alpha1.PendingInteraction{
					{RequestID: "ci1", Category: categories.ContentInspection},
				},
			}},
			watchdog.WaitVisible,
		},
		{
			"leakage outranks tool_approval (most-invisible wins)",
			&spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{
				PendingInteractions: []spiceboxv1alpha1.PendingInteraction{
					{RequestID: "tg1", Category: categories.ToolApproval},
					{RequestID: "lk1", Category: categories.InfoLeakage},
				},
			}},
			watchdog.WaitLeakage,
		},
		{
			"leakage outranks awaiting-user-input (most-invisible wins)",
			&spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{
				PendingInteractions:    leak(1),
				AwaitingUserInputSince: &metav1.Time{Time: time.Unix(1, 0)},
			}},
			watchdog.WaitLeakage,
		},
		{
			// Regression: the leakage scan must run BEFORE folding the rest of the
			// list into WaitVisible. A session can hold both a content_inspection
			// entry (visible, in-thread) and an info_leakage entry (invisible,
			// ephemeral) at once — leakage must still win so the watchdog announces
			// it instead of silently suppressing on the visible arm.
			"leakage outranks pending content_inspection interaction (most-invisible wins)",
			&spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{
				PendingInteractions: []spiceboxv1alpha1.PendingInteraction{
					{RequestID: "ci1", Category: categories.ContentInspection},
					{RequestID: "lk1", Category: categories.InfoLeakage},
				},
			}},
			watchdog.WaitLeakage,
		},
		{"idle phase → visible (the agent posted a reply)", sessWithPhase(spiceboxv1alpha1.AgentSessionPhaseIdle), watchdog.WaitVisible},
		{"awaiting-retry → visible (retry button shown)", sessWithPhase(spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry), watchdog.WaitVisible},
		{"awaiting-credentials → visible (link flow shown)", sessWithPhase(spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials), watchdog.WaitVisible},
		{"succeeded → visible (terminal)", sessWithPhase(spiceboxv1alpha1.AgentSessionPhaseSucceeded), watchdog.WaitVisible},
		{
			"runner-ready=false/awaiting-detector → visible (startup wait; suppress 'taking longer')",
			sessWithRunnerReady(metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector),
			watchdog.WaitVisible,
		},
		{
			"runner-ready=false/runner-crashed → active (NOT a benign startup wait)",
			sessWithRunnerReady(metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentSessionRunnerCrash),
			watchdog.WaitActive,
		},
		{
			"awaiting-user-input scalar → visible (yielded; suppress 'taking longer')",
			&spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{
				Phase:                  spiceboxv1alpha1.AgentSessionPhaseRunning,
				AwaitingUserInputSince: &metav1.Time{Time: time.Unix(1, 0)},
			}},
			watchdog.WaitVisible,
		},
		{
			"running without await marker → active (unchanged)",
			sessWithPhase(spiceboxv1alpha1.AgentSessionPhaseRunning),
			watchdog.WaitActive,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, waitFromStatus(tc.sess))
		})
	}
}

func sessWithPhase(p string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{Phase: p}}
}

func sessWithRunnerReady(status metav1.ConditionStatus, reason string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{Status: spiceboxv1alpha1.AgentSessionStatus{
		Conditions: []metav1.Condition{{
			Type:   spiceboxv1alpha1.AgentSessionConditionRunnerReady,
			Status: status,
			Reason: reason,
		}},
	}}
}

// TestStartupWait pins the generic startup-wait detection: a RunnerReady=False
// reason in startupWaitReasons yields its notice (today the security-scanner
// text for AwaitingDetector); a non-startup RunnerReady=False reason, a ready
// runner, and a missing condition all yield ok=false. The map is the single
// extension point for future startup dependencies.
func TestStartupWait(t *testing.T) {
	cases := []struct {
		name       string
		sess       *spiceboxv1alpha1.AgentSession
		wantOK     bool
		wantNotice string
	}{
		{
			"awaiting-detector → startup notice",
			sessWithRunnerReady(metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector),
			true, startupWaitReasons[spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector],
		},
		{"runner-crashed → not a startup wait", sessWithRunnerReady(metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentSessionRunnerCrash), false, ""},
		{"runner-ready=true → not a startup wait", sessWithRunnerReady(metav1.ConditionTrue, ""), false, ""},
		{"no RunnerReady condition → not a startup wait", &spiceboxv1alpha1.AgentSession{}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notice, ok := startupWait(tc.sess)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantNotice, notice)
		})
	}
}

// TestApplyEventGatesAndClears pins the unified entrypoint: a caption forwards
// and arms the window; a yield clears the indicator and disarms (Yielded); a
// subsequent out-of-order (lower-Seq) caption is dropped because the yield
// state persists. Also proves that a Seq==0 (unordered/legacy) caption forwards
// even when LastSeq>0, but is still suppressed by a yield. This is the gate
// the whole status-signaling fix exists for.
func TestApplyEventGatesAndClears(t *testing.T) {
	wd := newStatusWatchdog(nil, nil)
	ctx := context.Background()

	// caption forwards and arms (LastSeq=10)
	r := wd.ApplyEvent(ctx, "ns", "s", watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 10, UID: "u"})
	assert.True(t, r.Forward, "fresh caption forwards")

	// Seq==0 (unordered/legacy, e.g. "Continuing…" resume) forwards even though
	// LastSeq is now 10 — it is exempt from the stale-Seq ordering gate.
	r = wd.ApplyEvent(ctx, "ns", "s", watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 0, UID: "u"})
	assert.True(t, r.Forward, "Seq==0 caption forwards despite LastSeq>0")

	// yield clears the indicator + disarms (Yielded)
	r = wd.ApplyEvent(ctx, "ns", "s", watchdog.Event{Kind: watchdog.EvTurnActivity, Seq: 20, UID: "u", Active: false, Cause: channelevents.PauseCauseReply})
	assert.True(t, r.ClearCaption, "yield clears the indicator")

	// Seq==0 post-yield is still suppressed — yield gates ALL captions including
	// the unordered class.
	r = wd.ApplyEvent(ctx, "ns", "s", watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 0, UID: "u"})
	assert.False(t, r.Forward, "Seq==0 caption suppressed after yield")

	// stale (lower-Seq) post-yield caption now dropped — tracking persisted as
	// Yielded, so the machine gates it instead of re-arming a fresh state.
	r = wd.ApplyEvent(ctx, "ns", "s", watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 5, UID: "u"})
	assert.False(t, r.Forward, "stale post-yield caption dropped")
}

// TestWatchdogTouchCreatesAndArms verifies Touch delegates to the machine:
// it creates tracking state set to "now" so the warn window starts fresh.
func TestWatchdogTouchCreatesAndArms(t *testing.T) {
	wd := newStatusWatchdog(nil, nil)
	wd.Touch("ns", "sess")

	wd.mu.Lock()
	defer wd.mu.Unlock()
	st, ok := wd.state["ns/sess"]
	require.True(t, ok, "Touch must create tracking state")
	assert.WithinDuration(t, time.Now(), st.LastActivity, 2*time.Second)
}

// TestWatchdogExtendCreatesAndPushesDeadline verifies Extend delegates to the
// machine and applies the fudge: a 60s hint pushes the warn out to ~120s.
func TestWatchdogExtendCreatesAndPushesDeadline(t *testing.T) {
	wd := newStatusWatchdog(nil, nil)
	wd.Extend("ns", "sess", 60*time.Second)

	wd.mu.Lock()
	defer wd.mu.Unlock()
	st, ok := wd.state["ns/sess"]
	require.True(t, ok, "Extend must create tracking state")
	// Warn fires at LastActivity+WarnAfter; with the 2× fudge that should be
	// ~120s out, i.e. LastActivity ~= now + (120s - 60s) = now + 60s.
	assert.WithinDuration(t, time.Now().Add(60*time.Second), st.LastActivity, 3*time.Second)
}

func TestWatchdogExtendIgnoresNonPositive(t *testing.T) {
	wd := newStatusWatchdog(nil, nil)
	wd.Extend("ns", "sess", 0)
	wd.Extend("ns", "sess", -10*time.Second)
	wd.mu.Lock()
	defer wd.mu.Unlock()
	_, ok := wd.state["ns/sess"]
	assert.False(t, ok, "non-positive Extend must not create state")
}

// guard against an accidental emoji creeping back into the warn text.
func TestWatchdogWarnText_NoEmoji(t *testing.T) {
	assert.NotContains(t, watchdogWarnText, "⚠️")
	assert.Contains(t, watchdogWarnText, "Taking longer than expected")
}

// TestWatchdogTimeoutNotice_RepeatReadsAsARecheck pins that a second and later
// stall notice for the same session does not read as a fresh discovery. The
// user replied again and we re-checked; repeating "seems to have stalled"
// verbatim reads as a broken bot rather than as something that keeps looking.
func TestWatchdogTimeoutNotice_RepeatReadsAsARecheck(t *testing.T) {
	first := watchdogTimeoutNotice("sess", 1).Args()
	assert.Contains(t, first.Lead, "seems to have stalled", "the first notice reports the discovery")
	assert.NotContains(t, first.Lead, "Still no progress", "nothing to re-check on the first notice")

	third := watchdogTimeoutNotice("sess", 3).Args()
	assert.Contains(t, third.Lead, "Still no progress", "a repeat must say we looked again")
	assert.Contains(t, third.Lead, "3rd", "and how many times it has now stalled")

	// The session id survives on both, so whoever CAN investigate has what they
	// need. It is the id and nothing more: this lands in a chat thread in front
	// of a reader who probably has no cluster access, so a shell command would
	// be noise they cannot act on.
	for _, a := range []notice.Args{first, third} {
		require.Len(t, a.Fields, 1)
		assert.Equal(t, "sess", a.Fields[0].Value)
		assert.NotEmpty(t, a.NextStep, "a degraded notice must say what to do")
	}
}

// A stall is a guess about a session that may still be running, so it must not
// be dressed as a death: degraded, and explicitly not terminal.
func TestWatchdogTimeoutNotice_IsNotTerminal(t *testing.T) {
	withRealCategories(t)
	cat, ok := channelinteractions.Get(watchdogTimeoutNotice("sess", 1).Category())
	require.True(t, ok, "the stall category must be registered")
	assert.Equal(t, channelinteractions.ToneDegraded, cat.Tone)
	assert.False(t, cat.Terminal,
		"the watchdog reports an absence of progress, not a confirmed failure")
}

// TestWatchdogStallCount_ResetsWhenTheRunnerShowsActivity pins the recovery
// edge: consecutive stall notices are only "consecutive" while the runner stays
// silent. A live turn means it came back, so the next stall is a fresh
// discovery again, not the Nth of an old streak.
func TestWatchdogStallCount_ResetsWhenTheRunnerShowsActivity(t *testing.T) {
	wd := newStatusWatchdog(nil, nil)
	assert.Equal(t, 1, wd.bumpStall("ns/sess"))
	assert.Equal(t, 2, wd.bumpStall("ns/sess"))

	wd.OnTurnActivity(context.Background(), "ns", "sess", true, "", 1, "uid-1")

	assert.Equal(t, 1, wd.bumpStall("ns/sess"),
		"a live turn is forward progress; the consecutive-stall streak restarts")
}

// TestOnTurnActivity_Active arms the watchdog (creates fresh tracking) and
// re-baselines the ordering on the new episode's Seq.
func TestOnTurnActivity_Active(t *testing.T) {
	wd := newStatusWatchdog(nil, nil)
	wd.OnTurnActivity(context.Background(), "ns", "sess", true, "", 7, "uid-1")

	wd.mu.Lock()
	defer wd.mu.Unlock()
	st, ok := wd.state["ns/sess"]
	require.True(t, ok, "active turn_activity must arm tracking")
	assert.WithinDuration(t, time.Now(), st.LastActivity, 2*time.Second)
	assert.False(t, st.Yielded, "active turn_activity must not be yielded")
	assert.Equal(t, uint64(7), st.LastSeq, "active turn_activity re-baselines LastSeq")
}

// TestOnTurnActivity_PausedYieldsAndGates pins the unified semantics: a
// non-leakage pause yields — it dismisses the visible indicator but KEEPS
// tracking with Yielded set, so a later out-of-order caption is gated (the old
// behaviour Forgot tracking, which would have let a stale caption re-arm a
// fresh state). With a nil sender the indicator clear is a no-op; the State
// must remain, marked Yielded.
func TestOnTurnActivity_PausedYieldsAndGates(t *testing.T) {
	for _, cause := range []string{
		channelevents.PauseCauseReply,
		channelevents.PauseCauseApproval,
		channelevents.PauseCauseRetry,
		channelevents.PauseCauseFailed,
		"", // unknown ⇒ safe default = yield
	} {
		t.Run(cause, func(t *testing.T) {
			wd := newStatusWatchdog(nil, nil)
			wd.Touch("ns", "sess")
			wd.OnTurnActivity(context.Background(), "ns", "sess", false, cause, 100, "uid-1")

			wd.mu.Lock()
			defer wd.mu.Unlock()
			st, ok := wd.state["ns/sess"]
			require.True(t, ok, "paused turn_activity must KEEP tracking so later captions are gated (cause=%q)", cause)
			assert.True(t, st.Yielded, "paused turn_activity must mark Yielded (cause=%q)", cause)
		})
	}
}

// TestOnTurnActivity_LeakageDoesNotClear keeps the leakage notice alive: the
// watchdog's WaitLeakage path must still be able to announce the invisible
// approval, so tracking is retained and NOT yielded (the carve-out skips Apply).
func TestOnTurnActivity_LeakageDoesNotClear(t *testing.T) {
	wd := newStatusWatchdog(nil, nil)
	wd.Touch("ns", "sess")
	wd.OnTurnActivity(context.Background(), "ns", "sess", false, channelevents.PauseCauseLeakageApproval, 100, "uid-1")

	wd.mu.Lock()
	defer wd.mu.Unlock()
	st, ok := wd.state["ns/sess"]
	require.True(t, ok, "leakage pause must NOT clear — the notice stays")
	assert.False(t, st.Yielded, "leakage pause must NOT yield (would suppress the announce)")
}

// The stall notice is posted into a chat thread, read by whoever was talking
// to the agent. It must (a) carry a severity affordance, which the tone chip
// supplies from the category rather than a hand-placed glyph, and (b) not
// instruct a chat user to run kubectl — most people who see this have no
// cluster access, and a shell command in a Slack thread is noise they cannot
// act on. The session id alone is the useful handle: whoever CAN debug it can
// look it up.
func TestWatchdogTimeoutNotice_UserFacingShape(t *testing.T) {
	for _, consecutive := range []int{1, 2, 5} {
		withRealCategories(t)
		n := watchdogTimeoutNotice("hubspot-cron-input-a6318767", consecutive)
		a := n.Args()
		cat, ok := channelinteractions.Get(n.Category())
		require.True(t, ok)

		// The affordance is the tone, painted by whichever surface renders it,
		// rather than a glyph the publisher glued to the front of a string.
		assert.Equal(t, channelinteractions.ToneDegraded, cat.Tone,
			"the notice must carry a severity affordance (consecutive=%d)", consecutive)

		all := a.Lead + " " + a.Body + " " + a.NextStep
		for _, f := range a.Fields {
			all += " " + f.Label + " " + f.Value
		}
		assert.NotContains(t, all, "kubectl",
			"must not tell a chat user to run a shell command (consecutive=%d): %q", consecutive, all)
		assert.NotContains(t, all, "describe agentsession",
			"must not leak the operator runbook into a user-facing message")
		assert.Contains(t, all, "hubspot-cron-input-a6318767",
			"must still name the session so someone with access can look it up")
	}
}

// The repeat wording must survive the rewrite — an unchanged message on every
// reply reads as a stuck bot rather than an agent that keeps checking.
func TestWatchdogTimeoutNotice_RepeatStillDiffers(t *testing.T) {
	first := watchdogTimeoutNotice("s", 1).Args().Lead
	second := watchdogTimeoutNotice("s", 2).Args().Lead
	assert.NotEqual(t, first, second, "a repeat stall must re-word the lead")
	assert.Contains(t, second, "Still no progress", "the repeat says we looked again")
}

// withRealCategories installs the production category registry for a test that
// reads it, and restores it afterwards.
//
// No test in this package Resets the registry today, so this is insurance
// rather than a fix. It is worth the four lines because the failure it
// prevents is intermittent: a test that reads a Reset registry passes alone
// and fails only under some orderings, which is far more expensive to diagnose
// than it is to prevent.
func withRealCategories(t *testing.T) {
	t.Helper()
	channelinteractions.Reset()
	categories.RegisterAll()
	t.Cleanup(func() {
		channelinteractions.Reset()
		categories.RegisterAll()
	})
}
