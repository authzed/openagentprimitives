package watchdog_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/watchdog"
)

var t0 = time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

func cfg() watchdog.Config { return watchdog.Default() }

// ---- Decide: the whole behavioural table ----

func TestDecide(t *testing.T) {
	c := cfg()
	cases := []struct {
		name    string
		kind    watchdog.Wait
		warned  bool
		timeout bool
		elapsed time.Duration
		want    watchdog.Action
	}{
		// Active: silence escalates warn → timeout.
		{"active, fresh", watchdog.WaitActive, false, false, 0, watchdog.ActionNone},
		{"active, just under warn", watchdog.WaitActive, false, false, 59 * time.Second, watchdog.ActionNone},
		{"active, at warn", watchdog.WaitActive, false, false, 60 * time.Second, watchdog.ActionWarn},
		{"active, already warned", watchdog.WaitActive, true, false, 90 * time.Second, watchdog.ActionNone},
		{"active, at timeout", watchdog.WaitActive, true, false, 120 * time.Second, watchdog.ActionTimeout},
		{"active, already timed out", watchdog.WaitActive, true, true, 300 * time.Second, watchdog.ActionNone},

		// Visible: the channel shows why; never fire.
		{"visible, well past timeout", watchdog.WaitVisible, false, false, 600 * time.Second, watchdog.ActionNone},

		// Leakage: announce once at warn, never time out.
		{"leakage, under warn", watchdog.WaitLeakage, false, false, 59 * time.Second, watchdog.ActionNone},
		{"leakage, at warn", watchdog.WaitLeakage, false, false, 60 * time.Second, watchdog.ActionWarnLeakage},
		{"leakage, already warned", watchdog.WaitLeakage, true, false, 90 * time.Second, watchdog.ActionNone},
		{"leakage, way past timeout window still never times out", watchdog.WaitLeakage, false, false, 600 * time.Second, watchdog.ActionWarnLeakage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := watchdog.State{
				LastActivity: t0,
				Warned:       tc.warned,
				TimedOut:     tc.timeout,
			}
			assert.Equal(t, tc.want, s.Decide(tc.kind, t0.Add(tc.elapsed), c))
		})
	}
}

// ---- Activity ----

func TestActivity_AdvancesAndReArms(t *testing.T) {
	s := watchdog.State{LastActivity: t0, Warned: true, TimedOut: true}
	s.Activity(t0.Add(30 * time.Second))
	assert.Equal(t, t0.Add(30*time.Second), s.LastActivity)
	assert.False(t, s.Warned)
	assert.False(t, s.TimedOut)
}

func TestActivity_ForwardOnly(t *testing.T) {
	future := t0.Add(2 * time.Minute)
	s := watchdog.State{LastActivity: future}
	s.Activity(t0) // an older progress signal must not shrink an extended window
	assert.Equal(t, future, s.LastActivity, "Activity must never move LastActivity backward")
}

// ---- ExpectedDuration ----

func TestExpectedDuration_AppliesFudgeAndScheduleWarnAtNowPlusFudgeDur(t *testing.T) {
	c := cfg()
	s := watchdog.State{LastActivity: t0}
	s.ExpectedDuration(t0, 60*time.Second, c) // fudge 2 → warn should fire at +120s

	assert.Equal(t, watchdog.ActionNone, s.Decide(watchdog.WaitActive, t0.Add(119*time.Second), c), "no warn before the fudged deadline")
	assert.Equal(t, watchdog.ActionWarn, s.Decide(watchdog.WaitActive, t0.Add(120*time.Second), c), "warn at now + fudge*dur")
}

func TestExpectedDuration_CapsAtMaxExtension(t *testing.T) {
	c := cfg()
	s := watchdog.State{LastActivity: t0}
	s.ExpectedDuration(t0, 20*time.Minute, c) // 2x = 40m, capped to 30m
	// Warn must fire at the cap (30m), not 40m.
	assert.Equal(t, watchdog.ActionNone, s.Decide(watchdog.WaitActive, t0.Add(30*time.Minute-time.Second), c))
	assert.Equal(t, watchdog.ActionWarn, s.Decide(watchdog.WaitActive, t0.Add(30*time.Minute), c))
}

