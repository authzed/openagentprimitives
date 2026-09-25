// Package tool defines the unified Tool interface seen by the agent loop.
// Three Kinds exist, all of them live:
//
//   - KindMeta    — runs in-process in the runner (e.g. agent_work_complete);
//     see pkg/agent/tool/meta.
//   - KindSandbox — synthesized from a SpiceboxToolspec; Execute creates a
//     ToolCall CR the operator dispatches into the sandbox. See
//     pkg/agent/tool/sandbox.
//   - KindMCP     — synthesized from an MCPServer CR; Execute issues JSON-RPC
//     tools/call to the upstream server. See pkg/agent/tool/mcp.
package tool

import (
	"context"
	"encoding/json"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/memory"

	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

type Kind string

const (
	KindMeta    Kind = "meta"
	KindSandbox Kind = "sandbox"
	KindMCP     Kind = "mcp"
)

// Tool is what the LLM-facing tool list contains.
type Tool interface {
	Name() string
	Kind() Kind
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx context.Context, args json.RawMessage, sess *SessionContext) (Result, error)
	// Permission declares the per-tool authz policy; dispatchToolUses runs
	// authz.Check against it before routing to the kind dispatcher. Tools that
	// legitimately need no Check return StateImpact Stateless or Passthrough.
	Permission() authz.Permission
	// PermissionVariants returns conditional permission variants the dispatcher
	// evaluates against tool-call args before falling back to Permission(); nil
	// for tools with no variants (every kind but MCP today).
	PermissionVariants() []authz.PermissionVariant
}

// LookupByName indexes tools by their LLM-facing Name() and returns a lookup
// closure over that index. internal/cmd/runner and the e2e in-process factory share it
// to build the late-bound capability.RunnerEnv.ToolLookup closure, so the two
// wiring sites cannot drift on how "the tool list" becomes a lookup.
//
// Duplicate names resolve FIRST-WINS, matching the runner's own live lookup
// (Loop.lookupTool, a linear scan returning on first match). The two are
// consulted for different decisions about the SAME call — this one for the
// failing tool's Origin() in request_credential_update, the runner's for
// permission / toolguard / MCP-spec routing — so disagreeing on which concrete
// tool a name denotes is how a credential card ends up naming an upstream that
// never failed.
func LookupByName(tools []Tool) func(name string) (Tool, bool) {
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		if _, dup := byName[t.Name()]; dup {
			continue // first wins, as in Loop.lookupTool's linear scan
		}
		byName[t.Name()] = t
	}
	return func(name string) (Tool, bool) {
		t, ok := byName[name]
		return t, ok
	}
}

// OriginTool is implemented by a Tool belonging to a shared upstream (an MCP
// server, a sidecar toolbox) whose health is shared across sibling tools.
// Origin returns "<kind>/<name>", e.g. "mcpserver/github". Sandbox and meta
// tools don't implement it. Consumed by pkg/authz/toolguard for origin-level
// circuit breakers.
type OriginTool interface {
	Origin() string
}

