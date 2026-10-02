package runner

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/approval/summarizer"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/identityadvisor"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/userprofilegate"
	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/authfail"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/grants"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"

	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// MemoryAppender is the runner's view of the session transcript. It is
// satisfied by *turn.Appender — HTTP-backed in production (internal/cmd/runner),
// in-process for local mode (LocalMemoryAdapter).
type MemoryAppender interface {
	ReadAll(ctx context.Context) ([]memory.Turn, error)
	ReadAfter(ctx context.Context, after int) ([]memory.Turn, error)
	Append(ctx context.Context, t memory.Turn) error
}

// Loop holds the runner-loop dependencies. Construct via the internal/cmd/runner
// entrypoint or directly in tests.
type Loop struct {
	Provider llm.Provider
	Memory   MemoryAppender

	// Mem is the extensible-memory framework facade, independent of the
	// HTTP-backed Memory field above (which owns turn transcripts). Emits
	// lifecycle signals and records authz/relwrite/label/approval entries.
	// Nil-safe at every call site: production wires a non-nil instance, tests
	// often leave it nil.
	Mem memory.Memory
	KG  tool.KGQuerier

	// LifecycleMemory is the session-signed memory facade the runner Puts typed
	// lifecycle transition events into (publisher session:<ns/name>). The SAME
	// signing facade as Mem in production wiring, named separately so the
	// sequencer's intent is legible at its call sites and tests can leave it nil.
	// When nil every emit is a logged no-op: control flow still runs (StopLoop is
	// decided locally), but no durable transition log is written. See sequencer.go.
	LifecycleMemory memory.Memory

	Status *StatusPatcher
	Tools  []tool.Tool
	// AppTools holds MCP-UI app-visible tools, keyed by tool name. They are
	// DELIBERATELY separate from Tools: NEVER placed in the LLM tool list
	// (buildToolDefs iterates Tools only), and reachable only through the
	// viewer-driven app-tool path (handleAppToolCallReq, apptoolcall.go). Empty
	// for agents with no opted-in app-tools. See
	// pkg/agent/tool/mcp.SynthesizeResult.AppTools.
	AppTools map[string]tool.Tool
	// AppToolRateLimiter caps autonomous app-tool calls per MCPServer origin
	// (mcpUiAppTools.maxCallsPerMin); app-tool path only, not toolguard. Nil
	// means unlimited (no configured origin, or a caller/test that doesn't
	// wire one).
	AppToolRateLimiter *AppToolRateLimiter
	// approvalAskSpawnHookForTest, when non-nil, is invoked synchronously on the
	// caller's own goroutine immediately before handleAppToolCallReq spawns a
	// side-effecting call's detached executeToolContained goroutine (apptoolcall.go
	// step 7b), so a test can count approval-flow spawns without racing that
	// goroutine. Test-only observability seam: every production construction path
	// leaves it nil, and handleAppToolCallReq never reads it back.
	approvalAskSpawnHookForTest func()
	System                      string
	UserPrompt                  string
	Budget                      *Budget
	// RunClock accumulates active run-time (excludes human waits). Shared with
	// Budget (which reads Elapsed for maxDuration). Paused/resumed at the same
	// sites as the progress reporter; flushed to status.runDuration before the
	// pod can sleep. May be nil (in-process/local callers); all uses nil-safe.
	RunClock  *RunClock
	Model     string
	MaxTokens int

	// Routing carries the resolved OpenRouter dynamic-routing preferences
	// (status.effectiveSettings.modelRouting, converted by internal/cmd/runner) onto
	// every llm.Request this loop builds. Ignored by non-OpenRouter
	// providers. Nil = no routing preferences (the common case).
	Routing *llm.OpenRouterRouting

	// ReportSessionCost gates the post-session cost reporter hook. Resolved from
	// status.effectiveSettings.reportSessionCost by internal/cmd/runner.
	ReportSessionCost bool

	// ModelInputPerMTok/ModelOutputPerMTok are the resolved catalog price for the
	// session's model (USD/MTok), 0 when the catalog carries no price. When >0 the
	// cost hook prefers them (catalog-authoritative) over the provider's built-in
	// table, deriving cache buckets via the standard ratios.
	ModelInputPerMTok  float64
	ModelOutputPerMTok float64

	// UserID is stamped onto each llm.Request as the opaque request tag
	// (AgentSession UID). Empty = untagged.
	UserID string

	// ExtraToolDefs are tool defs added to the LLM request alongside the
	// client-side l.Tools. Used to inject Anthropic server-tool defs
	// (web_search_20250305, web_fetch_20250910) that the LLM API runs
	// inline rather than dispatching back to the runner. Empty for
	// channel-attached agents; set by the gen-agent driver.
	ExtraToolDefs []llm.ToolDef

	// ReplayToolCatalog narrows the tool set offered on each request to the one
	// a captured session recorded for that turn. TEST-ONLY: nil in production,
	// which is what every binary passes, and a nil one is never consulted.
	//
	// It exists because a whole-session replay boots a fixture the capture had
	// to rewrite to make bootable, and one of those rewrites — dropping a
	// SidecarToolbox's spec.secretInputs, a gate no fixture can satisfy —
	// hands the replayed run tools the captured run did not have until much
	// later. Handed the recorded set instead, the run offers what was offered,
	// and every difference is reported rather than absorbed: a recorded tool
	// now missing, or an extra nobody declared, is a regression in a gate that
	// no other assertion a bundle can make would see.
	//
	// Called on the turn loop's own goroutine, once per request, with EVERY
	// name on the request (provider server-tool defs included, because that is
	// what the tool_catalog Kind records). It returns the names to keep.
	//
	// Typed as a plain func over names rather than as the bronzethread check so
	// the runner does not depend on the bundle format, the same shape the id
	// minters take.
	ReplayToolCatalog func(turnIndex int, offered []string) []string

	// SessionKey identifies the session for log/error messages.
	SessionKey memory.NamespacedName

	// SessionContext is optionally pre-built by internal/cmd/runner (which populates
	// K8sClient, ArtifactClient, BundleSessions, AgentSessionUID). When nil,
	// Run default-constructs a minimal context (tests use this path).
	// SubmitResult is always bound at Run time.
	SessionContext *tool.SessionContext

	// sessionCtxMu guards the l.SessionContext pointer and its Run-time field
	// setup (SubmitResult/Mem/KG) against every off-loop reader: the NATS-goroutine
	// app-tool reader (appToolSessionContext / HandleAppToolCall), the
	// SIGTERM/SIGINT stop handler (MarkPlanStoppedBestEffort + seqForEmit), and
	// anything else reaching seqForEmit off-loop (the NATS revocation subscriber's
	// EmitRevoked, claimAndRecover's decision-re-arm goroutine). Loop-goroutine
	// readers are sequential with the Run-entry write and need no lock; an RLock
	// taken there is harmless.
	sessionCtxMu sync.RWMutex

	// toolsMu guards the l.Tools slice field against the one genuinely off-loop
	// reader — lookupTool, reached from the NATS-handler goroutine
	// (HandleAppToolCall → executeToolContained → the contained pipeline's hooks)
	// — racing ToolRefresher's mid-turn reassignment (l.Tools =
	// applyToolRefresh(...)) on the Run goroutine. Every other l.Tools reader runs
	// on the loop goroutine, sequential with ToolRefresher, so only lookupTool
	// RLocks. l.AppTools is unguarded: read-only after setup.
	toolsMu sync.RWMutex

	// PublishTurnActivity, when non-nil, publishes a planless KindTurnActivity
	// envelope at active⇄paused transitions so channelsd's silence watchdog can
	// arm or tear its indicator down without inferring from status polling.
	// seq/uid carry the envelope's logical order + session instance (computed by
	// emitActivity via seqForEmit) so consumers can order activity signals against
	// status captions. Nil for kubectl-driven sessions and tests (emit skipped).
	PublishTurnActivity func(ctx context.Context, active bool, cause string, seq uint64, uid string)

	// PublishPlanActivity, when non-nil, emits a plan_update snapshot carrying
	// the active/paused flag for one plan. seq/uid are stamped onto the snapshot
	// envelope (see PublishTurnActivity). Wired by internal/cmd/runner to the NATS
	// publisher; nil for kubectl-driven sessions and tests (emitActivity
	// then no-ops).
	PublishPlanActivity func(ctx context.Context, plan plans.Plan, paused bool, cause string, seq uint64, uid string)

	// AnnotationSummarizer is the isolated summarizer LLM
	// (pkg/agent/runner/approval/summarizer) maybeEchoAnnotationTurn uses to turn a
	// browser annotation batch's TRUSTED prose — the numbered
	// comment/intent/severity/target lines stripUntrustedBlocks leaves behind,
	// never the untrusted DOM JSON — into the KindUserEcho mirror's one-sentence
	// text. Nil ⇒ fall back to the deterministic
	// summarizer.FallbackAnnotationSummary: the mirror's availability must never
	// couple to the summarizer LLM's.
	AnnotationSummarizer summarizer.Provider

	// EchoPublish is the raw outbound NATS publish handle. maybeEchoAnnotationTurn
	// uses it (via channelevents.PublishOut) to mirror an annotation batch's
	// summary as a KindUserEcho envelope — the ONLY mirror an annotation turn
	// gets, since channelsd suppresses its raw echo (DeferEcho) — and
	// applyUIResource reuses the SAME handle for the KindSessionViewOffer
	// escalation anchor, both being outbound envelopes on the runner's one NATS
	// connection. Nil for kubectl-driven sessions and tests, in which case each
	// caller logs and skips its publish rather than dropping it silently.
	EchoPublish channelevents.PublishFunc

	// EnvelopeSigner signs every envelope the loop publishes with the
	// session's identity key. Nil-safe: a nil signer leaves the envelope
	// unsigned, which is what every EchoPublish call site already tolerates
	// (kubectl-driven sessions and tests).
	EnvelopeSigner *channelevents.EnvelopeSigner

	// definitionErrSeen dedups agent-definition fault reports (one monitoring
	// event per distinct fault) — see reportDefinitionError. Guarded because tool
	// dispatch runs concurrently: dispatchToolUses fans a turn's calls out across
	// goroutines and the NATS app-tool handler adds more, so one broken rule can
	// reach this map from several goroutines at once.
	definitionErrMu   sync.Mutex
	definitionErrSeen map[string]bool

	planActivityMu sync.Mutex
	lastPlanPaused *bool // last activity state emitted; nil = none emitted yet

	// lastAssistantTurnIndex is the durable memory Index of the most recent
	// assistant turn (initialized to the replay base, advanced each turn).
	// seqForEmit uses it as the memTurnIndex when emitActivity fires WITHOUT a
	// per-tool-call IDs context (start-of-turn, fireSessionEnd, retry/fail pauses),
	// so loop-side activity signals still sort against tool-block events.
	//
	// Written only on the main loop goroutine, but read from three genuinely
	// off-loop goroutines that reach seqForEmit's fallback with an IDs-free ctx:
	// the oap.revocation NATS callback (EmitRevoked → applyEvent → AppendLog →
	// lifecycleOrderKey), the SIGTERM/SIGINT stop handler
	// (MarkPlanStoppedBestEffort — internal/cmd/runner cancels the run context only AFTER
	// it returns, so the loop is still turning), and claimAndRecover's decision
	// re-arm goroutines (DecisionResolved → AppendLog). The loop's write holds no
	// lock, so there is no happens-before edge with them. Atomic rather than
	// mutex-guarded: an independent scalar, not part of the SessionContext
	// snapshot sessionCtxMu makes consistent, and seqForEmit already runs under
	// seqMu. Access ONLY through setLastAssistantTurnIndex /
	// lastAssistantTurnIndexValue — the type is what enforces the guard.
	lastAssistantTurnIndex atomic.Int64

	// approvalPauseDepth counts in-flight approval blocks so the plan card stays
	// paused until the LAST concurrent approval resolves: several approval-gated
	// tool calls can block in parallel within one turn, and one resolution must
	// not flip the card back to active while others are still pending. Guarded by
	// approvalPauseMu. See enterApprovalPause.
	approvalPauseMu    sync.Mutex
	approvalPauseDepth int

	// seqMu serializes every lifecycle transition append so the session's
	// per-publisher signing chain advances monotonically even when several
	// dispatch goroutines resolve hook/decision outcomes in one turn. The
	// fold→transition→append step is not atomic on its own; this lock makes
	// it so within the runner. See sequencer.go.
	seqMu sync.Mutex

	// lifecycleLog holds the transition log foldLifecycle has already decoded, so
	// a per-event fold reads only the tail rather than the whole append-only log.
	// Without it the sequencer's fold→append step costs O(E²) bytes and decodes
	// across a session, inside seqMu, with E unbounded in session age.
	//
	// The zero value is ready, and every read re-queries (so the operator's
	// appends to the same scope are picked up). Entered ONLY from foldLifecycle,
	// which runs only under seqMu — see the lock-order note there. A value, not a
	// pointer: Loop holds mutexes and is already non-copyable.
	lifecycleLog lifecycle.IncrementalReader

	// dispatchHalted is set by dispatchToolUses when a PreToolCall/PostToolCall
	// hook returned Halt in the just-finished dispatch. Run reads it after the
	// dispatch call to break the turn loop (a Halt must stop the loop —
	// no further Provider.Send). Set and read on the main loop goroutine only
	// (after wg.Wait), so it needs no lock of its own.
	dispatchHalted bool

	// deliveredThisRound records whether respond_to_user has successfully put
	// anything on the channel since the current round began (session start, or the
	// last inbound user message). needsDeliveryNudge reads it to decide whether a
	// round-terminating tool would end the round in silence. Written from the
	// per-tool dispatch goroutines, hence deliveredMu rather than dispatchHalted's
	// main-goroutine-only contract.
	deliveredThisRound bool
	deliveredMu        sync.Mutex

	// slotPinRefusals holds, PER RESOURCE TYPE, the newest user-visible refusal
	// produced when a mid-turn slot promotion (extracted at the autofill site,
	// observed after dispatch) was refused by a single-occupancy pin — the text
	// names the pinned instance and the route out. A subsequent tool call denied
	// on the same resource type reads the matching refusal appended to its
	// result, so the model learns WHY the instance it named is unusable instead
	// of seeing a bare "permission denied" that reads as a system fault.
	//
	// Keyed by type with newest-wins overwrite, and CLEARED whenever that type
	// binds or its pin moves (the promote nil-error paths, narrowToApproved's
	// success path): a refusal that said "pinned to A" must not outlive an
	// approved A→B move, or the model is handed advice about a pin that no
	// longer exists. Written from the per-call dispatch goroutines (extracted),
	// the loop goroutine (observed) and the approval host (clear on move), read
	// from the per-call goroutines at denial time — hence the mutex.
	// Process-scoped and advisory: losing it on restart costs one unexplained
	// denial, never a wrong authorization.
	slotPinRefusalMu sync.Mutex
	slotPinRefusals  map[string]slotPinRefusal

	// uiEscalated is set by applyUIResource the first time a tool result carries a
	// UIResource (the MCP ui:// interception), and gates every later widget in the
	// same run from re-publishing the session_view_offer anchor. Best-effort: it
	// resets on a fresh Run (pod respawn), acceptable since a resumed session may
	// reasonably re-offer the anchor once. Read/written only in the per-tool-result
	// loop after dispatchToolUses' wg.Wait, same single-goroutine contract as
	// dispatchHalted, so it needs no lock.
	uiEscalated bool

	// attachmentNonces memoizes the untrusted-marker nonce minted for each
	// attachment, keyed by its artifact handle, so an attachment renders
	// byte-identically on every request. Hydration re-derives native blocks from
	// their refs each request (attachments.go), so a per-call nonce would change
	// the marker text either side of every attached block on every turn — and a
	// cache breakpoint only helps up to the first changed block, so one image
	// early in a long conversation would re-cost everything after it, every turn.
	// Reuse is safe because a nonce is unforgeable BY THE FILE IT WRAPS: the bytes
	// are frozen at upload, before the nonce exists, so no file can contain its
	// own closing marker however long that nonce lives.
	//
	// Guarded because Loop is shared with genuinely off-loop goroutines (see
	// toolsMu, sessionCtxMu) and this is a WRITTEN map, though only the Run
	// goroutine's hydration pass touches it today. Process-scoped; losing it on
	// restart re-mints and costs one cold prefix.
	attachmentNoncesMu sync.Mutex
	attachmentNonces   map[string]string

	// nativeSuppressed disables native attachment blocks for the rest of the
	// session; set once by the provider-error path when a request carrying a
	// native block was rejected. A guard cannot close the malformed-bytes class:
	// the declared MIME is a hint, not a verified fact, and only the provider
	// knows what it will accept. An attachment with no text fallback lives in the
	// persistent window, so hydration rebuilds the rejected block on EVERY request
	// — without this switch the retry only reproduces the failure until the budget
	// drains and the session ends terminally, and the durable turn replays it
	// after a restart. One malformed screenshot would kill the conversation.
	//
	// Atomic rather than left to the Run goroutine's sequencing, so an off-loop
	// reader is not a race waiting to be rediscovered. Deliberately NOT persisted:
	// a fresh process gets one more chance, right if the rejection was transient
	// and one extra call if it was not.
	nativeSuppressed atomic.Bool

	// pinnedAttachments are artifact handles the agent asked to see again via
	// show_attachment. A pinned attachment is exempt from the newest-N window —
	// the agent has said explicitly that this file is the one it needs — but from
	// nothing else: the model must still declare the MIME, and the per-request
	// byte budget still applies, since that is what keeps the request under the
	// provider's hard ceiling.
	//
	// Pinning needs no authorization check because it is inert on its own:
	// hydration only ever consults this set for refs ALREADY present in this
	// session's conversation, so a handle from somewhere else can be pinned and
	// will never be read. PinAttachment still validates so the agent gets a
	// straight answer instead of silence, but the security property does not rest
	// on that validation.
	pinnedMu          sync.Mutex
	pinnedAttachments map[string]bool

	// prevBreakpointMsg is the message index that carried the moving cache
	// breakpoint on the previous request; it becomes this request's anchor. That
	// position normally holds a written cache entry, but not necessarily:
	// markCacheBreakpoints advances this index BEFORE Provider.Send, so a request
	// cancelled before being sent (a user interrupt) leaves the next anchor
	// pointing at a position nothing ever wrote — one wasted breakpoint on the
	// following request, not a correctness issue. hasPrevBreakpoint distinguishes
	// "no previous request" from a genuine index 0: Loop is built as a bare struct
	// literal at every call site, so the zero value must stay a safe default
	// rather than require a sentinel everywhere.
	//
	// Per-request conversation state, not durable session state: losing it on
	// restart costs one cold prefix, nothing more.
	prevBreakpointMsg int
	hasPrevBreakpoint bool

	// interrupts holds the cancel funcs for tool_use dispatch goroutines (and
	// the per-turn LLM send) currently in flight, so a user-initiated Interrupt
	// can cancel all of it at once. Zero value is ready to use; guarded by its
	// own mutex (see interruptRegistry), so it needs no additional lock here.
	interrupts interruptRegistry

	// ChannelAttached, when true, makes the loop:
	//   - refetch the memory tail before each LLM call
	//   - treat IdleExit terminals as "phase=Idle" (call idleWithWakeRecheck)
	//     instead of failing
	//   - skip re-dispatch of tool_use IDs already in the delivered system_note set
	//   - emit an agent-side notification when picking up a new user message —
	//     through Notify, when that is set — so the channel sees "Got it,
	//     working…" without waiting for an LLM turn
	ChannelAttached bool

	// DelegatedChild, when true, makes this session's terminal tool COMPLETE it
	// (status.result + phase=Succeeded) instead of parking it Idle, even though
	// it is channel-attached. For such a child that tool is return_result —
	// coreCapability offers it IN PLACE OF agent_work_complete, so the door this
	// flag governs is never the one named in the loop's registered default.
	//
	// A conversational delegation (task/chat) binds its child to an `agent`
	// Channel joining it to its parent, so the child is channel-attached in
	// every mechanical sense: it drains an inbox, it resumes from a park, and
	// ask_parent yields exactly as await_user_message does. What it does NOT
	// have is a person who may say more. ChannelAttached's terminal arm parks
	// at Idle for precisely that reason — "the conversation persists;
	// subsequent inbounds wake the session" — and for a delegated child that
	// reasoning does not hold: the delegation IS the conversation, and it ends
	// when the child reports. reply_to_subagent refuses a request that is not
	// AwaitingParent, so no further message can arrive on this binding once
	// the child has said it is done.
	//
	// It is load-bearing rather than cosmetic. The SubagentRequest controller
	// resolves a delegation by reading the child's phase
	// (pkg/controllers/subagentrequest's reconcileChild), and only Succeeded
	// carries a result back. A child that parks Idle instead leaves the
	// request Running forever, so the parent's delegate / reply_to_subagent
	// call polls to its own timeout and the delegation can never finish. That
	// is the whole task/chat feature, and it was inert.
	//
	// Set by the runner from spec.parent plus the binding's own KIND (the
	// registry's AllowsSessionCounterparty — the same question the capability
	// layer asks before withholding respond_to_user from such a session), so a
	// second session-to-session transport inherits this with no edit here.
	DelegatedChild bool

	// AgentName is the human-readable identifier (typically the AgentClass
	// name) interpolated into channel-attached notifications such as
	// "<agent> thinking…". Empty falls back to "agent".
	AgentName string

	// Notify, when set, is called by the loop on every fresh-user-input
	// boundary (cold start + every refetched-tail-yields-new) to tell the
	// channel that the agent picked up the message. Errors are swallowed
	// (best-effort, channel UX only). Only meaningful when ChannelAttached.
	Notify func(ctx context.Context, text string)

	// OnToolDispatch, when set, is invoked just before every sandbox or MCP tool
	// Execute, so the channel-attached runner can publish a tool_activity envelope
	// and channelsd's silence watchdog auto-ticks even when the agent forgets to
	// update_status. Errors are swallowed (watchdog UX only). Meta tools are
	// excluded — in-process, and either trivially fast (new_operation) or already
	// a stronger watchdog signal (update_status). Only meaningful when
	// ChannelAttached.
	OnToolDispatch func(ctx context.Context, toolName string, kind tool.Kind)

	// OnToolProgress, when set, publishes a KindToolProgress snapshot for a
	// running SYNC sandbox tool. The Loop injects a per-dispatch hook into the
	// tool's context (stamping CallID + Name); the sandbox tool's sync poll
	// loop drives the ticks. nil → no emit. Only meaningful when ChannelAttached.
	OnToolProgress func(ctx context.Context, u sandbox.ToolProgressUpdate)

	// Progress, when set, is called with human-readable progress
	// messages: before each LLM call ("thinking…"), with the LLM's
	// emitted text blocks, and before each tool dispatch (every kind,
	// not just sandbox/MCP). Used by the local-mode gen-agent driver
	// to surface what's happening on stderr; channel-attached agents
	// use Notify for that. Errors swallowed (best-effort).
	Progress func(format string, args ...any)

	// OnStreamEvent, if set, is called for each provider stream event
	// during the LLM call. internal/cmd/runner main wires this for channel-
	// attached sessions to forward high-signal events to channelsd.
	// Nil = no live forwarding.
	OnStreamEvent func(llm.StreamEvent)

	// ProgressPublish, when set, emits a throttled live turn-progress
	// snapshot (cumulative tokens + elapsed + a monotonic seq) for the
	// channel to render on its status surface. Wired by internal/cmd/runner for
	// channel-attached sessions; nil disables the indicator entirely.
	// See progressReporter.
	ProgressPublish func(inputTokens, outputTokens int64, elapsedSeconds, seq int)

	// progress is the per-Run throttler built from ProgressPublish at the
	// top of Run. nil when ProgressPublish is unset; its methods no-op on
	// a nil receiver, so call sites need no guard.
	progress *progressReporter

	// Operations is the per-session audit registry — the same
	// tool.OperationRegistry threaded onto SessionContext.Operations. Kept as its
	// own Loop field rather than reached through SessionContext so the
	// operation-activity heartbeat can start before SessionContext is finalized
	// and read it from a goroutine without depending on that field's write
	// ordering. nil disables the heartbeat below regardless of
	// OperationActivityPublish.
	Operations tool.OperationRegistry

	// OperationActivityPublish, when set (together with Operations), emits a
	// throttled live snapshot of the operation subtree running past
	// operationActivityActivation — see buildOperationActivity — for the channel
	// to render as a live progress tree. seq is a heartbeat-local monotonic
	// counter (see runOperationActivityHeartbeat for why it is not
	// PackSeq(memTurnIndex, blockIndex)). nil disables the feature entirely.
	OperationActivityPublish func(pl channelevents.OperationActivityPayload, seq int)

	// usageMu guards usage and usageByModel — the cumulative token-usage snapshots
	// across all LLM calls in this run. Kept on the Loop rather than as loop-local
	// vars so fireSessionEnd can read them from any terminal call site. One mutex
	// covers both: addUsage and addModelUsage are always called together, so a
	// second lock would add contention without protecting anything independent.
	usageMu sync.Mutex
	usage   sessionUsage
	// usageByModel is the per-served-model running accumulation, in
	// first-encountered order. usageByModelIdx maps a display id to its
	// index in usageByModel so repeat turns under the same served model
	// accumulate into the existing bucket instead of appending a duplicate.
	usageByModel    []modelUsageBucket
	usageByModelIdx map[string]int

	// Engine is the runner's single authz dependency. Production wiring comes from
	// internal/cmd/runner/main.go; tests inject a fake or leave nil (falling back to the
	// AuthzCli path below).
	Engine engine.Engine

	// AuthzCli is the SpiceDB CheckPermission client for per-tool
	// permission checks. Set by internal/cmd/runner/main.go. nil is fail-closed:
	// a Readonly/Readwrite/External call returns OutcomeDenied from
	// toolcheck.Checker rather than running unchecked. The only sanctioned
	// path for running without an AuthzCli is ToolAuthMode "disabled"
	// (used by kubectl-driven dev runs).
	AuthzCli toolcheck.Client

	// AuthzCache is the per-session ZedToken cache. Created at runner
	// startup; survives only the runner's lifetime. nil bypasses the
	// at-least-as-fresh-as freshness floor (falls back to MinimizeLatency).
	AuthzCache *toolcheck.ZedTokenCache

	// authSubject and authSubjects are populated by ResolveAuthSubjects from the
	// AgentSession + AgentClass before dispatchToolUses fires. authSubject is the
	// singular ACTING principal (threaded through as pipeline.Input.Requester) and
	// is set in EVERY mode; authSubjects is the authorization subject-list, set
	// only when toolAuthSubject is both (see setBothSubjects for why that mode
	// needs both fields rather than just the list).
	//
	// Both are ADVANCED after session start by advanceRequester (from drainInbox)
	// in currentRequester/both mode: a runner pod parks in-process across multiple
	// users' turns (await_user_message), and each drained turn carries its own
	// requester's canonical in memory.Turn.Author. Freezing them at session start
	// would authorize every later tool call against the session's original
	// initiator regardless of who sent the prompting turn — a privilege-escalation
	// bug in multiplayer channels. See authSubjectMode / authSubjectStartedBy.
	authSubject  identity.CanonicalUserID
	authSubjects []string

	// authSubjectMode is the resolved AgentClass.spec.authz.toolCalls.subject
	// mode ("currentRequester", "startedBy", or "both"), stamped by
	// ResolveAuthSubjects. drainInbox reads it via advanceRequester to decide
	// whether a newly-drained turn's Author should move the current-requester
	// component of the auth subject(s) — "startedBy" mode never advances (the
	// frozen initiator is correct there by design).
	authSubjectMode string

	// authSubjectStartedBy holds the started-by component of the "both"-mode
	// subject list. It is frozen at session start (ResolveAuthSubjects) and
	// never advances; setBothSubjects recombines it with the (possibly
	// advanced) current-requester component to rebuild authSubjects.
	authSubjectStartedBy identity.CanonicalUserID

	// Approval, when set, wires per-tool approval pauses on Check
	// denial (or stateImpact=external). nil disables approval flow
	// (kubectl-driven sessions; tests without approval flow).
	Approval *approval.Orchestrator

	// InteractiveHooks, when set, are attached to each tool-dispatch
	// context so interactive sandbox tools can bridge their gateway
	// stream to the channel (publish output, accept human input). nil
	// for kubectl-driven sessions or when NATS is down — interactive
	// tools dispatched in that state error with a clear diagnostic.
	InteractiveHooks *sandbox.InteractiveHooks

	// GuardianGrantWriter, when set, lets the runner delete one-shot
	// (external) grants after successful dispatch. nil disables.
	GuardianGrantWriter grants.Writer

	// ColdStartRequestPublish publishes a cold-start metaagent_request to
	// authzd for the session's initial prompt. nil disables the runner-driven
	// cold-start flow (the initial prompt is appended verbatim as turn 0).
	ColdStartRequestPublish func(ctx context.Context, ns, name string, payload []byte) error

	// ArgsHashKey is the per-session HMAC key for grant arguments_hash
	// values (per-session Secret key "args-hash-key", read by internal/cmd/runner
	// at startup). Keying makes approval-grant bindings unforgeable
	// outside the controller+runner trust domain. nil in unit tests —
	// HMAC with a nil key is deterministic, so in-process flows that
	// both write and check with the same Loop still agree.
	ArgsHashKey []byte

	// StartedByCanonical is the canonical identity that created the
	// AgentSession. Empty for kubectl-driven sessions.
	StartedByCanonical identity.CanonicalUserID

	// ChannelKind / LastInboundExternalID feed the Requester field of the
	// ToolApprovalRequestPayload. Empty values flow through as empty strings
	// (channelsd's sender tolerates that for kubectl-driven approval prompts,
	// where there is no inbound channel user).
	//
	// LastInboundExternalID has no paired "last inbound email" field — fine for
	// tool_call approvals (DecideApprovers resolves standing via SpiceDB owner
	// sets, Email unused), but NOT safe to reach for as the identity_choice gate's
	// requester() ExternalID: DecideRequester needs a verified Email to
	// canonicalize (see identitygate.go). Add a genuinely paired inbound-email
	// source before wiring this field for that path.
	ChannelKind           string
	LastInboundExternalID string

	// OutboundChannelKind is the kind of the channel a human actually reads —
	// the session's OutputChannel when it has one, else ChannelKind. Use it
	// whenever addressing a PERSON (approvers, recipients); use ChannelKind
	// for describing where the inbound came from.
	//
	// The two differ for cron/split-channel sessions, whose input kind "bento" has
	// no user identity and no transport to a human: stamping an approver with it
	// makes the slack interaction sender skip delivery ("recipient has no Slack
	// identity"), leaving a tool_approval parked for its full timeout with nobody
	// ever asked. Empty falls back to ChannelKind.
	OutboundChannelKind string

	// ToolAuthMode mirrors AgentClass.spec.toolAuthMode. Three values:
	// "enforcing" (default; deny-and-prompt), "permissive" (denied
	// readonly/readwrite Checks log a "would deny" warning and proceed; external
	// still pauses for approval), "disabled" (NO enforcement; every call runs
	// without authz, including external). Empty defaults to "enforcing".
	ToolAuthMode string

	// AgentClass / AgentSession are the live CR pointers for entity binding.
	// AgentClass.Spec.Authz.Slots drives BindDefaults at session start; nil
	// disables the binding path (kubectl-driven sessions / tests without binding).
	AgentClass   *spiceboxv1alpha1.AgentClass
	AgentSession *spiceboxv1alpha1.AgentSession

	// SlotRevoker withdraws a slot grant. Used by the one-shot external-effect
	// cleanup, which revokes the grant its approval wrote as soon as the call it
	// authorized has run. Nil leaves the grant to its (short) expiry.
	SlotRevoker authz.RelWriter

	// SlotBinder writes the instance-scoped grants an approval produces,
	// alongside the session_scope entry recorded next to each one.
	//
	// The same *spicedb.Client relation view as SlotRevoker, named separately
	// because the intents differ and a field called "Revoker" doing the
	// granting is how a reader concludes the code does the opposite of what it
	// says. Nil leaves an approval recorded but unbound — the pre-existing
	// behaviour, and loud in the log rather than silent.
	SlotBinder authz.RelWriter

	// PlanGateMode is the RESOLVED plan-gate mode, read from
	// status.effectiveSettings.authz.planGate — deliberately NOT from
	// AgentClass.spec.authz like the other authz settings on this Loop. The
	// mode is ceiling-clamped in the settings fold, and reading the class spec
	// would let a class opt out past a cluster floor by being the only value
	// anyone consults. "" or "disabled" ⇒ the hook is not registered.
	PlanGateMode string

	// PlanGatePlan is the SEED plan: the one-phase session plan the runner
	// synthesizes from the live permission surface, whose ceiling equals the
	// whole surface. It governs only until the agent freezes phases of its own,
	// after which the gate reads the frozen plan instead.
	PlanGatePlan plangate.Plan

	// PlanGateSurface is the enumerated permission surface, used to annotate
	// external reach on approval cards.
	PlanGateSurface []permsurface.Descriptor

	// PlanGateSlotTypes are the resource types the AgentClass declares in
	// spec.authz.slots — the instance axis's analogue of the surface, and what a
	// phase's slot requests are validated against at freeze.
	//
	// Empty means the class does not use the instance axis, so every slot a
	// phase requests is undeclared and dropped. That direction is deliberate: a
	// slot request becomes a SpiceDB grant on approval, and a class that never
	// declared the type never opted into having grants written against it.
	PlanGateSlotTypes []string

	// PlanGateSlotPermissions maps each declared slot resource type to the
	// permission a grant on it confers (AuthzSlot.Permission). Standing on a
	// slot is a question about that PAIR — holding `read` on a repo says
	// nothing about a slot whose grant confers `write`.
	PlanGateSlotPermissions map[string]string

	// PlanGateSlotPermissionSets is every permission each slot type may confer —
	// the ceiling a binding is filtered through. A permission outside it has no
	// slot_grant relation in the composed schema, and writing it fails the whole
	// grant write, so an approval that named it would buy nothing at all.
	PlanGateSlotPermissionSets map[string][]string

	// PlanGateSlotTransforms maps each declared slot resource type to the
	// transform chain a free-form value passes through to become the object id,
	// as published on AgentClass.status.resolvedSlots[].valueTransforms.
	//
	// Separate from PlanGateSlotPermissions because the two come from different
	// places: the permission is declared on the class spec, the chain is DERIVED
	// by the controller from the tools that key the type. A slot absent from
	// this map has no declared chain, which means its ids are already distinct
	// resources and are used as-is.
	PlanGateSlotTransforms map[string][]string

	// PlanGateSlotStanding maps each declared slot resource type to how an
	// approval on it gets its authority, from
	// AgentClass.status.resolvedSlots[].standing: StandingRequired asks SpiceDB
	// whether the approver personally holds the slot's permission on the named
	// instance before delegating it (today's behaviour); anything else —
	// including the type being ABSENT from this map — is session-only, and the
	// approver's decision is the whole authority, with no lookup at all.
	//
	// Absent must never mean StandingRequired: AgentClass.status may not have
	// been published yet when a session starts, and treating that window as
	// strict would make every slot unbindable until the controller catches up.
	// Session-only does not weaken enforcement downstream of the approval — the
	// grant it produces is still written, still carries a mandatory expiry, is
	// still scoped to the session, is still revocable, and the tool-call Check
	// still resolves slot_grant_<perm>->interact + owner. The only thing it
	// drops is asking "did the approver already hold it" first, which is
	// unanswerable for a type SpiceDB has no tuples for in the first place — a
	// git repository's permissions live at the forge, not in SpiceDB.
	PlanGateSlotStanding map[string]string

	// ResourceStandings maps each RESOURCE TYPE to how approval for it is
	// governed (AgentClass.status.resolvedResourceStandings, read via
	// ResourceStandingsOf). Distinct from PlanGateSlotStanding, which is keyed by
	// declared SLOT and feeds the plan-gate binding path: a tool check can name a
	// type that is not a declared slot, and the approval router needs an answer
	// for that type too.
	//
	// A type absent from this map has no published answer and the router refuses.
	ResourceStandings map[string]ResourceStanding

	// PlanGatePermissionTitles maps each declared permission's
	// "<resourceType>/<permission>" key to the human phrase the AgentClass
	// controller published, from AgentClass.status.resolvedPermissionTitles
	// (see PermissionTitlesOf). Display only: a pair absent from this map has
	// no declared title, and the card falls back to a detokenized handle
	// rather than guessing English.
	PlanGatePermissionTitles map[string]string

	// PlanGateResourceDisplays maps each declared resource type to the
	// icon/label/name presentation the AgentClass controller published, from
	// AgentClass.status.resolvedResourceDisplays (see ResourceDisplaysOf).
	// Display only: a type absent from this map has no declared display, and
	// its resource lines fall back to the wire type name with no icon and no
	// link — exactly the pre-existing behavior.
	PlanGateResourceDisplays map[string]plangate.ResourceDisplay

	// SpiceDBHasOnResource reports whether a subject holds `permission` on the
	// ONE resource named — the plan gate's approver-standing question for a slot
	// whose phase named its instance.
	//
	// This is the question that has to be asked before an approval writes a
	// grant, because the grant is written against that exact instance: an
	// approver holding contact_access on company 4210 and nothing else must not
	// clear a phase that named 4299, or a click on a card they were entitled to
	// see hands the session live access to somebody else's record.
	//
	// nil, with SpiceDBHasAnyOfType wired, means the gate can ask only the
	// type-level question — which is the escalation above — so a slot whose
	// phase named an instance is held back instead. Same direction as a slot
	// type the class declares no permission for: the pre-slice outcome (no
	// grant) rather than an unchecked one.
	//
	// BOTH hooks nil is the no-lookup wiring (unit fixtures, gen-agent): there
	// is no standing question to ask at all, so the requested slots are recorded
	// as asked. Safe only because the same binaries that leave these nil leave
	// SlotBinder nil too — cmd/runner and the e2e factory wire all three
	// together — so nothing is granted off an unasked question.
	SpiceDBHasOnResource func(ctx context.Context, resourceType, resourceID, permission string, canonicalID identity.CanonicalUserID) (bool, error)

	// SpiceDBHasAnyOfType reports whether a subject holds `permission` on at
	// least one resource of `resourceType`.
	//
	// The DEFERRED half of the same question, and only that half. A phase may
	// request a slot by TYPE alone, leaving the instance to a later fill source;
	// there is then no instance for a Check to name, and standing on the type is
	// all that can be asked. Nothing instance-scoped is granted off this answer —
	// a slot with no id has nothing to write a grant against — so "at least one"
	// bounds which requests the approver is told they can speak for, and never
	// which instance the session reaches.
	//
	// nil, with SpiceDBHasOnResource wired, holds deferred slots back for the
	// same reason SpiceDBHasOnResource nil holds named ones back. Both nil is the
	// no-lookup wiring described there.
	SpiceDBHasAnyOfType func(ctx context.Context, resourceType, permission string, canonicalID identity.CanonicalUserID) (bool, error)

	// PlanGateMaxCardHandles folds a long ceiling on a Routine card, from the
	// resolved PlanRendering limits. Elevated and Severe cards ignore it —
	// severity expands a card, never shortens it.
	PlanGateMaxCardHandles int

	// PlanGateMaxAutoApprove bounds the union of handles auto-approved without
	// a human across the session (the tier-0 budget).
	PlanGateMaxAutoApprove int

	// PlanGateRequirePlan denies permissioned calls until the agent declares a
	// plan, closing the "never call update_plan" bypass. Resolved, like the mode.
	PlanGateRequirePlan bool

	// planGateFrozen is the frozen plan currently in force, set when the agent
	// declares phases. Guarded because select_phase and the gate read it from
	// the tool-dispatch goroutines while update_plan writes it.
	planGateMu      sync.Mutex
	planGateFrozen  plangate.Plan
	planGateHasPlan bool

	// Client and Artifacts back applyUIResource's widget persistence: Client
	// creates + polls the ArtifactRender{Kind:"mcpui"} CR to Ready/Failed, and
	// Artifacts mints the artifact head ID and finalizes the revision once Ready
	// (the same CR-create+poll+finalize dance as
	// pkg/agent/tool/meta/artifact_prepare.go). Same values
	// capability.RunnerEnv.Client / .Artifacts are wired from. Either (or both)
	// may be nil: applyUIResource then logs and skips durable persistence for a
	// tool result carrying a UIResource, but still publishes the session_view_offer
	// anchor.
	Client    k8sclient.Client
	Artifacts *artifacts.Service

	// ArtifactReader fetches stored attachment bytes for the hydration pass
	// (hydrateAttachments, attachments.go). May be nil (kubectl-driven sessions
	// with no artifact store); a nil reader degrades every attachment to reference
	// form rather than panicking. Declared as the interface, never as a concrete
	// pointer — a typed-nil pointer here would be a non-nil interface that panics
	// on first call. Same value as capability.RunnerEnv.ArtifactReader, not a
	// second client construction.
	ArtifactReader modality.ArtifactReader

	// ApprovalSummarizer is the isolated "What this tool call will do"
	// LLM (see pkg/agent/runner/approval/summarizer + the package
	// docstring there). nil disables the call entirely → channelsd
	// renders the deterministic fallback for the "What" line.
	// Approval availability MUST NOT couple to summarizer
	// availability; every failure path here returns "" instead.
	ApprovalSummarizer summarizer.Provider
	// ApprovalSummarizerTimeout overrides summarizer.DefaultTimeout
	// per-Loop. 0 → use the package default (5s). Tests use a smaller
	// value to keep the suite fast.
	ApprovalSummarizerTimeout time.Duration

	// IdentityRecommender is the isolated advisory LLM for identityMode=dynamic.
	// Its presence is what distinguishes a dynamic choice from a plain ask:
	// non-nil ⇒ the gate labels the request "dynamic" and surfaces the
	// recommendation; nil ⇒ dynamic degrades to a plain ask (no recommendation).
	// Advisory only — the human always confirms. Wired by internal/cmd/runner only for
	// identityMode=dynamic sessions.
	IdentityRecommender identityadvisor.Provider
	// IdentityChoicePublish publishes the gate's interaction_request envelope
	// (category identity_choice) to channelsd. nil ⇒ ask/dynamic cannot run
	// interactively (the gate fails the session closed rather than silently
	// running as the agent identity).
	IdentityChoicePublish func(ctx context.Context, ns, name string, env channelevents.Envelope) error
	// IdentityChoiceTimeout bounds the human-choice wait. 0 ⇒
	// DefaultIdentityChoiceTimeout.
	IdentityChoiceTimeout time.Duration
	// IdentityGatePending gates registration of the SessionStart
	// IdentityChoiceGate. internal/cmd/runner sets it true ONLY for a first-boot
	// ask|dynamic session (status.effectiveIdentityMode not yet stamped), where
	// the runner boots on a PROVISIONAL agent identity and the gate must ask the
	// initiating user to confirm (agent) or hand off (userPassthrough). False for
	// static modes and for re-spawns past the choice — the gate is then not
	// registered and SessionStart proceeds unchanged.
	IdentityGatePending bool
	// IdentityHandoffExited is set by Run when the IdentityChoiceGate resolved to
	// userPassthrough: Run exits NON-terminally (nil, no terminal status written)
	// so the operator re-drives the passthrough credential-link flow and re-spawns
	// the runner. internal/cmd/runner reads it to log the handoff; the phase is already
	// Pending, folded from the signed IdentityChoiceResolved event.
	IdentityHandoffExited bool

	// LabelStore caches (resourceType, id) → friendly label tuples
	// extracted from MCP tool responses (see pkg/agent/tool/mcp's
	// post-effect chain). The Loop seeds this on every MCPTool via
	// SetLabelSink; the approval-emit path snapshots it into the
	// outbound envelope. Nil-safe: a nil store skips substitution.
	LabelStore *LabelStore

	// lastStatusMu guards lastStatusText. update_status calls happen
	// per assistant turn; the captured text is the most recent across
	// ALL turns so the cross-turn justification fallback in
	// HarvestJustification can find it even when the gated tool_use
	// arrives in a later turn than the announcement.
	lastStatusMu   sync.RWMutex
	lastStatusText string

	// disabledWarningOnce ensures the runner publishes the
	// "tool authz is disabled" KindNotification at most once per
	// session (on the first dispatched tool, before its Execute).
	disabledWarningOnce sync.Once

	// Engine-driven autofill is gated by the AP_BINDING_AUTOFILL env var. When
	// false (the default), the runner skips eng.FillToolArgs +
	// eng.WaitForExtraction entirely; tool dispatch is unchanged.
	BindingAutofillEnabled  bool
	BindingAutofillDeadline time.Duration

	// CurrentInboxIdx is the inbox-turn index of the most recent user
	// message. Populated by drainInbox; used by the autofill site to wait
	// for that turn's extraction. NOT reset on assistant-turn append (the
	// tool dispatch needs the latest user-message index, not zero).
	CurrentInboxIdx int

	// currentUserTurnIndex holds (transcript Index + 1) of the most recently
	// drained user turn, so the zero value of a bare Loop — or a fresh Run
	// that has not yet drained any user turn — reads back as "unknown" (-1)
	// via CurrentUserTurnIndex, never a false turn 0. Set beside every
	// advanceRequester call (drainInbox) and, at Run start, from the highest
	// user-turn index already in the transcript (see loop_subjects.go's
	// setCurrentUserTurnIndex / CurrentUserTurnIndex / highestUserTurnIndex).
	// preferencesReader captures the accessor so a preference read scopes to
	// what the CURRENT turn's author has actually seen.
	currentUserTurnIndex atomic.Int64

	// DisabledNotify, when set, is called the first time a tool
	// dispatch fires under disabled mode. Implementations publish a
	// KindNotification envelope to channelsd surfacing the warning to
	// the user. nil = no-op (kubectl-driven sessions; tests). Wired
	// by internal/cmd/runner/main.go using the same NATS plumbing as Notify.
	DisabledNotify func(ctx context.Context)

	// Info-leakage gate wiring. All optional; the gate is fully disabled when
	// LeakageConfig is nil OR resolves to Mode=disabled.

	// LeakageConfig is the resolved AgentClass.Authz.InformationLeakage policy.
	LeakageConfig *spiceboxv1alpha1.InformationLeakagePolicy

	// LookupToolMapping returns the MCPServer ToolResourceMapping for `toolName`,
	// or nil if unmapped.
	LookupToolMapping func(toolName string) *spiceboxv1alpha1.ToolResourceMapping

	// ResolveBoundSlots returns this session's bound data slots with the data
	// behind them, for placement in the opening context. Built by
	// ResolveBoundSlotsFor.
	//
	// Nil for every session that is not a delegated child, which is nearly all
	// of them: a session nobody bound a slot onto has nothing to place.
	ResolveBoundSlots func(ctx context.Context) ([]SlotDatum, error)

	// deliveredSlotTags are the tags deliverNewBoundSlots has already placed in
	// THIS process's run, by tag id.
	//
	// The durable dedup is the transcript itself (tagAlreadyDelivered), which
	// is what survives a restart. This covers the gap that read cannot: the
	// turns list is snapshotted at Run start, and both resume sites may deliver
	// after it. Written and read only from Run's own goroutine.
	deliveredSlotTags map[string]bool

	// LookupToolUntrustedSource reports whether `toolName`'s result may carry
	// untrusted public content, from the tool's own SEP-1913
	// `returnMetadata.source` declaration. It supplies the INTEGRITY axis of
	// every tag PtTagMint writes — the trifecta's leg A.
	//
	// Nil leaves every tag trusted, which is the honest answer where nothing
	// declares a source, and is what the gate did before this existed.
	LookupToolUntrustedSource func(toolName string) bool

	// TaintMemoryAppend records a taint record. Nil disables taint capture.
	TaintMemoryAppend func(ctx context.Context, rec infoleakagetaint.TaintRecord) error

	// TrifectaMode is the class's own trifecta mode (disabled|logging|
	// enforcing). Empty or "disabled" leaves the hook unbuilt.
	//
	// Deliberately NOT reached through toolCalls.mode: a control that
	// disappears when an operator disables a different permission check is not
	// a control. Turning off tool-call authz is not a decision that untrusted
	// content may drive a write.
	TrifectaMode string

	// TrifectaBoundTags returns the pt-tags bound into THIS session — the data
	// a parent handed it. They are the inputs legs A and B are derived from,
	// which is why this is a live lookup rather than a snapshot: a tag can be
	// bound mid-session, and a leg derived from a stale list would judge a
	// delegation on data it no longer describes.
	TrifectaBoundTags func(ctx context.Context) ([]string, error)

	// TrifectaDeps are the three SpiceDB lookups the derivation needs. Kept as
	// the trifecta package's own struct so this Loop holds no opinion about how
	// a reader set is resolved.
	TrifectaDeps trifecta.Deps

	// SessionStatusReader reads this session's own closureDenied flag — the
	// §2.8 structural precondition, stamped by the operator onto every member
	// of a delegation closure when one of them is denied.
	//
	// A function rather than a cached bool because the answer CHANGES mid-run:
	// a denial can land in a sibling branch while this session is between tool
	// calls, and a gate consulting a start-time snapshot would admit exactly
	// the call the denial exists to stop.
	SessionStatusReader func(ctx context.Context) (bool, error)

	// PtTagMint records the PER-DATUM provenance tag for a read, beside the
	// session-wide taint record above. Nil disables per-datum provenance.
	//
	// It calls the operator's mint route rather than writing the record: pt_tag
	// is component-written precisely so a session cannot author its own reader
	// set, and this runner holds a session credential. The request says which
	// RESOURCES the call touched; the operator derives who may read the result.
	PtTagMint func(ctx context.Context, req memory.PtTagMintRequest) (string, error)

	// PtTagVerify content-binds an outbound payload's parsed pt-untrusted regions
	// against the bytes the platform stored for their claimed ids, returning
	// which ids bound. Nil disables per-datum coverage (verifyPayloadTags then
	// falls to the coarse floor).
	//
	// It calls the operator's verify route rather than reading pt_tag_content
	// directly: that kind is component-READ (SessionReadable false), so this
	// session-credentialed runner cannot read it over the memory API — and even
	// if it could, doing the comparison here would let a compromised runner
	// declare a fabricated region a match. The runner PARSES the regions (a
	// parse, not a privilege); the operator, holding the bytes, does the bind.
	PtTagVerify func(ctx context.Context, regions []memory.PtTagRegion) (memory.PtTagVerifyResponse, error)

	// TaintMemoryList reads all taint records for the current session. Used by
	// the write-side gate at outbound channel emission to compute the audience
	// permitted-set.
	TaintMemoryList func(ctx context.Context) ([]infoleakagetaint.TaintRecord, error)

	// AuditMemoryAppend records an audit event. Nil disables audit.
	AuditMemoryAppend func(ctx context.Context, rec infoleakageaudit.AuditRecord) error

	// RequesterCanonicalID renders the PER-CALL principal as the SpiceDB subject
	// reference (the "user:"-prefixed form) the info-leakage gate checks and
	// addresses its notice to. perCall is pipeline.Input.Requester — the widget
	// VIEWER on a proxy-exec (app-tool) call, the loop's session subject on the LLM
	// path. It takes the principal rather than reading l.authSubject because a
	// proxy-exec runs on the NATS-handler goroutine or a detached approval
	// goroutine, concurrently with advanceRequester's writes: resolving from loop
	// state is both a data race and a wrong-principal check (the widget viewer's
	// read authorized against whoever last messaged the session).
	//
	// nil disables the requester view check (the read is audited read_unchecked)
	// and suppresses the leakage notice.
	RequesterCanonicalID func(ctx context.Context, perCall identity.CanonicalUserID) (identity.Subject, error)

	// SpiceDBCheck wraps a CheckPermission call. Returns true when permitted.
	// For the read-side gate, called as
	//   SpiceDBCheck(ctx, requester, permission, resource).
	// Implementations should fail closed (return false + error) on transient
	// SpiceDB errors.
	SpiceDBCheck func(ctx context.Context, subject, permission, resource string) (bool, error)

	// ChannelKindImpl is the AudienceResolver for the bound channel kind, if
	// the kind implements it. Used by the write-side info-leakage gate to
	// resolve the channel audience. Nil when the session is not
	// channel-attached OR the channel kind does not implement AudienceResolver.
	// Distinct from ChannelKind (a string) which carries the kind name only.
	ChannelKindImpl channelkinds.AudienceResolver

	// FetchSpeakerProfile resolves the current speaker's profile by email,
	// bound to the session's channel kind + credentials by
	// userprofilegate.Offer. Nil means the user_profile capability was not
	// granted (or the kind cannot serve profiles), and NOTHING about profiles
	// happens: no block, no API call. That the Loop holds a nil func rather
	// than a flag is deliberate — an inert capability has nothing to call.
	FetchSpeakerProfile userprofilegate.FetchFunc

	// SpeakerProfileFields is the operator's allowlist, already validated and
	// defaulted by the capability's ParseConfig. Meaningless when
	// FetchSpeakerProfile is nil.
	SpeakerProfileFields []userprofile.Field

	// lastInboundAuthor is the Author subject of the most recently drained inbound
	// turn — "who is speaking right now". Deliberately NOT authSubject: that field
	// is written only under AuthzCli != nil AND authSubjectMode
	// currentRequester/both, so under startedBy mode (or with authz disabled) it
	// is frozen at the initiator or empty. Attribution for display must not
	// inherit an authorization mode's gating.
	lastInboundAuthor identity.Subject

	// speakerProfileMu guards the four speakerProfile* fields below together —
	// one cohesive piece of per-turn memoization state, read and written as a unit
	// by hydrateSpeakerProfile/decideSpeakerProfile. Guarded (like
	// attachmentNoncesMu) rather than left to the Run goroutine's sequencing, so a
	// future off-loop caller does not rediscover it as a race. Every critical
	// section is a local map/bool operation only — never spanning the
	// FetchSpeakerProfile call, which is arbitrary (potentially network) I/O; see
	// decideSpeakerProfile's claim/fetch/commit shape.
	speakerProfileMu sync.Mutex

	// speakerProfileBlocks remembers the exact rendered block text emitted at each
	// message-slice index, keyed by that index. hydrateSpeakerProfile re-applies
	// every entry byte-for-byte on EVERY request — the block is never written back
	// into the persistent conversation (same reason hydrateAttachments never
	// writes native blocks back). A message index, once given an entry, never
	// changes meaning: the conversation only grows by append. Lost on restart,
	// which just costs old turns their blocks and resets the cache once.
	speakerProfileBlocks map[int]string

	// speakerProfileAuthor is the speaker for whom speakerProfileBlocks most
	// recently gained an entry. A later turn from the SAME speaker is not
	// re-fetched or re-rendered: only an actual speaker CHANGE does.
	speakerProfileAuthor identity.Subject

	// speakerProfileDecided / speakerProfileDecidedIdx memoize the one decision
	// already made for the current head user-turn index, across the many provider
	// round-trips a single human turn can involve (a multi-step tool-calling loop
	// re-enters hydrateSpeakerProfile once per round-trip, all resolving to the
	// same head index). Tracked separately from speakerProfileBlocks because a
	// MISS (ungranted profile, transport error, …) must also not be retried within
	// the turn; decidedIdx's zero value cannot be mistaken for "index 0 already
	// decided" because decided starts false.
	speakerProfileDecided    bool
	speakerProfileDecidedIdx int

	// SpiceDBLookupSubjects returns canonical user subjects that have
	// `permission` on `resource` (form "<resourceType>:<resourceID>").
	// Used by the write-side leakage gate to compute the per-resource
	// permitted-set.
	SpiceDBLookupSubjects func(ctx context.Context, resource, permission string) ([]string, error)

	// LeakageGrantWriter writes SpiceDB infoleakage_grant tuples after
	// approval. Signature matches approval.WriteInfoLeakageGrants. nil
	// causes the gate to refuse to un-gate after approval (fail-closed).
	LeakageGrantWriter func(ctx context.Context, sessionNs, sessionName string, audience []string, resources []approval.LeakageGrantResource, ttl time.Duration) (string, error)

	// InteractionRequestPublish publishes a generic interaction_request envelope
	// (the unified Interaction model) on the IN subject. Used by migrated
	// runner-host approval categories (content_inspection in C1); channelsd's
	// HandleInteractionRequest parks it + re-emits on OUT for the generic
	// renderer. nil ⇒ the gate fails closed (there is no channel to ask on).
	InteractionRequestPublish func(ctx context.Context, ns, name string, env channelevents.Envelope) error

	// TimeoutAppliedPublish publishes an approval "applied" envelope on BOTH the
	// IN subject (so channelsd's Handle*Applied handler clears its pending queue +
	// condition) and the OUT subject (so the channel sender edits the pending
	// message to show the request expired). Called when a per-kind approval
	// deadline elapses with no decision, for tool_call / leakage_share /
	// content_inspection; the envelope always carries a deny/timeout outcome — a
	// lapsed deadline never synthesizes an approval. nil disables (kubectl/test
	// sessions have no channel surface to clear and rely on the orchestrator
	// ctx-deadline alone).
	TimeoutAppliedPublish func(ctx context.Context, ns, name string, env channelevents.Envelope) error

	// UIPublish publishes one agent-UI push envelope on the session's out subject,
	// where webd's agent-UI live route relays it to attached browsers. It carries
	// TWO push kinds — ui_action_update (uiActionRecorder) and ui_view_update
	// (uiview.Runtime.Write, via runner.AttachUIView) — so do not rename it for
	// either one alone. Nil-safe: a kubectl-driven or test loop leaves it nil and
	// each lifecycle degrades to memory-only, which a reconnecting browser still
	// reads correctly.
	UIPublish func(ctx context.Context, ns, name string, env channelevents.Envelope) error

	// pipelineOnce ensures buildPipeline runs exactly once per Loop.
	pipelineOnce sync.Once
	// pipelineExec is the per-Loop pipeline executor (lazily constructed).
	// Typed as the pipelineRunner seam so tests can inject a runner whose
	// Run returns a non-nil host-primitive error (the fail-closed path).
	pipelineExec pipelineRunner
	// pipelineReg is the registry pipelineExec runs, kept so the read-side
	// PostToolCall pass over ungated meta tools (leakageread_meta.go) can reuse
	// the SAME hook instances rather than building a second set. nil when a test
	// injected pipelineExec directly.
	pipelineReg *pipeline.Registry

	// readSideOnce / readSidePostExec are the read-side PostToolCall pass over
	// ungated meta tools — see leakageread_meta.go. Separate from pipelineExec
	// because it runs a STRICT SUBSET of the hooks: no Pre leg, no plan gate,
	// no scope narrowing.
	readSideOnce     sync.Once
	readSidePostExec pipelineRunner
	// infoLeakAudienceHook is the per-Loop InfoLeakAudience hook instance, captured
	// at registry-build time so the leakage approval callbacks
	// (RecordApproved/RecordDenied) can update the hook's session-scoped
	// approved/denied sets once the host resolves an approval. Without it the
	// PreResponse gate would re-prompt for a resource the PostToolCall gate
	// already resolved this session.
	infoLeakAudienceHook *hooks.InfoLeakAudience

	// hookActiveOnce guards the one-time computation of hookActiveMap from the
	// shared hooks.ActiveHooks table.
	hookActiveOnce sync.Once
	// hookActiveMap is the per-Loop cached hook-name -> ActiveYes projection,
	// populated by hookActive (pipeline_wiring.go) on first use.
	hookActiveMap map[string]bool

	// SecretOutPublisher, when non-nil, is called after a successful local
	// store.Put to push the captured secret value to the operator so it lands
	// in the per-session Secret. nil is safe (the value remains only in the
	// in-process SessionStore and no status entry is written — used by tests
	// and local-mode runners that have no operator endpoint).
	SecretOutPublisher secretout.Publisher

	// ToolRefresher, when non-nil, is consulted at the top of each turn (before the
	// LLM request is built) and returns any tools that became available — or were
	// re-synthesized — SINCE the last call: typically secret-gated separate-pod
	// SidecarToolbox tools whose pod went Ready mid-session (a producer tool
	// emitted the gating secret), or whose pod was REPLACED (a token rotation made
	// the operator delete+recreate it with a new value and IP). Returned tools are
	// merged into l.Tools with REPLACE-by-name semantics (see applyToolRefresh)
	// and the LLM tool-def list is rebuilt so the NEXT turn calls the fresh tools,
	// without restarting the runner. The refresher owns its own dedup (each
	// newly-ready/replaced tool returned exactly once per change); the loop trusts
	// the returned slice. An error is logged and the turn proceeds with the
	// existing tool set — a transient probe failure must not stall the live
	// conversation, and a replaced sidecar keeps serving its prior tools until the
	// new pod answers — with the refresher consulted again next turn. A pass also
	// reports sidecars that will NEVER become available
	// (ToolRefreshResult.Advisories). nil disables the mid-session path entirely
	// (boot synthesis in internal/cmd/runner still applies).
	ToolRefresher func(ctx context.Context) (ToolRefreshResult, error)

	// PinDrift, when non-nil, is consulted at PreToolCall time to escalate
	// calls on tools whose backing MCP dependency drifted from its baseline
	// under the effective "approve" pinning mode. Populated by internal/cmd/runner
	// before the MCP boot loop and passed here; nil disables the per-call
	// escalation path (the drift is still noted in the description prefix and
	// the runner notes).
	PinDrift *PinDriftState

	// Provenance, when non-nil, maps LLM tool name → ProvenanceRecord for
	// every synthesized tool. Built at session start alongside PinDrift and
	// consumed by recordAuthzDecision to stamp pin-origin facts onto each
	// authz_decision audit entry. Nil-safe: recordAuthzDecision skips pin
	// field population when Provenance is nil or the tool has no entry.
	Provenance *ToolProvenance

	// ToolGuardPolicy is the session-start-resolved toolguard policy. Set by
	// internal/cmd/runner (never nil there — the builtin rule applies even with no
	// config); nil in tests/gen-agent disables the guard hooks entirely.
	ToolGuardPolicy *toolguard.ResolvedPolicy

	// AuthFailures records, per tool origin, that the platform ITSELF observed an
	// authentication-shaped failure — the corroboration a CredentialUpdateRequest
	// needs when the provider cannot be re-probed. Set by internal/cmd/runner and the e2e
	// in-process factory alike; nil (tests, gen-agent) records nothing. Every
	// method on it is nil-safe, so the hook below needs no guard of its own.
	AuthFailures *authfail.Recorder

	// ContentInspectors are the session-start-resolved content-guard instances
	// (built from status.effectiveSettings.contentInspectors). Empty ⇒ none.
	ContentInspectors []contentguard.Instance
	// ContentInspectorIDs is the parallel ID slice for ContentInspectors.
	// Instance carries no ID; the runner tracks them here so audit records
	// and hook names use the registry key, not a generated fallback.
	ContentInspectorIDs []string
	// toolGuardReg is the session-scoped breaker/rate state, built lazily
	// with the pipeline registry.
	toolGuardReg *toolguard.Registry

	// RevokedOrigins is the session-scoped live set of revoked tool origins.
	// It is consulted by the revocation_guard PreToolCall hook (reads via
	// IsRevoked) and written by the oap.revocation NATS subscriber's registered
	// invalidator (Invalidate) — the SAME instance threaded to both by
	// internal/cmd/runner. nil disables in-flight revocation (tests/gen-agent).
	RevokedOrigins *toolorigin.Set

	// RevocationRegistry maps a revocable-kind name to its Invalidator. It is the
	// SAME registry the oap.revocation subscriber dispatches through, and
	// claimAndRecover uses it to re-apply every revocation replayed from the
	// signed lifecycle log on a runner restart.
	//
	// It is separate from RevokedOrigins because the two are different
	// capabilities on different paths: the guard hook READS the concrete
	// *toolorigin.Set (IsRevoked), while restart recovery WRITES through the
	// kind-dispatched Invalidator interface. Going through the registry is what
	// lets a new revocable kind be re-applied on restart without editing the
	// sequencer. nil disables restart re-application.
	RevocationRegistry *revocation.Registry
}
