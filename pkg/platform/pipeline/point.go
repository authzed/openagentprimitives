package pipeline

// Point identifies a moment in an agent session's lifecycle at which hooks run.
// String values are stable: they appear in audit records and rendered views.
type Point string

const (
	SessionStart Point = "session_start"  // first turn of a genuinely new session
	InboundTurn  Point = "inbound_turn"   // each user message
	PreToolCall  Point = "pre_tool_call"  // before a tool executes
	PostToolCall Point = "post_tool_call" // tool result produced, before it feeds the LLM
	PreResponse  Point = "pre_response"   // before a message is published to the channel
	SessionEnd   Point = "session_end"    // session completes/fails/idles

	// The four metaagent control-plane points. Distinct from the data-plane
	// points above: they mutate WHAT THE SESSION MAY DO, out-of-band to the
	// agent's loop, and are run in sequence by the authzd worker (Received →
	// Extract → Decide → Apply) rather than by the runner. Both lifecycles
	// share the same Point type + Executor/Registry machinery; the metaagent
	// points are just four more string constants.
	MetaagentReceived Point = "metaagent_received" // gate (manage_scope) + classify kind
	MetaagentExtract  Point = "metaagent_extract"  // untrusted-text LLM → delta + shape (authzd-quarantined)
	MetaagentDecide   Point = "metaagent_decide"   // ClassifySkipped widen-bound + ApprovalAsk
	MetaagentApply    Point = "metaagent_apply"    // mutate scope / write cold_start_task + record + notify

	// SessionFork gates a user-triggered fork (restart-from-here) before the
	// operator materializes the child session + copies memory. Fires once, in
	// the operator, outside the data-plane lifecycle.
	SessionFork Point = "session_fork"
)
