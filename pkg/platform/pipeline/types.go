package pipeline

import (
	"encoding/json"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// SessionRef identifies the session a lifecycle event belongs to.
type SessionRef struct {
	Namespace string
	Name      string
	Class     string // AgentClass name
}

func (s SessionRef) String() string { return s.Namespace + "/" + s.Name }

// TurnInfo is the payload for SessionStart / InboundTurn.
type TurnInfo struct {
	Text     string // the user message (or initial prompt at SessionStart)
	InboxIdx int
}

// ToolCallInfo is the payload for PreToolCall / PostToolCall.
type ToolCallInfo struct {
	Name   string
	Args   json.RawMessage
	Result string // populated at PostToolCall only

	// UseID is the LLM tool_use block ID assigned by the model (e.g. the
	// Anthropic tool_use content block's "id" field). It links the dispatch,
	// taint records, and approval audit entries for a single tool invocation.
	// Populated by the Phase 1B dispatch site; "" until then.
	UseID string

	// Justification is the agent's stated reason for the call, extracted by
	// the runner from the _reason field of the tool-args envelope (if present).
	// Carried into the hooks so the approval envelope can surface it to
	// approvers without re-reading primary LLM output. "" when not present.
	Justification string

	// IsError is populated at PostToolCall only: true when the Execute
	// outcome was a failure (Go error from dispatch, or Result.IsError).
	// Post hooks that only inspect successful results (scope hard-deny,
	// info-leakage) early-return on it; toolguard's GuardRecord consumes it.
	IsError bool

	// GovernedMeta marks a meta tool that entered the contained pipeline ONLY
	// to be seen by the plan gate (a tool.PlanGateGoverned tool, i.e. delegate).
	// It reaches no external SpiceDB resource and carries no toolResourceMap
	// entry, so the info-leakage read/audience gates — which deny an unmapped
	// tool fail-closed — must skip it, exactly as they never see any other meta
	// tool (which take the ungated path). Only the plan gate acts on it. Set by
	// the dispatch site; false for every ordinary tool.
	GovernedMeta bool

	// UnbilledFailure is populated at PostToolCall only, from
	// tool.Result.UnbilledFailure: this failed call's provider metered nothing
	// for it, which is the shape of a credential it refused rather than of work
	// that ran and failed. toolguard's GuardRecord answers a consecutive streak
	// of them with a session halt.
	//
	// A structured observation the PLATFORM made from the toolkit's own
	// terminal event — never anything derived from Result above. See
	// tool.Result.UnbilledFailure for why a text-matched version of this would
	// be a prompt-injection primitive.
	UnbilledFailure bool
}

// ResponseInfo is the payload for PreResponse.
type ResponseInfo struct {
	Text string
	// HasAttachments reports that this reply carries files alongside Text.
	//
	// The per-datum audience check measures Text and nothing else, so a
	// fully-tagged sentence delivered with an untagged file used to be allowed
	// on the strength of the sentence. Attachments are not a minor side
	// channel: artifact#view resolves through parent->interact, so every
	// channel member who can interact can open the render, and the
	// artifact-preparation path carries no leakage hook of its own.
	//
	// The gate does not try to measure the files. It uses this to know that
	// part of what is being sent is unmeasured, and falls back to the coarse
	// session-wide check — preserving the invariant that per-datum tags only
	// ever REFINE a decision the coarse floor would otherwise make.
	HasAttachments bool
}

// SessionEndInfo is the payload for SessionEnd.
type SessionEndInfo struct {
	Reason string // why the session ended (completed|failed|idle)
	// Model and the token buckets are the session's cumulative LLM usage at
	// termination, populated by the runner so post-session reporters (cost) can
	// read them straight off the Input without reaching into the host.
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64

	// ByModel is the same cumulative usage broken down per actually-served
	// model, in first-encountered order, so a cost reporter can price and
	// attribute per model instead of blending everything under the configured
	// Model above. Empty means "no per-model breakdown available" — reporters
	// must NOT read that as zero usage.
	ByModel []ModelUsage
}

// ModelUsage is one served model's cumulative usage for a session, keyed by its
// display id. It carries full token counts rather than only cost so an
// unreported-cost bucket can still be priced through the same per-token math as
// the session total.
type ModelUsage struct {
	// Display is the uniform served-model id (e.g.
	// "openrouter/anthropic/claude-3.5-sonnet") — the UI/breakdown key.
	Display string
	// Model is the bare model id (Display minus its "<provider>/" prefix),
	// used to look up per-model pricing in the provider's table.
	Model string

	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64

	// ReportedCostMicroUSD is the sum of the provider's per-turn reported
	// dollar cost (converted to micro-USD) across turns served by this
	// model, valid only when CostReported is true.
	ReportedCostMicroUSD int64
	// CostReported is true when at least one turn served by this model
	// carried a provider-reported cost (e.g. OpenRouter's Usage.CostUSD).
	CostReported bool
}

// MetaagentInfo is the payload for the four metaagent control-plane points
// (MetaagentReceived/Extract/Decide/Apply). It carries ONLY generic primitives
// — pkg/platform/pipeline imports no domain types, so the AgentClass envelope, the
// extracted ScopeDelta, and the derived shape flow through per-request hook
// deps + the authzd Host scratch, NOT through this struct.
type MetaagentInfo struct {
	Kind      string // "cold_start" | "mid_session"   (classified at Received, pre-LLM)
	Trigger   string // "session_start" | "mention"
	Requester string // canonical subject of the requester
	Text      string // raw untrusted request text
	AutoApply bool   // extractAndAutoApply (cold_start only; false for mid_session)
	InboxIdx  int    // cold-start turn-0 index
}

// ForkInfo is the payload for the SessionFork point. Like MetaagentInfo it
// carries ONLY generic primitives — pkg/platform/pipeline imports no domain types.
type ForkInfo struct {
	ParentRef string // "ns/name" of the session being forked
	ChildRef  string // "ns/name" of the child to be created
	Forker    string // canonical "user:<...>" subject (PendingRestart.TriggeredBy)
	CutTurn   int    // last parent turn copied (inclusive)
}

// Input is the generic event data passed to every hook. Exactly the
// point-relevant payload pointer is non-nil; the rest are nil.
type Input struct {
	Point   Point
	Session SessionRef
	// Requester is the acting principal, and it is NOT always a human: on an
	// inbound whose Channel supplies no per-user identity it holds the
	// Channel's own declared subject verbatim, already qualified — a
	// "service:<id>" or an "agentsession:<ns>/<name>". Call
	// identity.CanonicalUserID.SubjectRef before rendering or checking it; the
	// bare Subject() would prefix "user:" onto those and name nobody. May be ""
	// at SessionEnd.
	Requester identity.CanonicalUserID
	// Subjects is the tool_call authz-check subject-list (authz.Inputs.Subjects):
	// the loop's l.authSubjects on the LLM path, or the single widget-viewer
	// subject on a proxy-exec (app-tool) path. Empty on non-tool-call points.
	//
	// Requester and Subjects are the ONLY principal a tool-call hook may
	// authorize against — ToolCallAuthz and the info-leakage read/audience
	// gates alike, never the runner's own l.authSubject / l.authSubjects. That
	// field is not a stale-but-equivalent copy: on a proxy-exec it is a
	// DIFFERENT principal (the session's current requester, not the widget
	// viewer), and advanceRequester writes it on the loop goroutine while a
	// proxy-exec runs on the NATS-handler or detached-approval goroutine — so
	// reading it there is at once a wrong-principal authorization and a data
	// race. A gate resolving its subject from a dependency closure rather than
	// from this struct is the shape that bug takes; pass the per-call value
	// into the closure instead.
	Subjects []string
	// ProxyExec marks this as a viewer-attributed proxy execution — an MCP-UI
	// app-tool call the widget VIEWER drives, not the LLM. When set, the
	// ToolCallAuthz hook attributes the approval-card Requester to Requester
	// (the viewer, D-D1), not the session's LastInbound requester. false on the
	// LLM path.
	ProxyExec bool
	// UIDataBinding marks a result bound for a BROWSER rather than the model:
	// an agent-UI data binding, resolved server-side under the viewer's
	// subject and never appended to the transcript. It selects the UI ingress
	// ceiling in toolguard's GuardRecord; false on every other path,
	// including an mcp-ui widget's app-tool call, whose result the widget
	// renders but which is not a declaration-driven data binding.
	UIDataBinding bool
	Turn          *TurnInfo
	Tool          *ToolCallInfo
	Response      *ResponseInfo
	End           *SessionEndInfo
	Metaagent     *MetaagentInfo
	Fork          *ForkInfo
}

// Verdict is a hook's gate decision. Allow is the zero value so an empty
// Decision{} means "allow, no effects".
type Verdict int

const (
	Allow Verdict = iota
	Deny          // short-circuits this point; reason surfaced to the caller
	Halt          // short-circuits AND ends the session (fail-closed)
)

// TimeoutPolicy is what the executor does when an approval await times out.
// TimeoutDeny is the zero value: the safe, non-crashing default.
type TimeoutPolicy int

const (
	TimeoutDeny TimeoutPolicy = iota // sticky-deny; the turn continues
	TimeoutHalt                      // fail-closed; end the session
)

// ApprovalAsk signals the executor to pause: publish an approval request, await
// a human decision (bounded by Timeout), then resume (approve) or Deny.
type ApprovalAsk struct {
	Kind      string         // e.g. "tool_call" | "leakage_share" | "cold_start"
	Summary   string         // approver-facing text (already prompt-injection-safe)
	Payload   map[string]any // rendering details for the channel
	Timeout   time.Duration  // 0 ⇒ Host's default
	OnTimeout TimeoutPolicy  // default TimeoutDeny; stamped from the lifecycle decisionParams table

	// CoalesceKey names the DECISION this ask represents. Concurrent asks
	// carrying the same key resolve as one: the first publishes and awaits, the
	// rest wait on its outcome.
	//
	// A turn's tool calls are dispatched concurrently, so several can reach the
	// same unapproved plan phase at once. Each raised its own card — observed
	// live as two `card_built` records in the same second, and again as two
	// `amendment_requested`. Approval volume scaled with the model's
	// PARALLELISM rather than with what was being decided.
	//
	// Empty means no coalescing: the ask stands alone. That is the default and
	// the safe direction — an ask that does not opt in is never merged with
	// another, so a single click can never grant authority an approver did not
	// separately consider. Only asks that are genuinely THE SAME decision
	// should set it.
	CoalesceKey string
}

// Notice is a user-facing message the executor delivers via the Host.
type Notice struct {
	// Notice is the user-facing message. Hooks build it with notice.New
	// against a registered category, so a hook supplies copy while the
	// registry supplies tone — the same split every other user-facing message
	// in the system uses.
	Notice *notice.Notice
	// ToRequester routes the delivery: true ⇒ ephemeral/DM to the requester,
	// false ⇒ the channel. It stays on this struct rather than moving into the
	// notice's Audience because a Host resolves it against transport it owns
	// (a Slack ephemeral, a browser toast), which the notice model has no view
	// of at hook time.
	ToRequester bool
}

// Text renders the notice for a Host whose only delivery surface takes a
// string.
//
// Lead plus next step and nothing else: on a plain-text surface those are the
// two things a stuck reader needs, and the supporting fields have no room to
// render legibly. A Host with a richer surface should read Notice directly and
// render it properly rather than calling this.
func (n Notice) Text() string {
	a := n.Notice.Args()
	if a.NextStep == "" {
		return a.Lead
	}
	return a.Lead + " — " + a.NextStep
}

// StatusUpdate is a transient session status (e.g. "awaiting approval…").
type StatusUpdate struct {
	Text string
}

// AuditRecord is one durable audit entry a hook wants written.
type AuditRecord struct {
	Kind   string
	Fields map[string]any
}

// Decision is what a hook returns. State mutations (taint, scope, binding) are
// performed by the hook itself via its own handles; only these flow back.
type Decision struct {
	Verdict  Verdict
	Reason   string
	Approval *ApprovalAsk
	Notices  []Notice
	Status   *StatusUpdate
	Audit    []AuditRecord

	// Definition, when non-nil, says this non-Allow verdict was caused by the
	// AGENT'S OWN DEFINITION being unevaluatable — a CEL constraint that will
	// not compile or errors at evaluation, a spec the validator cannot apply
	// — rather than by a policy that considered the call and refused it.
	//
	// Both fail closed, and must — a gate that cannot be evaluated has to be
	// treated as a gate that said no — but they want different handling. A
	// refusal is the system working, and the person who hit it needs to know
	// they lack access. A definition error is the system broken: NOBODY gets
	// past it, so telling that person they lack access is false, sends them to
	// request a permission that would not have helped, and reaches no one who
	// can fix it.
	//
	// A hook sets this whenever the deny came from ITS OWN configuration
	// failing rather than from the caller. Only the hook can tell the two
	// apart: once a deny is a Reason string, the distinction is gone. It
	// carries the cause rather than a bool because the consumers need different
	// depths of it — the viewer gets fixed copy naming no internals, the
	// monitoring report needs the locus and the authored expression.
	//
	// Non-nil-ness IS the classification, so never assign a typed-nil pointer
	// here (AGENTS.md, "Nil interfaces"): a nil *DefinitionError would make
	// every ordinary refusal report itself as broken configuration, then panic
	// on the first field read.
	Definition error
}

// Outcome is the executor's final result for a point.
type Outcome struct {
	Verdict   Verdict
	Reason    string
	FiredHook string // name of the hook that produced a non-Allow verdict (diagnostics)
	// Definition is the deciding hook's Decision.Definition — see there for
	// what it distinguishes. Carried through so the caller acting on the
	// verdict can tell a refusal from a broken definition, and can report the
	// breakage in detail, without parsing Reason — which is prose, and free
	// to change.
	Definition error
}