func TestExpectedDuration_ForwardOnly(t *testing.T) {
	c := cfg()
	s := watchdog.State{LastActivity: t0}
	s.ExpectedDuration(t0, 120*time.Second, c) // pushes deadline to +240s (warn ref far out)
	before := s.LastActivity
	s.ExpectedDuration(t0, 10*time.Second, c) // smaller hint must not pull it back
	assert.Equal(t, before, s.LastActivity)
}

func TestExpectedDuration_NonPositiveIsNoOp(t *testing.T) {
	c := cfg()
	s := watchdog.State{LastActivity: t0}
	s.ExpectedDuration(t0, 0, c)
	s.ExpectedDuration(t0, -5*time.Second, c)
	assert.Equal(t, t0, s.LastActivity, "non-positive duration must not change state")
}

// ---- Apply: unified status machine ----

func TestStatusMachineApply(t *testing.T) {
	now := time.Unix(1000, 0)
	cap := func(seq uint64, uid string) watchdog.Event {
		return watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: seq, UID: uid}
	}
	cap0 := func(uid string) watchdog.Event {
		// Seq==0 is the "unordered" class (see envelope.go).
		return watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 0, UID: uid}
	}
	yield := func(seq uint64, uid string) watchdog.Event {
		return watchdog.Event{Kind: watchdog.EvTurnActivity, Seq: seq, UID: uid, Active: false, Cause: "awaiting_reply"}
	}
	active := func(seq uint64, uid string) watchdog.Event {
		return watchdog.Event{Kind: watchdog.EvTurnActivity, Seq: seq, UID: uid, Active: true}
	}

	cases := []struct {
		name             string
		events           []watchdog.Event
		wantFwd          []bool // Forward per event
		wantClearCaption []bool // ClearCaption per event
		wantYield        bool
	}{
		{"caption forwarded then yield clears",
			[]watchdog.Event{cap(10, "u"), yield(20, "u")},
			[]bool{true, false}, []bool{false, true}, true},
		{"stale caption after yield is dropped, stays cleared",
			[]watchdog.Event{yield(20, "u"), cap(10, "u")},
			[]bool{false, false}, []bool{true, false}, true},
		{"caption arriving before yield (lower seq) still forwards, then cleared",
			[]watchdog.Event{cap(10, "u"), yield(20, "u"), cap(5, "u")},
			[]bool{true, false, false}, []bool{false, true, false}, true},
		{"active re-baselines: a fresh low-seq caption forwards after resume",
			[]watchdog.Event{yield(20, "u"), active(0, "u"), cap(1, "u")},
			[]bool{false, true, true}, []bool{true, false, false}, false},
		{"new uid (fork/recreate) resets baseline and accepts",
			[]watchdog.Event{cap(99, "old"), cap(1, "new")},
			[]bool{true, true}, []bool{false, false}, false},
		{"duplicate/stale caption same-uid lower seq dropped",
			[]watchdog.Event{cap(10, "u"), cap(9, "u")},
			[]bool{true, false}, []bool{false, false}, false},
		// Seq==0 escapes stale-drop (forwarded even while LastSeq>0) but yield still
		// suppresses it — and it must never advance LastSeq.
		{"Seq==0 caption escapes stale-drop but not yield",
			[]watchdog.Event{cap(10, "u"), cap0("u"), yield(20, "u"), cap0("u")},
			[]bool{true, true, false, false}, []bool{false, false, true, false}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s watchdog.State
			for i, ev := range tc.events {
				got := s.Apply(ev, now)
				assert.Equal(t, tc.wantFwd[i], got.Forward, "event %d Forward", i)
				assert.Equal(t, tc.wantClearCaption[i], got.ClearCaption, "event %d ClearCaption", i)
			}
			assert.Equal(t, tc.wantYield, s.Yielded, "final yielded state")
		})
	}
}

// TestStatusMachineSeqZeroDoesNotAdvanceBaseline pins that a Seq==0 (unordered)
// caption, though forwarded, never advances LastSeq — a subsequent ordered
// stale-drop must still be based on the last ordered Seq.
func TestStatusMachineSeqZeroDoesNotAdvanceBaseline(t *testing.T) {
	now := time.Unix(1000, 0)
	var s watchdog.State

	// Ordered caption sets baseline to 10.
	s.Apply(watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 10, UID: "u"}, now)
	assert.Equal(t, uint64(10), s.LastSeq, "ordered caption must set LastSeq")

	// Unordered (Seq==0) caption forwards but must not move LastSeq.
	got := s.Apply(watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 0, UID: "u"}, now)
	assert.True(t, got.Forward, "Seq==0 caption must forward when not yielded")
	assert.Equal(t, uint64(10), s.LastSeq, "Seq==0 caption must not advance LastSeq")

	// A subsequent ordered caption below 10 is still stale (baseline unchanged).
	got = s.Apply(watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 9, UID: "u"}, now)
	assert.False(t, got.Forward, "Seq=9 after Seq=10 baseline must be stale-dropped even after a Seq==0 caption")
}