// Result is what Execute returns to the runner.
type Result struct {
	// Content is the tool_result block fed back to the LLM.
	Content string
	// IsError marks the call as failed; the LLM still sees Content.
	IsError bool
	// Terminal ends the session. Only meta tools may set it.
	Terminal bool
	// HTTPStatus is the upstream HTTP status observed for a FAILED MCP-origin
	// call; zero means none was observed (success, a non-MCP kind, or a
	// transport failure that never got a response). Zero is unambiguous — a real
	// status is always 100-599 (RFC 7231) — which is the invariant
	// pkg/platform/identity/credupdate.Observation.HTTPStatus relies on to tell
	// the two apart. Set by pkg/agent/tool/mcp via errors.As on *probe.HTTPError.
	// Out-of-band only: never echo it into Content, which stays exactly what the
	// LLM already sees.
	HTTPStatus int
	// ExitCode is the process exit code observed for a CLI/toolkit-origin call;
	// nil means none was observed (an MCP call, or a sandbox call that never
	// reached a terminal ToolCall). A POINTER because a non-nil zero is a real
	// observation ("exited 0") that must stay distinguishable from "not
	// observed" — collapsing them onto a bare zero would let a provider
	// declaring `exitCodes: [0]` corroborate every unobserved call in the
	// session. Set by pkg/agent/tool/sandbox (composeResult for sync ToolCalls,
	// composeStreamResult for the bridge); like HTTPStatus, never echo it into
	// Content.
	ExitCode *int32
	// Stderr is the captured stderr of a FAILED CLI/toolkit-origin call, empty
	// otherwise; it is what a provider's authFailure.stderrPatterns match
	// against. Carried only on the failure path, which already inlines the same
	// bytes into Content, so it adds no exposure the LLM lacks — a SUCCEEDED
	// call's stderr is deliberately not carried. Bounded at the source
	// (maxInlineStderr / maxStderrTailBytes).
	Stderr string
	// OriginAuthenticated reports that the origin's own auth layer ACCEPTED this
	// call's credential and ran the tool — positive evidence the credential
	// works, carried even when IsError is true. False means "not observed",
	// never "authentication failed". Set by pkg/agent/tool/mcp on any completed
	// JSON-RPC exchange and by pkg/agent/tool/sandbox on a Succeeded ToolCall.
	//
	// It exists because IsError alone cannot separate a transport failure
	// (evidence about nothing) from a call the provider authenticated and then
	// answered with a tool-level error (proof the credential works). Without it
	// the second reaches the corroboration recorder as a failure, which retracts
	// nothing — and since the agent CHOOSES the arguments, a prompt-injected
	// agent could manufacture tool-level errors on demand to keep a dead
	// auth-failure observation alive until a human re-enters a working
	// credential. See pkg/agent/tool/authfail (Recorder.ObserveFailure).
	//
	// Deliberately a distinct bool rather than stamping HTTPStatus = 200:
	// httpStatuses is catalog-declarable, so a provider declaring 200 would
	// invert proof-it-works into corroboration-it-is-broken, and the fact is
	// kind-agnostic (a CLI exit 0 demonstrates the same thing).
	OriginAuthenticated bool
	// UnbilledFailure reports a FAILED toolkit-origin call whose provider
	// metered NOTHING for it — the toolkit's own structured terminal event said
	// both. It is the shape of a credential the provider refused, as opposed to
	// work that ran and then failed, and pkg/authz/toolguard answers a
	// consecutive streak of them by ending the session rather than letting a
	// dead credential burn every remaining turn.
	//
	// Set by pkg/agent/tool/sandbox (composeStreamResult) from
	// toolkitstream.Outcome.UnbilledFailure, and only when the process itself
	// failed. False means "not observed", never "authenticated".
	//
	// Never derived from stdout or stderr, and no CRD field may make it so. The
	// agent chooses the argv, every CLI echoes argv back into its errors, and
	// claude writes its authentication error to STDOUT — so a text-matched
	// version of this field would be a prompt-injection primitive: a hostile
	// tool result could end the session on demand, or read as ordinary work and
	// hide a real credential failure. That is also why it is separate from
	// Stderr, which a provider's authFailure.stderrPatterns DOES match against:
	// that path ends in a card a human judges, this one ends the session
	// unattended.
	UnbilledFailure bool
	// RenderedLive reports that the live surfaces ALREADY showed this result's
	// Content to the user — it is prose composed for a human, not plumbing the
	// user never saw. It is what lets a resumed transcript reproduce the
	// conversation the user actually watched: the runner stamps it onto the
	// persisted tool_result block, and turn.VisibleTimeline renders exactly the
	// blocks carrying it (see pkg/memory/kinds/turn/visible.go). Without it a
	// streaming sub-agent's report is on screen while it streams and gone on
	// reload, because a transcript alone cannot tell that result from a memory
	// query's rows.
	//
	// Set by pkg/agent/tool/sandbox (composeStreamResult) — the ONE place a
	// streaming/interactive toolkit's output is composed for a human. It is
	// deliberately per-CALL rather than a property the tool declares: a
	// streaming tool's argument-validation and missing-hooks failures return
	// before any stream exists, so keying on the tool's mode would resurrect
	// text the live surfaces never rendered.
	//
	// False means "not shown", never "hidden on purpose" — hiding an ordinary
	// tool result is the default and stays that way.
	//
	// A result carrying it can never also carry SecretOutput or UIResource:
	// both are set only on the sync (composeResult) and MCP paths, so the
	// runner's Content-replacing transforms cannot silently invalidate it.
	RenderedLive bool
	// IdleExit, when true on a Terminal result, signals the runner to write
	// phase=Idle (not Succeeded). Only meaningful when Terminal is true and
	// the session is channel-attached. Set by the tools that park the session
	// on someone else's reply — await_user_message and ask_parent — when the
	// idle TTL expires or the run context ends, and by respond_to_user when a
	// share is denied.
	IdleExit bool
	// ShareDenied marks a Terminal+IdleExit result that yields to Idle because a
	// response share was denied by the information-leakage gate (respond_to_user),
	// as opposed to a plain await_user_message TTL/cancel idle. The phase outcome
	// is identical (Idle), but it lets the runner record a distinct lifecycle
	// audit event (ShareDeniedYield vs IdleYield). Only meaningful when Terminal
	// and IdleExit are both true.
	ShareDenied bool
	// AwaitResumed is set by the tools that park the session on someone else's
	// reply — await_user_message and ask_parent — and only when a real inbound
	// message wakes them (never on TTL/ctx idle exit). The runner's mid-loop
	// inbox drain reads it to tell a yield-resume (drain the held inbox now)
	// from ordinary mid-tool-loop work (hold the inbox until the next yield).
	// See pkg/agent/runner/queuedmessages.go (resultsIncludeAwaitResume).
	AwaitResumed bool
	// SecretOutput, when non-nil, tells the runner to divert Content (the
	// secret value) to the per-session secret-output store and replace the
	// tool_result with a handle + Description. The value never reaches the LLM.
	SecretOutput *secretout.Spec
	// ContainerUpload, when non-nil, is meant to make the runner append a
	// container_upload block referencing FileID to the next provider request so
	// the provider preloads that file into the code-execution container. Set by
	// mount_artifact. NOT YET CONSUMED — the loop has no handling for it (compare
	// applySecretOutput, which IS wired), so mount_artifact must not be offered;
	// that is guarded today by capability.RunnerEnv.FileBridge always being nil.
	//
	// When it IS wired: the container_upload block belongs in its own message,
	// never merged into the Role:"user" message carrying tool_result blocks. That
	// message's all-blocks-are-tool_result shape is what tells the attachment
	// hydration pass it is a relay and not a human turn; breaking it silently
	// truncates every attachment's lifetime mid-tool-loop (see isToolResultRelay,
	// pkg/agent/runner/attachments.go).
	ContainerUpload *ContainerUploadSpec
	// UIResource, when non-nil, carries an MCP UI resource (mcp-ui / MCP Apps
	// ui://) an MCP tool returned instead of plain content. The MCP dispatcher
	// sets it and replaces Content with a short trusted summary, so the untrusted
	// widget HTML never reaches the LLM. The runner's applyUIResource hook
	// escalates it into a session_view_offer envelope and then clears the field.
	UIResource *UIResourceSpec
	// Trusted marks a meta-tool result whose Content is framework-controlled and
	// safe to skip content-guard inspection. Secure-by-default: the zero value
	// means UNTRUSTED, so a new meta tool surfacing third-party content is
	// inspected unless it deliberately opts out. The guards are needed here only
	// because meta tools otherwise bypass the pipeline (leakageGateApplies
	// excludes KindMeta); non-meta tools already run through it. The nonce'd
	// <untrusted-tool-output> wrapper applies to ALL results regardless.
	Trusted bool
}

