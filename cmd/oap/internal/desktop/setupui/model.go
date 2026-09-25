// Package setupui is the backend for the macOS "oap desktop" setup UI: a
// live timeline of VM bring-up (see desktop.Engine.Up / EngineHooks.Progress)
// plus a first-run config form. It is pure Go, cgo-free, and cross-platform
// so it can be unit-tested on any OS; the page itself (static/index.html) is
// a self-contained HTML/CSS/JS document served by server.go. Driving the
// Timeline from Engine.Up and hosting this server in a native webview
// window are later tasks.
//
// This file holds the data model: Timeline, a concurrency-safe ordered list
// of bring-up steps. Timeline never calls time.Now itself — every mutation
// that needs a timestamp takes one as an argument — so transitions are
// deterministic and unit-testable without a clock seam. Production wiring
// (a later task) drives it from desktop.EngineHooks.Progress and from
// Engine.Up's success/error return using time.Now().UnixMilli().
package setupui

import "sync"

// Status is one Step's lifecycle state.
type Status string

const (
	StatusPending Status = "pending"
	StatusActive  Status = "active"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// Phase is the Timeline's overall state, surfaced to the setup UI so it
// knows whether to show the first-run config form, the live timeline, or a
// terminal ready/failed screen.
type Phase string

const (
	// PhaseConfiguring is the initial phase: no bring-up step has Begun yet.
	PhaseConfiguring Phase = "configuring"
	// PhaseRunning is set as soon as the first step Begins.
	PhaseRunning Phase = "running"
	// PhaseReady is set once Complete is called (bring-up succeeded).
	PhaseReady Phase = "ready"
	// PhaseFailed is set once Fail is called.
	PhaseFailed Phase = "failed"
)

// Step is one bring-up step's state. StartedAt/EndedAt are unix
// milliseconds; 0 means unset.
type Step struct {
	Name      string `json:"name"`
	Status    Status `json:"status"`
	StartedAt int64  `json:"startedAt,omitempty"`
	EndedAt   int64  `json:"endedAt,omitempty"`
	// Detail is an optional sub-status line for a long-running step (e.g.
	// "waiting for PostgreSQL" during "Installing components").
	Detail string `json:"detail,omitempty"`
	// Error is set only when Status == StatusFailed.
	Error string `json:"error,omitempty"`
}

// TimelineState is a JSON-serializable snapshot of a Timeline: a copy, not
// a live view, so callers (the HTTP layer's /progress and /events routes)
// may hold and serialize it without any lock.
type TimelineState struct {
	// NeedsConfig tells the setup page whether to show the first-run
	// config form before the bring-up timeline. Defaults to false — a
	// Timeline constructed by NewTimeline assumes config already exists
	// (e.g. re-launch) until SetNeedsConfig says otherwise.
	NeedsConfig bool   `json:"needsConfig"`
	Phase       Phase  `json:"phase"`
	Steps       []Step `json:"steps"`
}

// Timeline is a concurrency-safe, ordered list of named bring-up steps.
// The zero value is not usable — construct with NewTimeline.
type Timeline struct {
	mu          sync.Mutex
	steps       []Step
	index       map[string]int
	phase       Phase
	needsConfig bool
	subs        map[chan TimelineState]struct{}
}

// NewTimeline seeds an ordered list of step names, all initially pending.
// The caller decides which steps apply — e.g. omitting the tunnel step
// when no external channel is configured — mirroring the ordered list
// Engine.Up's Progress hook fires (see orchestrate.go): "Provisioning disk
// image", "Booting virtual machine", "Waiting for guest network", "Fetching
// cluster credentials", "Installing components", "Configuring cluster",
// and, only when a channel is configured, "Setting up tunnel".
func NewTimeline(steps []string) *Timeline {
	t := &Timeline{
		steps: make([]Step, len(steps)),
		index: make(map[string]int, len(steps)),
		phase: PhaseConfiguring,
		subs:  make(map[chan TimelineState]struct{}),
	}
	for i, name := range steps {
		t.steps[i] = Step{Name: name, Status: StatusPending}
		t.index[name] = i
	}
	return t
}

// Begin marks step active and marks every not-yet-finished step before it
// (pending OR still active — Begin is called exactly once per step as it
// starts, so the immediately preceding step is normally sitting in
// StatusActive, not StatusPending, when this fires) as done. Engine.Up's
// steps fire strictly in order, so a later step starting implies every
// earlier one finished (see the package doc on orchestrate.go's
// EngineHooks.Progress: "every earlier step [is] complete once a later
// one starts"). Idempotent: calling Begin again for the step that is
// already active is a no-op, so a duplicate Progress call never resets
// StartedAt. Unknown step names are silently ignored — production callers
// only ever pass names from the fixed list this Timeline was constructed
// with, so this is a defensive no-op rather than a reachable error path.
func (t *Timeline) Begin(step string, nowMillis int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	idx, ok := t.index[step]
	if !ok || t.steps[idx].Status == StatusActive {
		return
	}
	for i := 0; i < idx; i++ {
		if t.steps[i].Status == StatusPending || t.steps[i].Status == StatusActive {
			if t.steps[i].StartedAt == 0 {
				t.steps[i].StartedAt = nowMillis
			}
			t.steps[i].EndedAt = nowMillis
			t.steps[i].Status = StatusDone
		}
	}
	t.steps[idx].Status = StatusActive
	t.steps[idx].StartedAt = nowMillis
	t.steps[idx].EndedAt = 0
	if t.phase == PhaseConfiguring {
		t.phase = PhaseRunning
	}
	t.broadcastLocked()
}

// Detail sets step's sub-status line. No-op for unknown step names.
func (t *Timeline) Detail(step, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	idx, ok := t.index[step]
	if !ok {
		return
	}
	t.steps[idx].Detail = detail
	t.broadcastLocked()
}

// Complete marks every not-already-failed step done and moves the overall
// phase to PhaseReady. Called once when Engine.Up returns nil.
func (t *Timeline) Complete(nowMillis int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i := range t.steps {
		if t.steps[i].Status == StatusFailed {
			continue
		}
		if t.steps[i].StartedAt == 0 {
			t.steps[i].StartedAt = nowMillis
		}
		t.steps[i].EndedAt = nowMillis
		t.steps[i].Status = StatusDone
	}
	t.phase = PhaseReady
	t.broadcastLocked()
}

// Fail marks step failed with err recorded, and moves the overall phase to
// PhaseFailed. step names not in the fixed list still move the overall
// phase to PhaseFailed but touch no Step — this covers a failure that
// happens before any step Begins (e.g. desktop.Config.Validate()
// rejecting the config before Engine.Up's first Progress call), which has
// no step name to attach the error to.
func (t *Timeline) Fail(step string, nowMillis int64, err string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if idx, ok := t.index[step]; ok {
		if t.steps[idx].StartedAt == 0 {
			t.steps[idx].StartedAt = nowMillis
		}
		t.steps[idx].EndedAt = nowMillis
		t.steps[idx].Status = StatusFailed
		t.steps[idx].Error = err
	}
	t.phase = PhaseFailed
	t.broadcastLocked()
}

// SetNeedsConfig sets whether the setup page should show the first-run
// config form before the bring-up timeline. Production wiring (a later
// task) calls this with true when no config.json exists yet on disk;
// the zero value (false) means bring-up can proceed straight to the
// timeline view. Broadcasts to subscribers like every other mutation, so
// a page already connected via /events flips screens live rather than
// needing a reload.
func (t *Timeline) SetNeedsConfig(needs bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.needsConfig = needs
	t.broadcastLocked()
}

// Snapshot returns a JSON-serializable copy of the current state.
func (t *Timeline) Snapshot() TimelineState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

// Subscribe registers a subscriber that receives a full TimelineState
// snapshot on every mutation. The channel is buffered and lossy-on-overflow
// (a slow consumer drops a frame rather than blocking the mutating
// goroutine; the next mutation carries full state, not a delta, so a
// dropped frame never desyncs the subscriber). The returned cancel MUST be
// called when the subscriber disconnects, or its channel — and slot in the
// Timeline's subscriber set — leaks.
func (t *Timeline) Subscribe() (<-chan TimelineState, func()) {
	ch := make(chan TimelineState, 8)
	t.mu.Lock()
	t.subs[ch] = struct{}{}
	t.mu.Unlock()
	return ch, func() {
		t.mu.Lock()
		delete(t.subs, ch)
		t.mu.Unlock()
	}
}

// snapshotLocked must be called with t.mu held.
func (t *Timeline) snapshotLocked() TimelineState {
	steps := make([]Step, len(t.steps))
	copy(steps, t.steps)
	return TimelineState{NeedsConfig: t.needsConfig, Phase: t.phase, Steps: steps}
}

// broadcastLocked must be called with t.mu held; it pushes the current
// state to every subscriber.
func (t *Timeline) broadcastLocked() {
	snap := t.snapshotLocked()
	for ch := range t.subs {
		select {
		case ch <- snap:
		default: // lossy: see Subscribe's doc.
		}
	}
}
