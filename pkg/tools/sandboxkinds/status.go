package sandboxkinds

import "errors"

// ErrPreconditionPending means the backend cannot create the sandbox yet, but
// nothing is wrong: a prerequisite it does not own is still being created. The
// controller requeues rather than failing the session.
//
// A backend returning this MUST wrap it with what is being waited on, and its
// Status MUST report PhasePending — "cannot proceed" without saying why is
// indistinguishable from a hang.
var ErrPreconditionPending = errors.New("sandboxkinds: sandbox precondition pending")

// The standard reasons are a shared vocabulary for Status.Reason, so a common
// cause reads the same whichever backend reported it.
//
// They are NOT an enum and nothing switches on them: a backend reports its own
// string where none of these fits, and consumers that must branch use
// Status.Phase. Their value is that an operator reading a condition — or
// comparing two backends — sees the same word for the same thing.
const (
	// ReasonCreating: Pending — the sandbox is being created.
	ReasonCreating = "Creating"
	// ReasonWaitingForPrereq: Pending — blocked on something the backend does
	// not own. Paired with ErrPreconditionPending; the Message names it.
	ReasonWaitingForPrereq = "WaitingForPrerequisite"
	// ReasonReady: the sandbox accepts exec.
	ReasonReady = "Ready"
	// ReasonNotReady: Pending — the sandbox exists but is not serving yet.
	ReasonNotReady = "NotReady"
	// ReasonStartFailed: Failed — it never started (unpullable image, bad config).
	ReasonStartFailed = "StartFailed"
	// ReasonCrashed: Failed — it started, then died.
	ReasonCrashed = "Crashed"
	// ReasonOOMKilled: Failed — killed for exceeding its memory limit.
	ReasonOOMKilled = "OOMKilled"
	// ReasonTerminating: Gone — shutting down; it will not serve exec again.
	ReasonTerminating = "Terminating"
	// ReasonGone: Gone — it no longer exists.
	ReasonGone = "Gone"
)

// StandardReasons returns every standard reason. It exists so a test can assert
// the set and the constants stay in step; it is not a validation list, because
// a backend-specific reason is legitimate.
func StandardReasons() []string {
	return []string{
		ReasonCreating, ReasonWaitingForPrereq, ReasonReady, ReasonNotReady,
		ReasonStartFailed, ReasonCrashed, ReasonOOMKilled, ReasonTerminating,
		ReasonGone,
	}
}

// Runtimes holds one constructed Runtime per sandbox kind, built once at
// startup and injected into the controllers that dispatch on a session's
// resolved kind.
type Runtimes map[string]Runtime

// For returns the runtime for a kind. Fail-closed: an unknown or empty kind
// misses rather than falling back to a default backend, because silently
// running on a different substrate than the one resolved is worse than
// refusing. Safe on a nil Runtimes.
func (r Runtimes) For(kind string) (Runtime, bool) {
	rt, ok := r[kind]
	return rt, ok
}