// ContainerUploadSpec identifies a provider file (from the Files API, via the
// files-modality bridge) to preload into the code-execution container.
type ContainerUploadSpec struct {
	FileID string
}

// UIResourceSpec carries an MCP UI resource intercepted from an MCP tool result.
type UIResourceSpec struct {
	// URI is the resource's ui:// identifier. Recorded, not yet read.
	URI string
	// MIMEType distinguishes an inline widget document from an mcp-ui
	// externalUrl (text/uri-list). Recorded, not yet read.
	MIMEType string
	// HTML is the raw widget document body (UNTRUSTED, MCP-server-authored).
	// For an externalUrl resource this holds the URL, not HTML.
	HTML []byte
	// Meta is the resource's _meta; the runner reads _meta.ui.csp from it.
	Meta json.RawMessage
	// Origin is the MCPServer CR name — the credential-scoping / revocation key,
	// NOT the LLM display prefix. Recorded, not yet read.
	Origin string
	// Tool is the LLM-facing name of the tool that returned the resource.
	Tool string
}

// AgentResult mirrors v1alpha1.AgentResult but is import-cycle-safe.
// agent_work_complete writes into this via SessionContext.SubmitResult.
type AgentResult struct {
	Summary   string
	Artifacts []ResultArtifact
}

// ResultArtifact identifies a single artifact produced by the session,
// referenced by ArtifactStore ID with a human-readable description.
type ResultArtifact struct {
	ID          string
	Description string
}

