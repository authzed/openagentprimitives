package pipeline

import (
	"context"
	"time"
)

// Hook is a single interceptor over the session lifecycle. Implementations are
// constructed with their own dependencies and registered; the executor passes
// only generic Input. A hook reads Input, performs any of its own state
// mutations via its held handles, and returns a Decision as data.
type Hook interface {
	Name() string
	Points() []Point
	Eval(ctx context.Context, in Input) Decision
}

// Host supplies the per-component primitives the executor calls to apply the
// effects a hook returns. channelsd / authzd / runner each implement it.
type Host interface {
	PublishApproval(ctx context.Context, ask ApprovalAsk) (reqID string, err error)
	// AwaitDecision blocks for a human decision, bounded by timeout.
	//   - timedOut == true  ⇒ the deadline lapsed with no decision; err MUST be
	//     nil. The executor turns this into a verdict via ApprovalAsk.OnTimeout;
	//     a timeout NEVER Halts unless OnTimeout == TimeoutHalt. Hosts classify
	//     their own deadline with pipeline.IsTimeout — they do NOT decide the
	//     verdict.
	//   - err != nil (timedOut == false) ⇒ a genuine host-primitive failure the
	//     executor turns into a Halt (fail-closed).
	//   - otherwise ⇒ approved carries the human decision.
	AwaitDecision(ctx context.Context, reqID string, timeout time.Duration) (approved bool, by string, timedOut bool, err error)
	Notify(ctx context.Context, n Notice) error
	SetStatus(ctx context.Context, s StatusUpdate) error
	Halt(ctx context.Context, reason string) error
	Audit(ctx context.Context, recs []AuditRecord) error
}