// TestStatusMachineTurnProgressGatedWhileYielded pins the turn-progress gating
// contract: a render-only turn-progress tick forwards while the turn is active,
// is dropped while the session is yielded (awaiting a user reply), and resumes
// forwarding once an EvTurnActivity{Active:true} re-arms the session.
func TestStatusMachineTurnProgressGatedWhileYielded(t *testing.T) {
	now := time.Unix(1000, 0)
	progress := watchdog.Event{Kind: watchdog.EvTurnProgress, UID: "u"}
	yield := watchdog.Event{Kind: watchdog.EvTurnActivity, Seq: 2, UID: "u", Active: false, Cause: "awaiting_reply"}
	active := watchdog.Event{Kind: watchdog.EvTurnActivity, Seq: 3, UID: "u", Active: true}

	var s watchdog.State

	// Active turn (not yielded): progress forwards for rendering.
	assert.True(t, s.Apply(progress, now).Forward, "turn_progress forwards while active")

	// Yield: a subsequent progress tick must be suppressed.
	s.Apply(yield, now)
	assert.True(t, s.Yielded, "yield must mark the session Yielded")
	assert.False(t, s.Apply(progress, now).Forward, "turn_progress must be dropped while yielded")

	// Re-arm: Active=true clears Yielded → progress forwards again.
	s.Apply(active, now)
	assert.False(t, s.Yielded, "Active=true must clear Yielded")
	assert.True(t, s.Apply(progress, now).Forward, "turn_progress forwards again after re-arm")
}

func TestStatusMachineDecideSuppressedWhileYielded(t *testing.T) {
	var s watchdog.State
	s.Apply(watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 1, UID: "u"}, time.Unix(0, 0))
	s.Apply(watchdog.Event{Kind: watchdog.EvTurnActivity, Seq: 2, UID: "u", Active: false, Cause: "awaiting_reply"}, time.Unix(0, 0))
	// 200s of silence while yielded → still no warn/timeout.
	act := s.Decide(watchdog.WaitActive, time.Unix(200, 0), watchdog.Default())
	assert.Equal(t, watchdog.ActionNone, act, "a yielded session never warns even if classified active")
}

func TestApply_EvToolProgress_ForwardsWhenActive(t *testing.T) {
	var s watchdog.State
	now := time.Unix(1000, 0)
	res := s.Apply(watchdog.Event{Kind: watchdog.EvToolProgress, UID: "u1", Seq: 5}, now)
	assert.True(t, res.Forward, "tool_progress forwards while active")
	assert.False(t, res.ClearCaption)
	assert.Equal(t, now, s.LastActivity, "tool_progress re-arms the silence timer")
	assert.Equal(t, uint64(0), s.LastSeq, "tool_progress must NOT advance caption LastSeq")
}

func TestApply_EvToolProgress_DroppedWhenYielded(t *testing.T) {
	var s watchdog.State
	now := time.Unix(1000, 0)
	// Yield the session first (paused turn activity).
	s.Apply(watchdog.Event{Kind: watchdog.EvTurnActivity, Active: false, UID: "u1"}, now)
	res := s.Apply(watchdog.Event{Kind: watchdog.EvToolProgress, UID: "u1", Seq: 5}, now)
	assert.False(t, res.Forward, "tool_progress is suppressed while yielded")
}

func TestApply_OperationActivity_ForwardsAndResolvesEffectiveLine(t *testing.T) {
	now := time.Unix(1000, 0)
	var s watchdog.State
	// A prior update_status caption is remembered.
	s.Apply(watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 5, UID: "u1", Text: "Reading files…"}, now)
	// Operation detail arrives → EffectiveLine is the op line, forwarded.
	res := s.Apply(watchdog.Event{Kind: watchdog.EvOperationActivity, Seq: 6, UID: "u1", Text: "fetch goals ‣ querying"}, now)
	assert.True(t, res.Forward)
	assert.Equal(t, "fetch goals ‣ querying", res.EffectiveLine)
	assert.Equal(t, "fetch goals ‣ querying", s.EffectiveLine())
}