// SessionContext is the runner-side handle exposed to a Tool's Execute.
//
// internal/cmd/runner populates it once per session and hands the same value to every
// tool; SubmitResult is filled in later, inside Loop.Run, because it captures
// per-loop state. The `any`-typed fields are typed that way only to keep this
// package free of a controller-runtime and artifact-client dependency — their
// dynamic types are fixed and documented per field, and tools type-assert them.
type SessionContext struct {
	// Namespace and Name identify the owning AgentSession CR.
	Namespace string
	Name      string

	// IsDelegatedChild reports whether this session was spawned by a delegation
	// (AgentSession.spec.parent is set). Set from that one discriminator, the
	// same one core.go uses to swap the terminal tool and offer request_input.
	//
	// A completion requirement reads it to answer a question a child cannot: a
	// child never authors a plan (planning tools are withheld from it), so
	// plan-steps-complete has no steps of its OWN to judge — it is satisfied by
	// design, not by the accident of an empty plan store. Carried here rather
	// than re-derived from spec.parent because a Check sees only the
	// SessionContext, never the CR.
	IsDelegatedChild bool

	// AgentSessionUID is the UID of the owning AgentSession CR. Used by
	// sandbox tools to set OwnerReferences on ToolCall CRs so cascade GC
	// works when the AgentSession is deleted.
	AgentSessionUID types.UID

	// SubmitResult is called by agent_work_complete to publish the final
	// session result. Runner consumes this in its loop after Tool.Execute
	// returns Terminal=true.
	SubmitResult func(AgentResult)

	// Memory appends turn content out of band. Nothing populates it today, so
	// it is always nil; see MemoryAppender.
	Memory MemoryAppender

	// Mem is the memory query interface backing query_memory / search_memory.
	Mem MemoryQuerier

	// KG is the knowledge-graph querier backing query_knowledge.
	KG KGQuerier

	// K8sClient is a sigs.k8s.io/controller-runtime/pkg/client.Client, the
	// same one internal/cmd/runner builds. Sandbox and interactive tools assert it to
	// create ToolCall CRs; the workspace meta-tools assert it to read theirs.
	K8sClient any
	// ArtifactClient is a sandbox.ArtifactClient (production:
	// *sandbox.HTTPArtifactClient). artifact_prepare asserts it to read stored
	// artifact bytes back.
	ArtifactClient any
	// BundleSessions maps a bundle name to its SpiceboxSession name, so a
	// sandbox tool knows which session to dispatch its ToolCall against.
	BundleSessions map[string]string

	// Operations is the per-session audit registry. new_operation calls
	// Begin to register a new logical operation; sandbox dispatch calls
	// Get to validate every call's operation_id and RecordCall to bump the
	// per-operation tool-call counter.
	Operations OperationRegistry

	// State is the per-session, in-process registry of typed state stores keyed
	// by Kind name; kinds plug in via pkg/agent/session/state.Register without
	// runner edits. Nil-safe — an unregistered Kind sees (nil, false) from Get.
	State StateRegistry

	// SecretOut is the per-session secret-output store. Non-nil in production;
	// the runner only dereferences it when a Result carries SecretOutput.
	SecretOut secretout.Store
}

