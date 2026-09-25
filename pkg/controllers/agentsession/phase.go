package agentsession

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// derivePhase is the steady-state phase computer: it reads the phase straight
// off the projection of the folded lifecycle log. The lifecycle state machine
// is the single source of truth for phase — there is no longer a condition- or
// queue-derived fallback here. Callers fold the session's transition log (see
// Reconciler.foldLifecycle) and pass the resulting state.
//
// Early-return paths in Reconcile do not reach this call site; they emit their
// transition event via applyEvent (which writes the projected phase) and return
// before the steady-state computation. Terminal phases are sticky inside the
// core's Transition, so a fold that observes a terminal event keeps it terminal
// regardless of later events.
func derivePhase(state lifecyclecore.State) string {
	return lifecyclecore.Project(state).Phase
}

// reconcilePhase reconciles the log-derived phase with the phase already sitting
// on the CR this reconcile, guarding against a spurious demotion to Pending when
// the folded lifecycle log is empty or incomplete.
//
// Folding a log that carries no advancing events projects to the bootstrap
// floor, Pending. That is correct for a brand-new session — but the same
// bootstrap Pending also surfaces when the log is unavailable or lagging:
// LifecycleMemory is nil (test fixtures that don't wire the audit path), or an
// operator restart / inmem read has not yet observed events another writer
// appended. In those cases a phase set directly this reconcile — the crash
// kill-switch's Failed, a grandfathered running session's Running — would be
// clobbered straight back to Pending by the steady-state write. This reconciles
// the two so an incomplete-log Pending never demotes a more-advanced or terminal
// current phase.
//
// It deliberately does NOT block legitimate demotions to Pending. A real
// WakeRequested (Idle→Pending) or CredsLinked is applied through applyEvent
// earlier in the same reconcile, which already sets the CR phase to Pending
// before this runs — so current == projected there and this is a no-op. A
// genuine bootstrap (current phase still "") likewise yields Pending. Only a
// bootstrap Pending that would overwrite an already-established, more-advanced
// phase is suppressed; any projected phase other than Pending (a real
// RunnerClaimed→Running, AgentWorkComplete→Idle, or a terminal event in the log)
// is applied as-is.
func reconcilePhase(projected, current string) string {
	// Terminal is sticky and highest precedence: once Failed/Succeeded is on the
	// CR, a lagging or incomplete log must never resurrect the session. The next
	// reconcile that observes the terminal event in the log agrees, so this
	// self-corrects rather than flapping.
	if isTerminalPhase(current) {
		return current
	}
	// Suppress only the incomplete-log bootstrap Pending. Any non-empty,
	// non-Pending current phase is more advanced than the bootstrap floor and is
	// preserved; a legitimate demotion to Pending has already set current==Pending
	// (or current==""), so this guard is a no-op for it.
	if projected == spiceboxv1alpha1.AgentSessionPhasePending &&
		current != "" && current != spiceboxv1alpha1.AgentSessionPhasePending {
		return current
	}
	return projected
}

// stampTerminalFinish backfills FinishedAt for a phase the steady-state fold
// just produced. The explicit failure/success paths (markBootFailed, the
// runner-crash kill-switch, retry-TTL) stamp FinishedAt themselves, but a plain
// lifecycle fold to a terminal phase — a runner-reported terminal event
// projected by derivePhase/reconcilePhase — does not, leaving every consumer
// that keys off FinishedAt (the terminal-pod reaper, the CLI, the admin UI,
// session GC) with a terminal session that never "finished". This stamps `now`
// on exactly that gap: a terminal phase with no existing stamp. It preserves an
// existing stamp (idempotent — a byte-identical re-apply is a no-op, and the
// original finish time is not overwritten) and never stamps a non-terminal
// phase.
func stampTerminalFinish(phase string, current *metav1.Time, now metav1.Time) *metav1.Time {
	if isTerminalPhase(phase) && current == nil {
		return &now
	}
	return current
}
