// Package toolkitstream defines the per-toolkit stream-parser plug-in
// surface. A Parser consumes an interactive tool's stdout chunks and
// emits a small neutral Event vocabulary the channel kinds render.
//
// The package has zero dependencies on Kubernetes APIs or transport
// machinery so parser plug-ins can be unit-tested in isolation.
package toolkitstream

// EventType is the closed set of neutral events parsers may emit.
// Mirrors AssistantStreamDeltaPayload's vocab (text_delta,
// tool_use_start, tool_use_stop) plus a "result" event for
// per-session terminal summaries (cost, duration).
type EventType string

const (
	EventTextDelta    EventType = "text_delta"
	EventToolUseStart EventType = "tool_use_start"
	EventToolUseStop  EventType = "tool_use_stop"
	EventResult       EventType = "result"
)

// Event is the neutral payload published by the runner as
// KindToolSessionEvent. Fields are populated per-EventType — see
// the per-field comments below.
type Event struct {
	Type EventType

	// Populated for EventTextDelta.
	Text string

	// Populated for EventToolUseStart and EventToolUseStop.
	// ToolID correlates Start/Stop pairs across the event stream.
	ToolName string
	ToolID   string
	Summary  string // Start: input gloss; Stop: result gloss.

	// EventToolUseStop: did the tool succeed.
	// EventResult: did the overall session succeed.
	OK bool

	// EventResult only.
	DurationMs int64
	CostUSD    float64
}

// Outcome is the caller-facing result a Parser distills for the orchestrator
// LLM, distinct from the per-Event stream (which feeds the channel). It is
// populated from the tool's own terminal output — for claude, the stream-json
// `result` event. Valid after Done().
type Outcome struct {
	// Text is the inner tool's final, deliberate output. For claude this is the
	// terminal result event's `result` field — its final --print answer. Empty
	// when the tool produced no such text.
	Text string
	// HasResult reports whether the tool emitted a recognizable terminal result
	// event. Distinguishes "succeeded, no text" from "never reported".
	HasResult bool
	// OK mirrors the terminal result's success flag; meaningful when HasResult.
	OK bool
	// DurationMs and CostUSD mirror the terminal result's metadata when present.
	DurationMs int64
	CostUSD    float64

	// Unbilled reports that the toolkit's provider metered NOTHING for this
	// run. Set only by a parser whose toolkit reports a billing tally in its
	// terminal event; every other parser leaves it false.
	//
	// It is not `CostUSD == 0` spelled differently, and collapsing the two
	// would be a defect. A zero CostUSD is also what a toolkit that never
	// reports cost looks like, so it cannot tell "the provider charged
	// nothing" from "this toolkit said nothing about cost" — and
	// UnbilledFailure answers that difference with a session halt. Keeping the
	// claim explicit means a new parser plugs in without inheriting a halt it
	// never opted into.
	Unbilled bool
}

// UnbilledFailure reports the shape of a run that never got past its
// provider's auth layer: the toolkit reached its own terminal result, that
// result was not a success, and the provider metered nothing for it. A run the
// provider authenticated is billed for the work it then did, so a non-success
// that cost nothing is a run that never started.
//
// This is the credential-halt classifier (pkg/authz/toolguard answers a
// consecutive streak of these by ending the session), and it deliberately
// reads THREE structured facts and no text. Text is not evidence here: the
// agent picks the argv, every CLI echoes argv back into its own errors, and
// claude writes its authentication error to STDOUT — so matching on output
// would hand any upstream able to write into a result the ability to end a
// session on demand, or to dress an authentication failure up as ordinary work
// and hide one.
func (o Outcome) UnbilledFailure() bool {
	return o.HasResult && !o.OK && o.Unbilled
}

// Parser converts an interactive tool's stdout chunks into neutral
// Events. Implementations buffer across chunks for line-delimited
// formats. Stderr is NOT parsed — the sandbox tool routes stderr to
// the raw delta path regardless.
//
// Parser instances are per-session (one per interactive ToolCall);
// the registry holds Factory values that mint fresh Parsers.
type Parser interface {
	// Parse consumes one stdout chunk and returns zero or more
	// events. Returns an error only for unrecoverable internal
	// state; transient malformed bytes are dropped silently and
	// optionally surfaced as a synthetic text_delta event
	// ("[stream truncated]") so the user sees something happened.
	Parse(chunk []byte) ([]Event, error)

	// Done is called once when the tool exits. Flushes any pending
	// buffer and returns a final synthetic event when appropriate
	// (e.g. emit a result event if the tool exited without one).
	Done() ([]Event, error)

	// Outcome returns the caller-facing result accumulated across the stream,
	// valid after Done(). Zero value (HasResult == false) when the tool
	// produced no recognizable terminal result.
	Outcome() Outcome
}

// Factory builds a per-session Parser. Implementations register a
// single Factory at init() time via pkg/tools/toolkitstream/registry.
type Factory interface {
	// Kind matches Toolkit.StreamFormat. Pattern: lowercase,
	// dash-separated. Example: "claude-stream-json".
	Kind() string

	// New returns a fresh Parser for one interactive session.
	New() Parser
}