// Operation is the audit record for a logical unit of work the agent has
// declared via new_operation.
type Operation struct {
	// ID is the registry-minted operation id tool calls cite as operation_id.
	ID string
	// Description is the agent's stated purpose for the operation.
	Description string
	// CreatedAt is when Begin registered the operation.
	CreatedAt time.Time
	// Closed makes the sandbox dispatcher reject further calls against this
	// operation. Set by Close, including when a plan item transitions to `done`.
	Closed bool
	// Parent anchors this operation in the audit graph; nil for top-level ones.
	Parent *OperationParent
	// Calls is the chronologically-ordered tool calls recorded against this
	// operation. Its length IS the tool-call count — there is no separate counter.
	Calls []OperationCall
	// Root marks the per-session fallback operation minted by
	// OperationRegistry.Root. It is what makes the audit graph total: a call
	// with neither an explicit operation_id nor an in-progress plan item
	// attributes here rather than nowhere.
	//
	// Readers that DISPLAY operations must skip it. It is an anchor, not work
	// the agent declared, and buildOperationActivity treats a call-less
	// operation as having current work — so an unfiltered root would sit in the
	// live activity tree for the whole session saying nothing.
	Root bool
}

// OperationParent anchors an Operation to one parent. Exactly one of
// OperationID / PlanItem is set; the other is zero-valued. Validated at
// SetParent time; readers can assume the invariant holds.
type OperationParent struct {
	OperationID string       // sibling/child operation
	PlanItem    *PlanItemRef // plan-item-anchored operation
}

// PlanItemRef identifies one plan item by (plan name, item id). Stored
// on Operation.Parent.PlanItem when an operation is auto-opened by
// update_plan or manually opened with parent={plan, item}.
type PlanItemRef struct {
	Plan string
	Item string
}

// OperationCall is the per-call audit entry captured by RecordCall.
type OperationCall struct {
	// Tool is the LLM-facing tool name, so an audit reader can correlate the
	// entry against the model trace.
	Tool string
	// Reason is the LLM-supplied "_reason" argument on the tool call.
	Reason string
	// ToolCallID is the ToolCall CR name; "" for synchronous tools.
	ToolCallID string
	// At is when RecordCall appended the entry.
	At time.Time
	// CompletedAt is set by CompleteCall once the dispatch returned (success or
	// error). Zero means still in flight, which is how buildOperationActivity
	// decides whether an operation keeps surfacing in the live activity tree.
	CompletedAt time.Time
}

