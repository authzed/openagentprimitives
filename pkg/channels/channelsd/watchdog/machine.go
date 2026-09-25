// Package watchdog is the pure decision core of channelsd's per-session "is the
// agent silently stuck?" watchdog. It holds no I/O and no Kubernetes types:
// internal/cmd/channelsd owns the per-session map, the tick loop, and the Slack/k8s
// calls, and feeds this machine two kinds of signal —
//
//   - Activity / ExpectedDuration: the agent emitted progress (setStatus,
//     tool_activity, plan_update) or declared how long it expects to be busy.
//   - Wait classification: the session's current phase, derived from its status
//     by the session_watcher (the single source of truth).
//
// Decide() is a pure function of (state, now, config), so every behavioural rule
// lives in one table-testable place rather than smeared across fire-time
// re-checks.
package watchdog

import "time"

// Config holds the tunable thresholds. Zero value is not valid; use Default.
type Config struct {
	// WarnAfter is how long a session may be silent (while actively working)
	// before the watchdog posts a "taking longer than expected" notice.
	WarnAfter time.Duration
	// TimeoutAfter is how long before it declares the session stalled.
	TimeoutAfter time.Duration
	// MaxExtension caps how far an agent's expected-duration hint may push the
	// deadline. The agent is trusted to size the hint to the work (a large
	// artifact generation can legitimately run for many minutes), so this is a
	// high ceiling — not a tight bound — kept only so a runaway or hallucinated
	// hint can't disable the watchdog entirely.
	MaxExtension time.Duration
	// ExpectedDurationFudge multiplies the agent's expected-duration hint
	// before it becomes a deadline — agents under-estimate, so a 60s hint buys
	// 120s of silence at fudge=2.
	ExpectedDurationFudge int
}

// Default returns the production thresholds: warn at 60s, stall at 120s, hints
// doubled (the fudge) and honoured up to a 30m runaway ceiling — high enough
// that a legitimately long artifact generation is never cut short.
func Default() Config {
	return Config{
		WarnAfter:             60 * time.Second,
		TimeoutAfter:          120 * time.Second,
		MaxExtension:          30 * time.Minute,
		ExpectedDurationFudge: 2,
	}
}

// Wait classifies why a session is not actively producing output. It is the
// single lever that decides whether silence is alarming.
type Wait int

const (
	// WaitActive: the agent is working (Running/Pending). Silence counts toward
	// the warn/timeout deadlines.
	WaitActive Wait = iota
	// WaitVisible: the session is waiting, but the channel already shows why —
	// an idle session (the agent posted a reply), a tool-call approval (public
	// thread message), a permission request (in-thread), an awaiting-retry
	// button, or a terminal phase. The watchdog stays silent.
	WaitVisible
	// WaitLeakage: an info-leakage approval is pending. Its prompt is delivered
	// only as a private ephemeral to the approver(s), so the requester and the
	// rest of the thread see nothing — the watchdog announces the wait so they
	// know why the agent went quiet. Never times out (a human approver is
	// legitimately deliberating).
	WaitLeakage
)

// State is one session's watchdog state: when it last made progress, whether
// the warn/timeout alarms have fired, and the ordered-status fields (Caption,
// Yielded, UID, LastSeq) that Apply folds events into, so caption routing, yield
// suppression, stale-drop, and silence timing share one view of the session.
//
// It deliberately does NOT store the Wait classification: that is computed fresh
// from the session's status at decision time, so the watchdog never acts on a
// stale phase.
type State struct {
	LastActivity time.Time
	Warned       bool
	TimedOut     bool

	// Status (caption/yield/ordering) state, unified with timing above so a
	// yield seen by Apply disarms the same State that Decide reads.
	Caption string
	Yielded bool
	UID     string
	LastSeq uint64

	// OpDetail is the current operation compact line ("" = no active operation).
	OpDetail string
}

// EventKind names the ordered status events the shell feeds to Apply.
type EventKind int

