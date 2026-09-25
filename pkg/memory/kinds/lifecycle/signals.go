// Package lifecycle is the canonical Kind for session/scope-lifecycle
// signals. Its ScopeHooks listens to every signal in its scope and
// writes a lifecycle Entry — the audit timeline falls out for free.
package lifecycle

import "github.com/authzed/openagentprimitives/pkg/memory"

const (
	SigSessionStarted       memory.SignalKind = "lifecycle/session.started"
	SigSessionPaused        memory.SignalKind = "lifecycle/session.paused"
	SigSessionIdle          memory.SignalKind = "lifecycle/session.idle"
	SigSessionCompleted     memory.SignalKind = "lifecycle/session.completed"
	SigSessionFailed        memory.SignalKind = "lifecycle/session.failed"
	SigSessionAwaitingRetry memory.SignalKind = "lifecycle/session.awaiting_retry"
	SigTurnCompleted        memory.SignalKind = "lifecycle/turn.completed"
)