// OperationRegistry is the per-session audit-trail interface used by the
// new_operation meta tool and the sandbox dispatcher. The concrete impl
// lives in pkg/agent/tool/operations to keep this package free of state.
type OperationRegistry interface {
	// Begin registers a new operation with the given description and returns
	// it (with the freshly-minted ID). Implementations must be safe under
	// concurrent calls.
	Begin(description string) Operation
	// Root returns the session-root operation, minting it on first call and
	// returning the same one thereafter. Lazy rather than eager so a session
	// that attributes every call explicitly carries no phantom node.
	//
	// The returned operation is ordinary in every other respect — Get,
	// RecordCall and CompleteCall all work against its ID, which is what lets
	// the ambient resolver hand it to the same recording path every other call
	// uses instead of growing a second one.
	Root() Operation
	// Get returns the operation registered under id. ok=false when no such
	// operation exists; callers must reject the dispatch in that case.
	Get(id string) (Operation, bool)
	// RecordCall appends an audit entry and returns its index within
	// Operation.Calls — stable for the operation's life, so CompleteCall can mark
	// the same call done. Returns (-1, false) without mutating state when id is
	// not registered.
	RecordCall(id string, call OperationCall) (index int, ok bool)
	// CompleteCall stamps CompletedAt on the call at index so it stops counting
	// as in-flight. False when id is unknown or index is out of range; callers
	// defer it so it fires on every Execute exit path.
	CompleteCall(id string, index int) bool
	// All returns a snapshot of all registered operations, for audit dumps and
	// status reporting.
	All() []Operation
	// SetParent anchors the operation in the audit graph; false when id is
	// unknown. p must satisfy the XOR invariant (exactly one of OperationID /
	// PlanItem); the implementation panics on violation, as that is a programmer
	// error rather than user input.
	SetParent(id string, p *OperationParent) bool
	// Close rejects subsequent dispatches against id. False when id is unknown;
	// idempotent (true) on an already-closed operation.
	Close(id string) bool
}

// StateStore is the per-session, in-process state for one registered
// session-state Kind. The concrete impls live in pkg/agent/session/state/<kind>;
// the framework lives in pkg/agent/session/state.
type StateStore interface {
	// Kind returns the discriminator the system_note wrapper carries (e.g.
	// "plans"), stable across the Store's lifetime.
	Kind() string
	// ReplayNote feeds a previously-emitted system_note's `data` field back in
	// during memory replay so the store rebuilds itself on resume.
	// Implementations log and continue on malformed input; a non-nil error is
	// reserved for genuine bugs.
	ReplayNote(payload json.RawMessage) error
}

// StateRegistry holds one StateStore per registered Kind for a single
// session. Lookups are by name; tools cast the returned StateStore to
// the kind's concrete type via the kind's typed accessor (e.g.,
// pkg/agent/session/state/plans.From).
type StateRegistry interface {
	Get(name string) (StateStore, bool)
}

// MemoryAppender is the interface a Tool would use to add a turn to memory
// outside the loop's normal append path. Nothing implements or calls it today —
// SessionContext.Memory is never populated.
type MemoryAppender interface {
	AppendTurn(ctx context.Context, role string, content string) error
}

// MemoryQuerier is the interface a Tool uses to query session memory.
// Satisfied by memory.Memory (the Local facade or httpclient.Client).
type MemoryQuerier interface {
	Query(ctx context.Context, q memory.Query) (memory.QueryResult, error)
	Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error)
}

// KGQuerier is the interface a Tool uses to query the knowledge graph.
// Satisfied by memory.KGProvider and httpclient.KGClient.
type KGQuerier interface {
	SearchFacts(ctx context.Context, query string, limit int) ([]memory.KGFact, error)
	GetEntity(ctx context.Context, uuid string) (*memory.KGEntity, error)
	EntityFacts(ctx context.Context, entityUUID string) ([]memory.KGFact, error)
	RelatedEntities(ctx context.Context, entityUUID string, limit int) ([]memory.KGEntity, error)
	Communities(ctx context.Context, groupID string) ([]memory.KGCommunity, error)
}

// PerCallPermission is an optional Tool capability: resolve the effective
// permission for ONE call's arguments.
//
// It exists because a tool is not always one authorization question. A sandbox
// tool is synthesized per TOOLSPEC, covering a whole CLI, so `gh pr create` and
// `gh pr view` arrive at the same tool with very different severity — external
// versus readonly. Resolving from the tool alone authorized them identically,
// as the toolkit-level default, discarding every per-subcommand check the
// toolkit declared.
//
// Distinct from PermissionVariants, which answers the same question through CEL
// over named args. A tool that can parse its own call answers better than an
// expression that has to re-derive the parse: the toolkit parser already knows
// how to skip leading global flags. Consumers prefer this when present and fall
// back to variants, so MCP tools are unaffected.
//
// ok=false means "I cannot answer for this call" — the caller then falls back.
// It does NOT mean deny.
type PerCallPermission interface {
	PermissionForCall(args map[string]any) (authz.Permission, bool)
}