const (
	// EvTurnActivity is a turn lifecycle event: Active=true means the runner
	// started a new episode (re-baselines); Active=false means it yielded
	// (awaiting_reply or similar — the channel now shows the wait).
	EvTurnActivity EventKind = iota
	// EvCaption is a setStatus-bearing notification or plan_update. It carries
	// the caption text (or empty for clear) and a monotone Seq.
	EvCaption
	// EvToolActivity is a tool-call progress signal that re-arms timing but
	// does not route any envelope to the sender.
	EvToolActivity
	// EvTurnProgress is a render-only turn-progress tick (the runner's
	// cumulative token / elapsed-time spinner update). It forwards to the
	// sender for rendering, except while the session is yielded — a paused
	// turn must stop animating progress until it resumes.
	EvTurnProgress
	// EvToolProgress is a render-only per-tool liveness/progress tick published
	// by the sandbox tool while a SYNC tool executes. Forwarded to the sender
	// (the channel renders it), except while the session is yielded — a paused
	// turn must stop animating. Like EvTurnProgress it carries no caption
	// ordering, so it never touches LastSeq.
	EvToolProgress
	// EvOperationActivity is a render-only snapshot of the active operation
	// subtree. Like EvToolProgress it is yield-suppressed and re-arms the
	// silence timer, but it also carries the resolved effective compact line
	// (its own throttled stream — it never touches caption LastSeq).
	EvOperationActivity
)

// Event is one ordered status event delivered to Apply.
type Event struct {
	Kind      EventKind
	Seq       uint64
	UID       string
	Active    bool   // EvTurnActivity: true=active (new episode), false=paused (yield)
	Cause     string // EvTurnActivity pause cause (e.g. "awaiting_reply")
	IsCaption bool   // EvCaption: this envelope carries a setStatus caption
	// Text carries the caption text for EvCaption (remembered for revert) and
	// the operation compact line for EvOperationActivity ("" on a cleared tick).
	Text string
}

// ApplyResult tells the shell what to do with the envelope that produced this event.
type ApplyResult struct {
	Forward       bool   // route this envelope to the sender?
	ClearCaption  bool   // tell the sender to clear the status indicator
	EffectiveLine string // resolved compact line the sender should render
}

// Apply folds one ordered status event into the state and returns what the
// shell should do. Pure except for mutating the receiver. Identity-scoped: a
// new UID (fork/recreate) resets the ordering baseline.
func (s *State) Apply(ev Event, now time.Time) ApplyResult {
	if ev.UID != "" && ev.UID != s.UID {
		s.UID = ev.UID
		s.LastSeq = 0
		s.Yielded = false
		s.Caption = ""
		s.OpDetail = ""
	}
	switch ev.Kind {
	case EvTurnActivity:
		if ev.Active {
			s.Yielded = false
			s.LastSeq = ev.Seq // re-baseline for the new episode
			s.Activity(now)    // re-arm timing
			// Forward=true for API symmetry only — the relay never routes
			// turn_activity to a sender, so this field is inert for this event kind.
			return ApplyResult{Forward: true}
		}
		// paused: yield (leakage is handled by the shell's WaitLeakage path,
		// which does not route a paused EvTurnActivity here).
		s.Yielded = true
		s.Caption = ""
		s.OpDetail = ""
		return ApplyResult{Forward: false, ClearCaption: true}
	case EvCaption:
		// Seq==0 is the "unordered" envelope class (see envelope.go): forwarded
		// subject only to yield suppression, never part of the stale-Seq ordering.
		// Producers like the "Continuing…" indicator-resume publish Seq=0 via plain
		// PublishOut and must not be gated by ordered caption state.
		if s.Yielded {
			return ApplyResult{Forward: false} // yield suppresses ALL captions, incl. Seq==0
		}
		if ev.Seq != 0 && ev.Seq < s.LastSeq {
			return ApplyResult{Forward: false} // stale — only applies to ordered Seq>0 captions
		}
		if ev.Seq != 0 {
			s.LastSeq = ev.Seq // unordered Seq==0 must not advance LastSeq
		}
		// A plan_update folds through here with ev.Text=="", which must not wipe a
		// remembered update_status caption out from under the operation-activity
		// revert path. Intentional clears (yield, UID reset) set s.Caption = ""
		// directly on their own paths.
		if ev.Text != "" {
			s.Caption = ev.Text
		}
		s.Activity(now) // a caption is forward progress
		return ApplyResult{Forward: true, EffectiveLine: s.EffectiveLine()}
	case EvToolActivity:
		s.Activity(now)
		return ApplyResult{Forward: false}
	case EvTurnProgress:
		// Render-only spinner tick, dropped while yielded so a paused turn stops
		// animating; a later EvTurnActivity{Active:true} resumes it. Carries its
		// own per-turn counter, so it never touches the caption ordering (LastSeq).
		if s.Yielded {
			return ApplyResult{Forward: false}
		}
		return ApplyResult{Forward: true}
	case EvToolProgress:
		// Render-only tool tick. Suppressed while yielded; otherwise re-arms the
		// silence timer — a running tool IS progress, which is what keeps a
		// multi-minute git clone from tripping the alarm. No LastSeq interaction.
		if s.Yielded {
			return ApplyResult{Forward: false}
		}
		s.Activity(now)
		return ApplyResult{Forward: true}
	case EvOperationActivity:
		// Render-only operation snapshot. Suppressed while yielded; otherwise
		// re-arms the silence timer — a running operation IS progress, which also
		// covers long MCP tools that emit no tool_progress. Its own throttled
		// stream: no LastSeq interaction.
		if s.Yielded {
			return ApplyResult{Forward: false}
		}
		s.OpDetail = ev.Text // "" on a cleared tick → EffectiveLine reverts to Caption
		s.Activity(now)
		return ApplyResult{Forward: true, EffectiveLine: s.EffectiveLine()}
	}
	return ApplyResult{Forward: false}
}

