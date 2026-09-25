package lifecycle

// Effect is an instruction the caller (the current sequencer) executes. The
// core never performs I/O; it returns Effects to keep side effects centralized
// and ordered.
type Effect interface{ isEffect() }

type baseEffect struct{}

func (baseEffect) isEffect() {}

// AppendLog appends the triggering event to the signed lifecycle log.
type AppendLog struct {
	baseEffect
	Event Event
}

// ProjectStatus tells the caller to write the projected CR status from the new State.
type ProjectStatus struct{ baseEffect }

// StopLoop ends the runner turn loop. A Halt verdict must always stop the loop — no further LLM calls after a halt.
type StopLoop struct{ baseEffect }

// ArmTimer/CancelTimer manage a decision's timeout (owned by the sequencer's clock).
type ArmTimer struct {
	baseEffect
	RequestID string
	Kind      DecisionKind
}
type CancelTimer struct {
	baseEffect
	RequestID string
}

// Notify is a best-effort wake/notify nudge (NATS), never the source of truth.
type Notify struct {
	baseEffect
	Reason string
}

// Unpark releases a blocked dispatch goroutine with the resolution.
type Unpark struct {
	baseEffect
	RequestID string
	Approved  bool
	TimedOut  bool
}

// ReissuePending re-arms waits after a restart re-fold, so every outstanding decision the prior runner left open is re-registered with the orchestrator.
type ReissuePending struct {
	baseEffect
	Pending []PendingDecision
}

// MarkPlanStopped tells the runner sequencer to mark the agent's plans
// state-kind stopped on interrupted termination (SIGTERM / admin-kill / crash). The operator never
// interprets this — it has no in-memory plans store; its stop paths deliver a
// pod SIGTERM that the runner turns into a Stopped event and self-marks.
type MarkPlanStopped struct{ baseEffect }