// PlanGateGoverned is an optional Tool capability: a tool that the plan gate
// must govern even though its DISPATCH permission is Stateless.
//
// It exists for exactly one shape — delegate — where the action is
// consequential to the plan (spawning a child hands a fresh tool surface to a
// new actor) but reaches no SpiceDB resource of its own, so it cannot carry a
// Readwrite/External permission without either being denied fail-closed by the
// tool checker or forcing per-call approval in every mode. The marker decouples
// the two questions: dispatch stays Stateless (no check, no approval), while the
// plan gate is handed a `tool:<name>` handle so it governs the call under
// `enforcing` mode. A tool that returns false, or does not implement this, is
// unaffected — its plan-gate handle is derived from its Permission as before.
type PlanGateGoverned interface {
	PlanGateGoverned() bool
}

// PipelineRouted is an optional Tool capability: a tool that must flow through
// the runner's CONTAINED dispatch pipeline even though its dispatch permission
// is Stateless, because a PreToolCall hook — not the tool checker — is what
// governs it.
//
// It is the second marker of the shape PlanGateGoverned introduced, and it
// exists for the same reason: routing and authorization are different
// questions, and StateImpact answers only the second. record_observation is the
// tool that needs it. Its destination is a resource pool named per call, so its
// TYPE varies — PermissionCheck.ResourceType is a static string with no
// interpolation, so no Check can express it — and the gate that does authorize
// the call (the info-leakage audience gate at PreToolCall, plus the memory
// door's own write-grant proof server-side) runs only on the contained path.
//
// Reaching that path by declaring a check-requiring StateImpact instead is what
// this marker exists to stop. Readonly and Readwrite share one arm in
// toolcheck.Checker.check, and that arm DENIES a nil Check outright
// ("internal: stateImpact requires a check but none was supplied"); needsApproval
// returns false for it, so no card is raised either. A tool declared that way is
// refused on every call under the CRD-default enforcing mode, and — because
// ToolCallAuthz runs at order 20 and InfoLeakAudience at order 50 — the gate it
// was routed in for never runs at all. External is not the escape hatch it
// looks like: it takes a different arm entirely (checkExternalSlotGrant) and
// forces per-call human approval in every mode.
//
// A tool that returns false, or does not implement this, is unaffected.
type PipelineRouted interface {
	PipelineRouted() bool
}

// PlanGateHandle returns the handle the PLAN GATE should govern a tool by,
// which is the tool's dispatch handle (BaseHandle) UNLESS the tool opts into
// governance via PlanGateGoverned — then it is `tool:<name>` regardless of a
// Stateless dispatch permission. This is the only place the two derivations are
// allowed to differ, and only in the safe direction: the gate sees MORE than
// the dispatcher, never less, so a governed spawn is never silently skipped.
func PlanGateHandle(t Tool) (permsurface.Handle, bool) {
	if g, ok := t.(PlanGateGoverned); ok && g.PlanGateGoverned() {
		if h, err := permsurface.NewToolHandle(t.Name()); err == nil {
			return h, true
		}
	}
	return BaseHandle(t)
}

// NamedCallArgs is an optional Tool capability: render a call's arguments as
// the NAMED map a permission check reads.
//
// Authorization checks resolve a resource id from named arguments —
// `resourceIDTemplate: "{repo}"` looks up a key. MCP tools already arrive that
// way. A sandbox tool arrives with an argv array, so the template resolved
// against nothing and every per-resource check on one failed. The permission
// itself resolved correctly; the resource did not, which is the half that says
// WHICH instance — and therefore the half a slot binds.
//
// The tool renders this because only the tool knows how to parse its own call.
// Absent the capability, args pass through unchanged.
//
// ok=false means "these arguments do not parse". Callers must then leave the
// args alone rather than substitute a partial view: a check resolving against
// arguments the call never made would authorize a resource nobody named.
type NamedCallArgs interface {
	NamedArgs(args map[string]any) (map[string]any, bool)
}