// EffectiveLine is the compact status line: live operation detail when present,
// else the last remembered caption. Enables clean revert when operations clear.
func (s State) EffectiveLine() string {
	if s.OpDetail != "" {
		return s.OpDetail
	}
	return s.Caption
}

// Activity records forward progress: it advances the deadline reference to now
// (forward-only — a late progress signal never shrinks an extended window) and
// re-arms the warn/timeout alarms.
func (s *State) Activity(now time.Time) {
	if now.After(s.LastActivity) {
		s.LastActivity = now
	}
	s.Warned = false
	s.TimedOut = false
}

// ExpectedDuration applies an agent's expected-duration hint: it pushes the
// deadline out to now + fudge*dur (forward-only, capped at MaxExtension) so the
// default window doesn't fire spuriously inside a known-slow operation. A
// non-positive dur is a no-op.
func (s *State) ExpectedDuration(now time.Time, dur time.Duration, cfg Config) {
	if dur <= 0 {
		return
	}
	dur *= time.Duration(cfg.ExpectedDurationFudge)
	if dur > cfg.MaxExtension {
		dur = cfg.MaxExtension
	}
	// LastActivity is the reference from which WarnAfter elapses; setting it to
	// (now + dur - WarnAfter) makes the warn fire at exactly now + dur.
	target := now.Add(dur - cfg.WarnAfter)
	if target.After(s.LastActivity) {
		s.LastActivity = target
	}
	s.Warned = false
	s.TimedOut = false
}

// Action is what a tick should do for one session.
type Action int

const (
	// ActionNone: do nothing this tick.
	ActionNone Action = iota
	// ActionWarn: post the generic "taking longer than expected" notice.
	ActionWarn
	// ActionWarnLeakage: post the info-leakage "waiting for approval to share
	// data" notice (the wait is invisible in-channel).
	ActionWarnLeakage
	// ActionTimeout: post the "appears to have stalled" message.
	ActionTimeout
)

// Decide returns the action the tick should take for this state, given the
// session's wait classification, at time now. Pure: it never mutates State — the
// caller marks Warned/TimedOut after acting. wait is passed in rather than
// stored so the decision always reflects fresh session status.
func (s State) Decide(wait Wait, now time.Time, cfg Config) Action {
	if s.Yielded {
		return ActionNone
	}
	switch wait {
	case WaitVisible:
		// The channel already shows why the agent is quiet. Stay silent.
		return ActionNone
	case WaitLeakage:
		// Invisible approval wait: announce once, never time out.
		if !s.Warned && now.Sub(s.LastActivity) >= cfg.WarnAfter {
			return ActionWarnLeakage
		}
		return ActionNone
	default: // WaitActive
		elapsed := now.Sub(s.LastActivity)
		if !s.TimedOut && elapsed >= cfg.TimeoutAfter {
			return ActionTimeout
		}
		if !s.Warned && elapsed >= cfg.WarnAfter {
			return ActionWarn
		}
		return ActionNone
	}
}