func TestApply_OperationActivity_ClearedRevertsToCaption(t *testing.T) {
	now := time.Unix(1000, 0)
	var s watchdog.State
	s.Apply(watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 5, UID: "u1", Text: "Reading files…"}, now)
	s.Apply(watchdog.Event{Kind: watchdog.EvOperationActivity, Seq: 6, UID: "u1", Text: "fetch goals ‣ querying"}, now)
	// Cleared tick: OpDetail empty → EffectiveLine reverts to the remembered caption.
	res := s.Apply(watchdog.Event{Kind: watchdog.EvOperationActivity, Seq: 7, UID: "u1", Text: ""}, now)
	assert.True(t, res.Forward)
	assert.Equal(t, "Reading files…", res.EffectiveLine)
	assert.Equal(t, "", s.OpDetail)
}

func TestApply_OperationActivity_SuppressedWhileYielded(t *testing.T) {
	now := time.Unix(1000, 0)
	var s watchdog.State
	s.Apply(watchdog.Event{Kind: watchdog.EvTurnActivity, Seq: 3, UID: "u1", Active: false}, now) // yield
	res := s.Apply(watchdog.Event{Kind: watchdog.EvOperationActivity, Seq: 4, UID: "u1", Text: "op ‣ r"}, now)
	assert.False(t, res.Forward, "a yielded turn must stop animating operation detail")
}

func TestApply_OperationActivity_ReArmsSilenceTimer(t *testing.T) {
	start := time.Unix(1000, 0)
	var s watchdog.State
	s.Activity(start)
	later := start.Add(90 * time.Second)
	s.Apply(watchdog.Event{Kind: watchdog.EvOperationActivity, Seq: 4, UID: "u1", Text: "op ‣ r"}, later)
	assert.Equal(t, watchdog.ActionNone, s.Decide(watchdog.WaitActive, later.Add(time.Second), watchdog.Default()),
		"a running operation is progress — must re-arm the warn deadline")
}

func TestApply_OperationActivity_DoesNotAdvanceCaptionLastSeq(t *testing.T) {
	now := time.Unix(1000, 0)
	var s watchdog.State
	s.Apply(watchdog.Event{Kind: watchdog.EvOperationActivity, Seq: 100, UID: "u1", Text: "op ‣ r"}, now)
	// A real caption at a LOWER seq must NOT be stale-dropped by op-activity's seq.
	res := s.Apply(watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 20, UID: "u1", Text: "Compiling…"}, now)
	assert.True(t, res.Forward, "operation-activity must not advance caption LastSeq")
}

func TestApply_OperationActivity_UIDResetClearsOpDetail(t *testing.T) {
	now := time.Unix(1000, 0)
	var s watchdog.State
	s.Apply(watchdog.Event{Kind: watchdog.EvOperationActivity, Seq: 6, UID: "u1", Text: "op ‣ r"}, now)
	s.Apply(watchdog.Event{Kind: watchdog.EvTurnActivity, Seq: 1, UID: "u2", Active: true}, now) // fork/recreate
	assert.Equal(t, "", s.OpDetail)
}

func TestApply_OperationActivity_PlanUpdateDoesNotWipeCaptionRevert(t *testing.T) {
	now := time.Unix(1000, 0)
	var s watchdog.State
	s.Apply(watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 5, UID: "u1", Text: "Analyzing the codebase…"}, now)
	// A plan_update folds through EvCaption with empty text (relay's captionTextOf).
	s.Apply(watchdog.Event{Kind: watchdog.EvCaption, IsCaption: true, Seq: 6, UID: "u1", Text: ""}, now)
	// An operation runs, then clears — must revert to the update_status caption, not "".
	s.Apply(watchdog.Event{Kind: watchdog.EvOperationActivity, Seq: 7, UID: "u1", Text: "op ‣ r"}, now)
	res := s.Apply(watchdog.Event{Kind: watchdog.EvOperationActivity, Seq: 8, UID: "u1", Text: ""}, now)
	assert.Equal(t, "Analyzing the codebase…", res.EffectiveLine)
}
