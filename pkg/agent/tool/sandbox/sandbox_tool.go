package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/observe"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/render"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

const (
	maxInlineStdout = 32 * 1024 // 32 KiB
	maxInlineStderr = 4 * 1024  // 4 KiB on the failure path
)

// SandboxOpts configures a synthesized sandbox Tool.
type SandboxOpts struct {
	BundleName string // e.g. "code"
	Suffix     string // class tool name; the LLM sees "<bundle>_<suffix>"
	// (Anthropic tool names must match ^[a-zA-Z0-9_-]{1,128}$ — no dots.)
	Description  string        // pre-rendered LLM-facing description
	ToolspecName string        // metadata; surfaced in deterministic CR labels
	Timeout      time.Duration // per-call ToolCall.Spec.Timeout (default 60s)
	PollInterval time.Duration // 250ms in production; tests override

	// Toolkit is the parsed toolkit definition backing this tool.
	// Its Permission field is the default authz block for all subcommands.
	Toolkit *toolkit.Toolkit
	// Spec is the parsed toolspec backing this tool — the capability
	// contract authored against Toolkit's revision. Retained so
	// Introspect can render the full effective contract on demand
	// (Description holds only the pre-rendered summary).
	Spec *spec.Spec
	// Subcommand, when non-nil, is the specific subcommand this adapter
	// represents. Its Permission overrides Toolkit.Permission when set.
	// Its Mode field determines whether Execute routes through the
	// streaming/bridge path (stream or interactive) or sync path (default).
	Subcommand *toolkit.Subcommand

	// Credentials is the pre-computed credential descriptor slice stamped onto
	// every ToolCall.Spec.Credentials this tool creates. Built by the caller of
	// Synthesize (internal/cmd/runner, e2e factory) via credresolve.Descriptors over the
	// cli kind's SetupRequirements.
	// nil is safe (no descriptors are stamped when the slice is empty).
	Credentials []spiceboxv1alpha1.CredentialDescriptor
}

// IDs are passed by the runner per-call so deterministic ToolCall names
// survive resume across runner pod restarts.
type IDs struct {
	TurnIndex int
	ToolUseID string

	// BlockIndex is this tool_use's position within the assistant turn (1-based;
	// 0 = SeqBlockStart is reserved for the turn-start signal).
	// MemTurnIndex is the assistant turn's durable memory Index (rebuilt by
	// replay across resume; NOT the per-Run turnCount which resets to 0 on
	// restart). SessionUID is the UID of the owning AgentSession CR.
	// Together these three fields let producers stamp Envelope.Seq via
	// PackSeq(MemTurnIndex, BlockIndex) so the channelsd status state machine
	// can order and deduplicate status events correctly across resume and fork.
	// See the status-signaling design.
	BlockIndex   int
	MemTurnIndex int
	SessionUID   string

	// PreDispatch, when non-nil, requests the operator's toolcall
	// controller take a workspace snapshot before dispatching this
	// ToolCall. Set by the runner for stateful tool calls
	// (stateImpact ∈ {readwrite, external}). The sandbox tool
	// stamps this onto ToolCallSpec.PreDispatchSnapshot at build
	// time.
	PreDispatch *spiceboxv1alpha1.PreDispatchSnapshot
}

// maxConsecutivePollErrors is how many back-to-back failed reads of a
// dispatched ToolCall the watcher tolerates before it declares the call
// unobservable and fails it. Counted in polls rather than wall-clock so it
// scales with PollInterval: at the 250ms default that is a full minute of
// continuous failure — long enough to ride out an apiserver rollout, short
// enough that a permanently unreadable ToolCall cannot pin an agent turn open
// indefinitely. Any successful read resets the count.
const maxConsecutivePollErrors = 240

// NewSandboxTool builds a single Tool whose Execute issues a ToolCall against
// the bundle's SpiceboxSession and returns the resulting stdout (or stderr on
// failure).
func NewSandboxTool(opts SandboxOpts) *SandboxTool {
	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 250 * time.Millisecond
	}
	// Resolve the effective Permission at construction time.
	// Subcommand-level override wins over toolkit-level default.
	// If neither is set, permission is the zero value; AgentClass validation
	// will reject tools without a Permission spec — that is intentional.
	perm := resolvePermission(opts.Toolkit, opts.Subcommand)
	return &SandboxTool{opts: opts, permission: perm}
}

// resolvePermission applies the subcommand-wins-over-toolkit priority.
func resolvePermission(tk *toolkit.Toolkit, sub *toolkit.Subcommand) authz.Permission {
	if sub != nil && sub.Permission != nil {
		return *sub.Permission
	}
	if tk != nil && tk.Permission != nil {
		return *tk.Permission
	}
	return authz.Permission{}
}

// streamMode returns the ToolCallMode for this tool's subcommand:
// stream / interactive when the subcommand declares one, else sync.
func (o SandboxOpts) streamMode() spiceboxv1alpha1.ToolCallMode {
	if o.Subcommand == nil {
		return spiceboxv1alpha1.ToolCallModeSync
	}
	switch o.Subcommand.Mode {
	case toolkit.SubcommandModeStream:
		return spiceboxv1alpha1.ToolCallModeStream
	case toolkit.SubcommandModeInteractive:
		return spiceboxv1alpha1.ToolCallModeInteractive
	default:
		return spiceboxv1alpha1.ToolCallModeSync
	}
}

// resolveCallTimeout picks the per-call ToolCall.Spec.Timeout. A streaming /
// interactive tool is pinned to a single subcommand whose budget was already
// resolved into opts.Timeout at synthesis, so it keeps that value. A sync tool
// can span several subcommands with different budgets (e.g. git clone vs git
// status on one `code_git` tool); the budget is resolved from the subcommand
// the argv actually selects, honoring only an explicitly-declared sc.Timeout so
// a caller's opts.Timeout is never overridden by the generic mode default.
func (s *SandboxTool) resolveCallTimeout(args []string) time.Duration {
	if s.opts.Subcommand != nil || s.opts.Toolkit == nil {
		return s.opts.Timeout
	}
	if d, ok := subcommandTimeout(parser.IdentifySubcommand(s.opts.Toolkit, args)); ok {
		return d
	}
	return s.opts.Timeout
}

type SandboxTool struct {
	opts       SandboxOpts
	permission authz.Permission

	// relWriter is the SpiceDB write surface for the toolspec's
	// WritesRelationships post-effect (opts.Spec.WritesRelationships).
	// Wired by the runner at session start via SetRelWriter; nil-safe —
	// when unset, the post-effect is skipped. Mirrors
	// mcp.MCPTool.relWriter.
	relWriter relwrites.Writer
	// slotBoundChecker answers, per resolved tuple, whether this session holds
	// a slot grant on the tuple's resource. Consulted ONLY for a block whose
	// RequireSlotBound is true. Wired by the runner at session start alongside
	// relWriter; nil is NOT a bypass — relwrites.Run refuses every tuple of a
	// marked block when it is nil. Mirrors mcp.MCPTool.slotBoundChecker.
	slotBoundChecker relwrites.SlotBoundChecker
	// logger reports post-effect (relwrites) failures without changing
	// the returned tool.Result. Falls back to slog.Default() when nil.
	// Mirrors mcp.MCPTool.logger.
	logger *slog.Logger

	// mem is the in-process memory framework used to record the toolspec's
	// declared Observes blocks (opts.Spec.Observes) as observed facts after a
	// SUCCESSFUL call. Wired by the runner at session start via SetMemory;
	// nil-safe — when unset, the post-effect is skipped. Mirrors
	// mcp.MCPTool.mem.
	mem memorypkg.Memory
}

// SetRelWriter wires the SpiceDB writer for the post-Succeeded
// WritesRelationships post-effect declared on this tool's toolspec. Called
// by the runner at session start; nil is allowed (the post-effect is then
// skipped). Mirrors mcp.MCPTool.SetRelWriter.
func (s *SandboxTool) SetRelWriter(w relwrites.Writer) { s.relWriter = w }

// SetSlotBoundChecker wires the per-tuple slot-grant check used by any
// declared block with requireSlotBound. Called by the runner at session start
// beside SetRelWriter, and by the e2e harness's own factory — those are two
// wiring sites, not one. Nil is accepted but is not a bypass: relwrites.Run
// refuses every tuple of a marked block against a nil checker. Mirrors
// mcp.MCPTool.SetSlotBoundChecker.
func (s *SandboxTool) SetSlotBoundChecker(c relwrites.SlotBoundChecker) { s.slotBoundChecker = c }

// SetLogger wires a slog logger used for post-effect failure reporting.
// Nil falls back to slog.Default() at call time. Mirrors
// mcp.MCPTool.SetLogger.
func (s *SandboxTool) SetLogger(l *slog.Logger) { s.logger = l }

// SetMemory wires the in-process memory framework used to record this tool's
// declared Observes blocks. Called by the runner at session start
// (internal/cmd/runner/main.go, alongside SetRelWriter/SetLogger); nil is
// allowed (the post-effect is then skipped). Mirrors mcp.MCPTool.SetMemory —
// per RULING T5-B, this setter is deliberately paired with a production call
// site in internal/cmd/runner/main.go, not left as a test-only hook.
func (s *SandboxTool) SetMemory(mem memorypkg.Memory) { s.mem = mem }

func (s *SandboxTool) Name() string        { return s.opts.BundleName + "_" + s.opts.Suffix }
func (s *SandboxTool) Kind() tool.Kind     { return tool.KindSandbox }
func (s *SandboxTool) Description() string { return s.opts.Description }

// Origin implements tool.OriginTool. It returns "toolkit/<name>" where
// <name> is the SpiceboxToolkit this tool belongs to — the revocation unit
// used by toolguard for origin-level circuit breakers. Returns "" when the
// tool has no toolkit (origin-less; the guard never revokes it).
func (s *SandboxTool) Origin() string {
	if s.opts.Toolkit == nil || s.opts.Toolkit.Name == "" {
		return ""
	}
	return "toolkit/" + s.opts.Toolkit.Name
}

// Permission returns the effective authz.Permission for this tool.
// Subcommand-level Permission wins over toolkit-level; if neither is set,
// the zero Permission{} is returned (AgentClass validation rejects this).
func (s *SandboxTool) Permission() authz.Permission {
	return s.permission
}

// Introspect renders the tool's full effective contract via the
// toolspec renderer — every allowed subcommand with flags/positionals,
// effects, and constraint justifications. Satisfies tool.Introspectable.
func (s *SandboxTool) Introspect() (string, error) {
	if s.opts.Spec == nil || s.opts.Toolkit == nil {
		return "", fmt.Errorf("introspect: tool %q has no spec/toolkit loaded", s.Name())
	}
	return render.Describe(s.opts.Spec, s.opts.Toolkit, render.FormatMarkdown)
}

func (s *SandboxTool) InputSchema() json.RawMessage {
	return tool.WrapInputSchema(
		json.RawMessage(`{"type":"array","items":{"type":"string"},"description":"argv passed to the tool. The exact flags and positional arguments accepted by each subcommand are NOT enumerated here — call introspect_tool with this tool's name first to get them. Guessing flag names wastes turns: the parser is strict and rejects unknown flags."}`),
		map[string]json.RawMessage{
			"stdin": json.RawMessage(`{"type":"string","description":"Optional stdin bytes."}`),
		},
	)
}

// Execute is the loop-side entry point. The runner provides the per-turn IDs
// via SessionContext-side state, but for tests we expose ExecuteWithIDs
// directly. The Execute path falls back to a generated ID if the SessionContext
// doesn't carry one (defensive; the runner always does).
func (s *SandboxTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	// In production the runner passes IDs out-of-band via a context value so
	// names are deterministic. For tests, callers use ExecuteWithIDs directly.
	ids, _ := IDsFromCtx(ctx)
	if ids.ToolUseID == "" {
		ids = IDs{TurnIndex: 0, ToolUseID: "anon"}
	}
	if s.opts.streamMode() != spiceboxv1alpha1.ToolCallModeSync {
		hooks, ok := InteractiveHooksFromCtx(ctx)
		if !ok {
			return tool.Result{
				Content: "interactive tool dispatched without InteractiveHooks (runner not channel-attached?)",
				IsError: true,
			}, nil
		}
		return s.executeStreaming(ctx, raw, sess, ids, hooks)
	}
	return s.ExecuteWithIDs(ctx, raw, sess, ids)
}

type idsContextKey struct{}

// WithIDs returns a context carrying the per-call IDs.
func WithIDs(ctx context.Context, ids IDs) context.Context {
	return context.WithValue(ctx, idsContextKey{}, ids)
}

// IDsFromCtx returns the IDs stored by WithIDs, and whether they were present.
func IDsFromCtx(ctx context.Context) (IDs, bool) {
	ids, ok := ctx.Value(idsContextKey{}).(IDs)
	return ids, ok
}

type sandboxArgs struct {
	// OperationID is the Operation this call is audited against.
	OperationID string `json:"operation_id"`
	// Reason is the LLM's stated justification, recorded in the audit entry.
	Reason string `json:"_reason"`
	// Args is the toolkit command's argv tail.
	Args []string `json:"args"`
	// Stdin is fed to the process; empty means a closed stdin.
	Stdin string `json:"stdin,omitempty"`
}

func (s *SandboxTool) ExecuteWithIDs(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext, ids IDs) (tool.Result, error) {
	var a sandboxArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tool.Result{Content: "sandbox: invalid arguments: " + err.Error(), IsError: true}, nil
	}
	if a.OperationID == "" {
		return tool.Result{Content: "sandbox: operation_id is required — call new_operation first and pass its operation_id on every tool call", IsError: true}, nil
	}
	if a.Reason == "" {
		return tool.Result{Content: "sandbox: _reason is required — explain why this specific call is needed for the operation", IsError: true}, nil
	}
	if sess == nil || sess.K8sClient == nil {
		return tool.Result{}, fmt.Errorf("sandbox: SessionContext.K8sClient is nil")
	}
	if sess.Operations == nil {
		return tool.Result{}, fmt.Errorf("sandbox: SessionContext.Operations is nil — runner must wire an OperationRegistry")
	}
	op, ok := sess.Operations.Get(a.OperationID)
	if !ok {
		return tool.Result{
			Content: fmt.Sprintf("sandbox: operation_id %q is not registered — call new_operation to mint a fresh operation_id before issuing tool calls", a.OperationID),
			IsError: true,
		}, nil
	}
	if op.Closed {
		return tool.Result{
			Content: fmt.Sprintf("sandbox: operation_id %q is closed; this work item is finished. Open a new operation (or update_plan an item to in_progress) before retrying.", a.OperationID),
			IsError: true,
		}, nil
	}
	c, ok := sess.K8sClient.(client.Client)
	if !ok {
		return tool.Result{}, fmt.Errorf("sandbox: SessionContext.K8sClient does not satisfy controller-runtime client.Client")
	}
	bundleSession, ok := sess.BundleSessions[s.opts.BundleName]
	if !ok || bundleSession == "" {
		return tool.Result{Content: "sandbox: no SpiceboxSession registered for bundle " + s.opts.BundleName, IsError: true}, nil
	}
	artClient, _ := sess.ArtifactClient.(ArtifactClient)

	// Write-once secret-output fast-fail (ordering-critical — must run BEFORE
	// the ToolCall CR is created). When the toolspec declares a secretOutput,
	// refuse to execute a second time for the same name: re-Get the
	// AgentSession for its current status (SessionContext carries no status
	// snapshot) and check Status.SatisfiedSecretOutputs. Without this gate, a
	// second fetch would run the ToolCall to Succeeded, fire the toolspec's
	// WritesRelationships pin block for a SECOND target (a bogus pin — see
	// evaluateWritesRelationships) and only then be rejected by the
	// operator's 409. Failing here, before any ToolCall or relwrites side
	// effect, means the pin block can only ever fire once per session.
	if s.opts.Spec != nil && s.opts.Spec.SecretOutput != nil {
		soName := s.opts.Spec.SecretOutput.Name
		var current spiceboxv1alpha1.AgentSession
		if err := c.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &current); err != nil {
			return tool.Result{
				Content: fmt.Sprintf("sandbox: secret-output %q: failed to verify write-once status: %v", soName, err),
				IsError: true,
			}, nil
		}
		for _, sat := range current.Status.SatisfiedSecretOutputs {
			if sat.Name == soName {
				return tool.Result{
					IsError: true,
					Content: fmt.Sprintf("secret-output %q already satisfied for this session (write-once): the session is pinned; start a new session to target a different cluster", soName),
				}, nil
			}
		}
	}

	// ToolCall name must satisfy RFC 1123 subdomain (lowercase alphanumeric +
	// '-'). Anthropic tool_use IDs look like "toolu_01WW…" — uppercase letters
	// and underscores both violate, so normalize before composing.
	tcName := fmt.Sprintf("%s-%d-%s", sess.Name, ids.TurnIndex, normalizeToolCallNameSegment(ids.ToolUseID))
	tval := true
	ownerRefs := []metav1.OwnerReference{}
	if sess.AgentSessionUID != "" {
		ownerRefs = []metav1.OwnerReference{{
			APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:               "AgentSession",
			Name:               sess.Name,
			UID:                sess.AgentSessionUID,
			Controller:         &tval,
			BlockOwnerDeletion: &tval,
		}}
	}
	// Resolve the per-call timeout once: it stamps the ToolCall spec below and
	// also seeds the sync poll loop's progress budget (BudgetSeconds).
	callTimeout := s.resolveCallTimeout(a.Args)
	tc := &spiceboxv1alpha1.ToolCall{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:       "ToolCall",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      tcName,
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentsession":  sess.Name,
				"agentbundle":   s.opts.BundleName,
				"agenttoolspec": s.opts.ToolspecName,
				// Operation ID is a label so `kubectl get toolcall -l ap.operation=<id>`
				// pulls every call belonging to a logical operation.
				"ap.operation": a.OperationID,
				// Raw tool_use ID: the toolcall controller reads this to make the
				// pre-dispatch snapshot's append-only audit entry ID unique per
				// dispatch. Omitting it collapses the ID to `tds-<turn>-<seq>-`
				// and two session runs that reach the same (turn, seq) collide.
				spiceboxv1alpha1.LabelToolUseID: ids.ToolUseID,
			},
			Annotations: map[string]string{
				// Reason is an annotation (free-form text, not for selection).
				"ap.operation/reason": a.Reason,
			},
			OwnerReferences: ownerRefs,
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: bundleSession,
			Tool:    s.opts.Suffix,
			Args:    a.Args,
			Stdin:   a.Stdin,
			Mode:    spiceboxv1alpha1.ToolCallModeSync,
			Timeout: metav1.Duration{Duration: callTimeout},
			// Static, non-secret env the toolkit declares for its own binary.
			// Nil for a toolkit that declares none, so spec.env keeps omitting.
			Env:                 toolkitEnvDefaults(s.opts.Toolkit, nil),
			Credentials:         s.opts.Credentials,
			PreDispatchSnapshot: ids.PreDispatch, // nil when not stateful
		},
	}

	// When the toolspec declares a file:-sourced secret output, the producer
	// writes the secret value to a file in its sandbox rather than to stdout.
	// Request that file via CaptureOutputs so the toolcall controller harvests
	// it into an artifact; composeResult reads it back as the secret value.
	if p, ok := secretOutputFilePath(s.opts.Spec); ok {
		tc.Spec.CaptureOutputs = append(tc.Spec.CaptureOutputs, p)
	}

	if err := c.Create(ctx, tc); err != nil && !errors.IsAlreadyExists(err) {
		return tool.Result{Content: "sandbox: create ToolCall: " + err.Error(), IsError: true}, nil
	}
	// Audit-log the dispatch on the operation registry. The ID was validated
	// above; if RecordCall returns false here it means the operation was
	// concurrently removed, which the registry doesn't currently support —
	// surface as IsError rather than silently dropping the audit entry.
	callIdx, recorded := sess.Operations.RecordCall(a.OperationID, tool.OperationCall{
		Tool:       s.Name(),
		Reason:     a.Reason,
		ToolCallID: tcName,
	})
	if !recorded {
		return tool.Result{
			Content: fmt.Sprintf("sandbox: operation %q disappeared between validation and dispatch", a.OperationID),
			IsError: true,
		}, nil
	}
	// Mark the call done on every exit path (success or error) below — the
	// operation-activity live tree only surfaces operations with in-flight
	// (not-yet-completed) calls.
	defer sess.Operations.CompleteCall(a.OperationID, callIdx)

	// Watch via poll until the ToolCall reaches a terminal condition. While the
	// call runs, emit throttled ToolProgressUpdate liveness ticks (elapsed +
	// budget) via the hook injected by the runner (WithToolProgressHook), so a
	// channel-attached watcher can show the tool as still-running while the LLM
	// itself is blocked awaiting the result.
	hook, hasHook := ToolProgressHookFromCtx(ctx)
	hasHook = hasHook && hook.Emit != nil
	budgetSeconds := int(callTimeout.Seconds())
	startedAt := time.Now()
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	done := make(chan *spiceboxv1alpha1.ToolCall, 1)
	unreadable := make(chan error, 1)
	go func() {
		var lastEmit time.Time
		emittedActive := false
		consecutiveErrs := 0
		ticker := time.NewTicker(s.opts.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var fresh spiceboxv1alpha1.ToolCall
				if err := c.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: tcName}, &fresh); err != nil {
					// A read failure right after Create is legitimate (an
					// informer that has not caught up yet), so retry — but not
					// forever and not silently. Nothing on this path carries a
					// deadline: callTimeout stamps ToolCallSpec.Timeout, never
					// the Go context, so before this guard a permanently
					// unreadable ToolCall (RBAC withdrawn, CR deleted,
					// apiserver unreachable) blocked the tool — and the agent
					// turn behind it — until outer session cancellation, with
					// no log line to find it by.
					consecutiveErrs++
					if consecutiveErrs == 1 {
						logger.Info("sandbox: ToolCall status read failed; retrying",
							"tool", s.Name(), "session", sess.Namespace+"/"+sess.Name,
							"toolcall", tcName, "err", err.Error())
					}
					if consecutiveErrs >= maxConsecutivePollErrors {
						logger.Info("sandbox: giving up on an unreadable ToolCall",
							"tool", s.Name(), "session", sess.Namespace+"/"+sess.Name,
							"toolcall", tcName, "attempts", consecutiveErrs, "err", err.Error())
						unreadable <- err
						return
					}
					continue
				}
				consecutiveErrs = 0
				if hasHook {
					now := time.Now()
					if emit, elapsed := toolProgressTick(now, startedAt, lastEmit, toolProgressThreshold, toolProgressInterval); emit {
						hook.Emit(ctx, ToolProgressUpdate{BudgetSeconds: budgetSeconds, ElapsedSeconds: int(elapsed.Seconds())})
						lastEmit = now
						emittedActive = true
					}
				}
				if isTerminal(&fresh) {
					if hasHook && emittedActive {
						hook.Emit(ctx, ToolProgressUpdate{Done: true})
					}
					done <- &fresh
					return
				}
			}
		}
	}()

	var final *spiceboxv1alpha1.ToolCall
	select {
	case final = <-done:
	case err := <-unreadable:
		return tool.Result{
			Content: "sandbox: lost track of ToolCall " + tcName + " (its status became unreadable): " + err.Error(),
			IsError: true,
		}, nil
	case <-ctx.Done():
		return tool.Result{Content: "sandbox: context canceled before ToolCall terminal", IsError: true}, nil
	}

	// Pass the spec's SecretOutput declaration (nil when not set) so
	// composeResult can mark the result for out-of-band capture.
	var secretOutSpec *spec.SecretOutputSpec
	if s.opts.Spec != nil {
		secretOutSpec = s.opts.Spec.SecretOutput
	}
	res, stdout := composeResult(ctx, final, artClient, secretOutSpec)
	if secretOutSpec != nil {
		// Drop the captured bytes HERE, at the one place that knows they are a
		// secret, rather than handing them down and trusting the callee's own
		// branch not to use them. evaluateObserves keeps that branch as
		// defence in depth, but this is what makes its "unreachable BY
		// CONSTRUCTION" claim true of the data flow and not merely of one
		// `if`: for a secretOutput toolspec the value never travels.
		stdout = ""
	}
	// Both post-effects are reassigned: each can refuse the call outright — a
	// slot-bound relwrites refusal, an observe that failed to record — and
	// tool.Result is a plain value type, so a flip recorded on a copy inside
	// the callee would never reach this return. Ordering matters: a call whose
	// authority write was refused asserts nothing either, and evaluateObserves'
	// own `if res.IsError` gate short-circuits on the Result this line produces.
	res = s.evaluateWritesRelationships(ctx, res, stdout, a.Args, sess)
	res = s.evaluateObserves(ctx, res, stdout, a.Args, sess)
	return res, nil
}

// evaluateWritesRelationships runs the toolspec's declared WritesRelationships
// blocks after composeResult has produced the final tool.Result. Gating on
// res.IsError is stricter than the ToolCall condition alone, because a Succeeded
// ToolCall can still compose an error Result (a missing secret-output file).
//
// It mirrors mcp.MCPTool's post-Execute post-effect, and the line between what
// it swallows and what it surfaces is drawn at whether the outcome is a
// MECHANISM FAILURE or an AUTHORIZATION DECISION:
//
//   - A CEL error, a SpiceDB write rejection, an Exclusive write-once conflict —
//     logged only, Result unchanged. The sandbox call itself succeeded and the
//     agent already has that answer; retracting it would cost the agent a
//     result it can act on in exchange for a failure it cannot.
//   - A SLOT-BOUND refusal (relwrites.ErrSlotBoundRefused) — flips the Result to
//     IsError, exactly as the MCP path does for every relwrites failure. This is
//     not the store being unavailable; it is the platform ruling that the agent
//     may not write authority onto that instance. Swallowed, it looks precisely
//     like a write nobody attempted: the tool succeeds, the tuple never lands,
//     a later view_memory or permission Check resolves empty, and nothing
//     anywhere connects the two. That is the "mysterious denial several steps
//     later" the MCP path's own comment argues against.
//
// The divergence is therefore narrower than it was, and it is the same line
// evaluateObserves already draws on this very path: a fact that FAILED to
// record looks exactly like a fact nobody tried to record, so that post-effect
// flips too. Returning tool.Result rather than mutating is forced by the same
// thing — tool.Result is a plain value type, so a flip recorded on a copy
// inside the callee would never reach the caller's return.
//
// Nil-safe when no blocks are declared or no writer is wired.
//
// CEL bindings: result carries {"success": bool} and, for a toolspec that
// does NOT declare secretOutput, "stdoutJSON" — the call's stdout, best-effort
// JSON-parsed. Mirrors evaluateObserves' own enrichment of the same
// sandboxCELVars base; see that function's doc comment for the full
// two-level argument for why a secretOutput toolspec's captured bytes can
// never reach either post-effect's CEL vars. A secretOutput tool's Content is
// the captured secret value pre-diversion, and must never be reachable from a
// relwrites expression that could smuggle secret bytes into a SpiceDB object
// ID.
func (s *SandboxTool) evaluateWritesRelationships(ctx context.Context, res tool.Result, stdout string, argv []string, sess *tool.SessionContext) tool.Result {
	if s.opts.Spec == nil || len(s.opts.Spec.WritesRelationships) == 0 || s.relWriter == nil {
		return res
	}
	if res.IsError {
		return res
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	blocks := relwrites.BlocksFromSpec(s.opts.Spec.WritesRelationships)
	vars := sandboxCELVars(argv, res, sess)
	// Mirrors evaluateObserves: a block may read the call's own output,
	// parsed. Gated on SecretOutput exactly as that sibling is — and, like
	// it, this branch is defence in depth rather than the protection: the
	// caller clears stdout for a secretOutput toolspec before either
	// post-effect runs, so for such a spec there is nothing here to expose.
	if s.opts.Spec.SecretOutput == nil {
		result := map[string]any{"success": !res.IsError}
		var parsed any
		if err := json.Unmarshal([]byte(stdout), &parsed); err == nil {
			result["stdoutJSON"] = parsed
		} else {
			logger.Info("relwrites: stdout did not parse as JSON; result.stdoutJSON left unset",
				"tool", s.Name(), "err", err.Error())
		}
		vars["result"] = result
	}
	written, err := relwrites.Run(ctx, s.relWriter, blocks, vars, s.slotBoundChecker, func(msg string, kv ...any) {
		logger.Info(msg, append([]any{"tool", s.Name(), "session", sess.Name}, kv...)...)
	})
	// Audit what LANDED before deciding whether to report err — mirrors
	// mcp.MCPTool's identical ordering (dispatch.go) and shares its helper
	// (relwritesaudit.RecordWritten) so the two dispatch paths can never
	// diverge on what "a tuple landed" gets audited as. This is the path
	// PR-identity tuples actually use (the shipped reviewbot toolspec has no
	// MCP equivalent), and until this call existed here, every sandbox-written
	// tuple reached SpiceDB with no audit trail at all — invisible to an
	// operator, and silently corrupting pkg/steelthread/read.go's seed
	// derivation, which subtracts audited tuples from a captured session.
	if sess != nil {
		scope := memorypkg.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
		if auditErr := relwritesaudit.RecordWritten(ctx, s.mem, scope, s.Name(), written); auditErr != nil {
			logger.Info("relwritesaudit.Record", "tool", s.Name(), "session", scope.ID, "err", auditErr.Error())
		}
	}
	if err != nil {
		// A write-once (Exclusive) precondition rejection is EXPECTED, not a
		// failure: the session is already pinned and this second pin was
		// atomically refused by SpiceDB. Log it at INFO as such (the atomic
		// backstop to the Task 6 runner fast-fail — it also covers the
		// concurrent-same-turn and best-effort-status-failure cases the
		// fast-fail can't see). Any OTHER error is the only signal an operator
		// gets that a declared pin didn't land, so it stays logged. For both of
		// those the tool.Result is left unchanged — unlike the MCP path — since
		// the sandbox call itself succeeded and the agent/user already have
		// that answer.
		//
		// A SLOT-BOUND refusal is the exception, and is checked FIRST because it
		// is an authorization ruling rather than a mechanism failure: see this
		// function's doc comment. It is raised to Warn as well as surfaced,
		// because an operator reading logs wants the gate's refusals to stand
		// out from an upstream hiccup.
		switch {
		case stderrors.Is(err, relwrites.ErrSlotBoundRefused):
			logger.Warn("sandbox relwrites refused: the session holds no slot grant on that resource",
				"tool", s.Name(), "session", sess.Name, "err", err.Error())
			res.Content = fmt.Sprintf("%s: refused to register access-control relationships — this session is not bound to that resource, so a downstream permission check would deny: %v", s.Name(), err)
			res.IsError = true
			// A secretOutput toolspec's res.SecretOutput/res.Content pairing IS
			// the captured credential — the runner (loop_secretout.go) reads
			// Content unconditionally whenever SecretOutput is non-nil. Clearing
			// it here is load-bearing, not cosmetic, for exactly the reason
			// evaluateObserves states: leaving it set would publish this
			// substituted refusal text into the per-session Secret store as
			// though it were the secret the call captured.
			res.SecretOutput = nil
		case stderrors.Is(err, relwrites.ErrWriteOnceConflict):
			logger.Info("pin already exists for session; second pin rejected (write-once)",
				"tool", s.Name(), "session", sess.Name, "err", err.Error())
		default:
			logger.Info("sandbox relwrites failed", "tool", s.Name(), "session", sess.Name, "err", err.Error())
		}
	}
	return res
}

// sandboxCELVars builds the BASE CEL `vars` map shared by every post-effect
// this file evaluates against a terminal ToolCall (relwrites, observes). ONE
// builder, reused rather than re-literal'd, so the two post-effects can never
// silently diverge on the args/session half of what a sandbox call exposes
// to CEL.
//
// The shape this function itself returns — {"args":{"argv":[...]},
// "result":{"success":bool}, "session":"<ns>/<name>"} — carries no tool
// content (stdout/stderr text). Each caller then layers its own
// `result.stdoutJSON` on top, gated identically: ONLY when the toolspec does
// NOT declare secretOutput (see evaluateWritesRelationships and
// evaluateObserves, both of which build that enriched `result` map the same
// way, right after calling this function). So a secretOutput toolspec's
// captured bytes never reach either post-effect's CEL vars at two levels: the
// caller clears stdout before either post-effect runs for such a spec, and
// even if it hadn't, this builder's own base carries nothing to leak.
// observe's own secretOutput test
// (TestSandboxToolObserves_SecretOutputSeesOnlySuccess) is what pins that
// property.
func sandboxCELVars(argv []string, res tool.Result, sess *tool.SessionContext) map[string]any {
	return map[string]any{
		"args":    map[string]any{"argv": toAnySlice(argv)},
		"result":  map[string]any{"success": !res.IsError},
		"session": sess.Namespace + "/" + sess.Name,
	}
}

// evaluateObserves runs the toolspec's declared Observes blocks after
// composeResult has produced the final tool.Result, recording what a
// SUCCESSFUL result asserts as observed facts (pkg/memory/kinds/observedfact).
// Mirrors mcp.MCPTool's Execute observe post-effect (dispatch.go) — same
// "runs only after a successful call" gate, same fail-closed reporting on
// evaluate/record failure. Returns the (possibly IsError-flipped) Result;
// unlike evaluateWritesRelationships this one MUST be able to signal failure
// back to the caller, and tool.Result is a plain value type, so the mutated
// copy has to travel out via a return rather than by reference.
//
// The CEL vars start from sandboxCELVars — the same builder
// evaluateWritesRelationships uses, so `args`/`session` are identical — with
// ONE further addition specific to this post-effect: when the toolspec does
// NOT declare secretOutput, `result.stdoutJSON` carries the call's stdout,
// best-effort JSON-parsed. A toolspec declaring secretOutput skips this
// entirely, so `result` stays exactly {"success": bool} — the ONLY shape
// relwrites ever sees — which is what makes a secretOutput capture
// unreachable from an observes block. That holds at two levels, deliberately:
// the caller never passes the captured bytes in at all for such a toolspec
// (ExecuteWithIDs clears stdout), so there is nothing here to leak, and the
// SecretOutput branch below is kept anyway as defence in depth against a
// future caller that forgets. A block written assuming stdoutJSON
// against such a toolspec fails CEL evaluation (no such key) rather than
// silently recording a zero value — see
// TestSandboxToolObserves_SecretOutputSeesOnlySuccess.
//
// Nil-safe when no blocks are declared, memory isn't wired, or sess is nil.
func (s *SandboxTool) evaluateObserves(ctx context.Context, res tool.Result, stdout string, argv []string, sess *tool.SessionContext) tool.Result {
	if s.opts.Spec == nil || len(s.opts.Spec.Observes) == 0 {
		return res
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	if res.IsError {
		// A failed call asserts nothing — the same gate evaluateWritesRelationships
		// uses, for the same reason: a Succeeded ToolCall can still compose an
		// error Result (e.g. a missing secret-output file).
		return res
	}
	if s.mem == nil || sess == nil {
		// A declared block that did NOT run: distinguish "skipped" from "nothing
		// declared" in the logs — mirrors mcp.MCPTool's dispatch.go. Production
		// always wires s.mem (internal/cmd/runner's session start) and sess
		// (the runner's own dispatch), so this is a defensive branch, not an
		// expected path — which is exactly why it must log rather than fall
		// through unremarked.
		logger.Info("observe: declared blocks skipped — memory or session not wired",
			"tool", s.Name(), "memWired", s.mem != nil, "sessWired", sess != nil)
		return res
	}

	vars := sandboxCELVars(argv, res, sess)
	if s.opts.Spec.SecretOutput == nil {
		// Build the enriched result map as its own local rather than a naked
		// `vars["result"].(map[string]any)` assertion — that assertion would
		// panic if sandboxCELVars ever changed what it puts under "result"; a
		// local var makes the shape explicit and the assignment infallible.
		result := map[string]any{"success": !res.IsError}
		// Best-effort: a non-JSON or truncated (maxInlineStdout) stdout just
		// leaves stdoutJSON unset, which is a normal "no such key" CEL error for
		// a block that reaches for it — reported below like any other evaluate
		// failure, never a silently-wrong value. Logged (not just silently
		// skipped) so an operator debugging "no such key" doesn't have to
		// rediscover that the parse itself is what failed.
		var parsed any
		if err := json.Unmarshal([]byte(stdout), &parsed); err == nil {
			result["stdoutJSON"] = parsed
		} else {
			logger.Info("observe: stdout did not parse as JSON; result.stdoutJSON left unset",
				"tool", s.Name(), "err", err.Error())
		}
		vars["result"] = result
	}
	scope := memorypkg.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}

	// observedfact.Record's repeat-observation path reads the stored entry
	// back to tell an ordinary re-observation from a genuine contradiction,
	// and Local.Query checks ReadMemory unconditionally regardless of Kind
	// (facade.go:516) — unlike the append-only Put itself, which needs no
	// WriteMemory approval at all. Nothing upstream of Execute mints a
	// ReadMemory approval onto ctx for the sandbox dispatch path, so this
	// mints its own the same way mcp.MCPTool.Execute does for its own
	// "mcp:observe" call — WithSystemApproval only ADDS to ctx's approval set
	// (memory.WithApproval), so it can never revoke one the caller already
	// carried.
	recordCtx := memorypkg.WithSystemApproval(ctx, "sandbox:observe")

	for i, o := range s.opts.Spec.Observes {
		obs, err := observe.Evaluate(observeBlockFromSpec(o), vars)
		if err != nil {
			logger.Info("observe: evaluate failed", "tool", s.Name(), "block", i, "err", err.Error())
			res.Content = fmt.Sprintf("%s: could not record what this result asserts (block %d): %v", s.Name(), i, err)
			res.IsError = true
			// A secretOutput toolspec's res.SecretOutput/res.Content pairing IS
			// the captured credential — the runner (loop_secretout.go) reads
			// Content unconditionally whenever SecretOutput is non-nil. Clearing
			// it here is load-bearing, not cosmetic: leaving it set would publish
			// this substituted error string into the per-session Secret store as
			// though it were the secret the call captured.
			res.SecretOutput = nil
			return res
		}
		for _, one := range obs {
			// ToolUseID stays empty, matching the neighbouring relwritesaudit
			// convention (mcp/dispatch.go): no tool_use_id is threaded into
			// Tool.Execute anywhere in this repo today (it exists one layer up,
			// at loop_dispatch.go), so neither call site can populate one yet.
			// ToolName still lets an auditor join on WHICH tool produced the fact.
			one.Source = factcontent.Source{ToolName: s.Name(), ToolUseID: ""}
			if err := observedfact.Record(recordCtx, s.mem, scope, one); err != nil {
				logger.Info("observe: record failed", "tool", s.Name(), "block", i, "err", err.Error())
				res.Content = fmt.Sprintf("%s: could not record what this result asserts (block %d): %v", s.Name(), i, err)
				res.IsError = true
				// Same reasoning as the evaluate-failure branch above: never let a
				// post-effect failure's substituted Content masquerade as the
				// captured secret for a stdout-sourced secretOutput toolspec.
				res.SecretOutput = nil
				return res
			}
			// Log the SUCCESS too, mirroring mcp/dispatch.go line for line. An
			// `observes` block is meant to behave identically on both dispatch
			// paths, and a diagnostic present on one and missing on the other is
			// the same portability gap the `session` binding had. Names, never
			// VALUES — and here that is not merely prudence: a secretOutput
			// toolspec's stdout IS a credential, and a fact derived from it must
			// not reach a log line.
			for _, subj := range one.Subjects {
				logger.Info("observe: recorded",
					"tool", s.Name(), "block", i,
					"resourceType", subj.ResourceType, "resourceID", subj.ResourceID,
					"facts", factcontent.FactNames(one.Facts))
			}
		}
	}
	return res
}

// observeBlockFromSpec converts this file's already-parsed library type
// (spec.ObservesSpec, the far side of SpiceboxToolspecSpec.ToSpec()'s JSON
// round-trip) into observe.Block. Not the same conversion
// pkg/authz/observe/fromcrd.FromCRD collapses — FromCRD converts FROM the CRD
// type (v1alpha1.ObservesBlock), which the controllers hold; this converts
// from the toolspec library type the runner holds. Go gives no implicit
// conversion between structurally-identical named types, so calling FromCRD
// here would mean re-encoding a spec.ObservesSpec back into a CRD struct
// first — more code than the four-field copy below, not less.
func observeBlockFromSpec(o spec.ObservesSpec) observe.Block {
	subjects := make([]observe.SubjectExpr, len(o.Subjects))
	for i, subj := range o.Subjects {
		subjects[i] = observe.SubjectExpr{ResourceType: subj.ResourceType, ResourceID: subj.ResourceID}
	}
	return observe.Block{
		When:     o.When,
		ForEach:  o.ForEach,
		Subjects: subjects,
		Facts:    o.Facts,
	}
}

// toAnySlice converts a []string argv into []any so it satisfies CEL's
// dyn-typed args.argv binding used by relwrites' celEnv.
func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, v := range ss {
		out[i] = v
	}
	return out
}

func isTerminal(tc *spiceboxv1alpha1.ToolCall) bool {
	for _, c := range tc.Status.Conditions {
		if c.Status != metav1.ConditionTrue {
			continue
		}
		switch c.Type {
		case spiceboxv1alpha1.ToolCallConditionSucceeded,
			spiceboxv1alpha1.ToolCallConditionFailed,
			spiceboxv1alpha1.ToolCallConditionTimeout,
			spiceboxv1alpha1.ToolCallConditionCanceled:
			return true
		}
	}
	return false
}

// composeResult builds a tool.Result from a terminal ToolCall and stamps the
// credential-update observation carrier onto it.
//
// The carrier (Result.ExitCode / .Stderr) is the CLI counterpart of the MCP
// path's Result.HTTPStatus: a toolkit exposes no HTTP status, so the exit code
// and harvested stderr are the ONLY things the platform can independently
// observe about "was this credential rejected". Stamping happens HERE because
// this is the one place already holding both — the stderr artifact is read once,
// for the LLM-facing content, and reused.
//
// Two deliberate narrowings:
//
//   - ExitCode is carried verbatim, nil included. A ToolCall that failed before
//     the process ran reports no exit code, and synthesizing a 0 would let a
//     provider declaring `exitCodes: [0]` corroborate an unobserved failure.
//   - Stderr is carried only when the ToolCall did NOT succeed — the same path
//     that already inlines those bounded bytes into Content, so the carrier adds
//     no exposure the LLM lacks, and a platform-authored error over a SUCCEEDED
//     call never offers a healthy call's stderr up for pattern matching.
//
// OriginAuthenticated, the positive carrier, gates on toolCallSucceeded rather
// than !res.IsError. Those diverge exactly for those platform-authored errors:
// composeToolCallResult returns IsError=true from INSIDE its succeeded branch
// when a declared secret output was missing, unreadable or empty. The process
// ran and exited 0, so the credential demonstrably worked, and stamping that
// here is what lets a later call retract a stale auth-failure observation.
//
// Also returns the raw stdout bytes read for the call (pre-footer, pre-secret-
// diversion), so evaluateObserves and evaluateWritesRelationships can each
// offer a non-secretOutput toolspec's stdout to CEL as parsed JSON without
// re-reading the artifact a second time. This is NOT a new exposure:
// res.Content already carries these same bytes for a non-secretOutput
// toolspec (with an appended "[exit=...]" footer). For a secretOutput one the
// returned string IS the captured secret, so this function does not gate —
// its single caller does, discarding the value immediately rather than
// passing it on, so nothing downstream is ever holding it (ExecuteWithIDs).
func composeResult(ctx context.Context, tc *spiceboxv1alpha1.ToolCall, art ArtifactClient, secretOut *spec.SecretOutputSpec) (tool.Result, string) {
	stdout := readArtifact(ctx, art, tc.Status.StdoutArtifactRef, maxInlineStdout)
	stderr := readArtifact(ctx, art, tc.Status.StderrArtifactRef, maxInlineStderr)

	res := composeToolCallResult(ctx, tc, art, secretOut, stdout, stderr)

	// Copy rather than alias the ToolCall's pointer: the Result outlives this
	// call and travels through the runner's hook pipeline, and nothing there
	// should be able to reach back into the CR snapshot.
	if tc.Status.ExitCode != nil {
		exit := *tc.Status.ExitCode
		res.ExitCode = &exit
	}
	if toolCallSucceeded(tc) {
		res.OriginAuthenticated = true
	} else {
		res.Stderr = stderr
	}
	return res, stdout
}

// toolCallSucceeded reports whether tc carries a True Succeeded condition.
func toolCallSucceeded(tc *spiceboxv1alpha1.ToolCall) bool {
	for _, c := range tc.Status.Conditions {
		if c.Status == metav1.ConditionTrue && c.Type == spiceboxv1alpha1.ToolCallConditionSucceeded {
			return true
		}
	}
	return false
}

// composeToolCallResult builds the LLM-facing tool.Result from a terminal
// ToolCall and its already-read stdout/stderr. secretOut, when non-nil,
// controls secret-value capture:
//
//   - source "stdout": the result's Content is set to the raw stdout value and
//     SecretOutput is populated (Description from the spec) so the runner's
//     applySecretOutput diverts it to the store.
//   - source "file:<path>": the value is the FILE the producer wrote in its
//     sandbox, harvested into an OutputArtifact via CaptureOutputs. Content is
//     set to the captured file bytes and SecretOutput's Description is the
//     producer's stdout. A missing/empty captured file yields an error result.
//
// Failure results are never marked as secret output — the caller asked for the
// value only when the tool succeeded.
func composeToolCallResult(ctx context.Context, tc *spiceboxv1alpha1.ToolCall, art ArtifactClient, secretOut *spec.SecretOutputSpec, stdout, stderr string) tool.Result {
	if toolCallSucceeded(tc) {
		exit := int32(0)
		if tc.Status.ExitCode != nil {
			exit = *tc.Status.ExitCode
		}

		// When the spec declares a stdout-sourced secret output, the entire
		// stdout content IS the secret value. Return it raw in Content so
		// the runner's applySecretOutput can divert it to the store; the
		// Description and Name come from the spec, not from stdout itself.
		if secretOut != nil && secretOut.Source == "stdout" {
			return tool.Result{
				Content: stdout,
				SecretOutput: &secretout.Spec{
					Name:        secretOut.Name,
					Description: secretOut.Description,
				},
			}
		}
		// File source: the secret value is a file the producer wrote in its
		// sandbox, harvested into an OutputArtifact via CaptureOutputs (stamped
		// at ToolCall-create time). Read those bytes as the value; the
		// producer's stdout becomes the human-visible Description. A missing or
		// empty captured file means the producer failed to produce the secret —
		// surface that as an error result, never a partial/empty handle.
		if path, ok := secretOutputFilePathRaw(secretOut); ok {
			oa := matchOutputArtifact(tc.Status.OutputArtifacts, path)
			if oa == nil {
				return tool.Result{
					Content: fmt.Sprintf("secret output %q: declared file %s was not produced (no captured artifact); the tool succeeded but did not write the secret", secretOut.Name, path),
					IsError: true,
				}
			}
			value, rerr := readArtifactBytes(ctx, art, oa.ArtifactRef)
			if rerr != nil {
				return tool.Result{
					Content: fmt.Sprintf("secret output %q: reading captured file %s (artifact %s): %v", secretOut.Name, path, oa.ArtifactRef, rerr),
					IsError: true,
				}
			}
			if len(value) == 0 {
				return tool.Result{
					Content: fmt.Sprintf("secret output %q: captured file %s is empty; the tool succeeded but produced no secret bytes", secretOut.Name, path),
					IsError: true,
				}
			}
			return tool.Result{
				Content: value,
				SecretOutput: &secretout.Spec{
					Name:        secretOut.Name,
					Description: stdout,
				},
			}
		}

		var b bytes.Buffer
		b.WriteString(stdout)
		if !strings.HasSuffix(stdout, "\n") {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[exit=%d; artifacts: stdout=%s, stderr=%s]\n",
			exit, tc.Status.StdoutArtifactRef, tc.Status.StderrArtifactRef)
		return tool.Result{Content: b.String()}
	}

	// Failure path — pull the failed condition reason+message; tail stderr.
	reason, message := failedSummary(tc)
	var b bytes.Buffer
	fmt.Fprintf(&b, "ToolCall failed: %s — %s\n", reason, message)
	if stderr != "" {
		fmt.Fprintf(&b, "stderr (last %d bytes):\n%s\n", maxInlineStderr, stderr)
	}
	return tool.Result{Content: b.String(), IsError: true}
}

func failedSummary(tc *spiceboxv1alpha1.ToolCall) (string, string) {
	for _, c := range tc.Status.Conditions {
		if c.Status != metav1.ConditionTrue {
			continue
		}
		switch c.Type {
		case spiceboxv1alpha1.ToolCallConditionFailed,
			spiceboxv1alpha1.ToolCallConditionTimeout,
			spiceboxv1alpha1.ToolCallConditionCanceled:
			return c.Reason, c.Message
		}
	}
	return "Unknown", "no terminal condition"
}

// secretOutputFilePath returns the absolute file path of a file:-sourced
// secret output declared on sp, and true when sp declares one. It is the
// Spec-level convenience used at ToolCall-create time to decide whether to
// stamp CaptureOutputs.
func secretOutputFilePath(sp *spec.Spec) (string, bool) {
	if sp == nil {
		return "", false
	}
	return secretOutputFilePathRaw(sp.SecretOutput)
}

// secretOutputFilePathRaw extracts the absolute path from a file:<path> source.
// Returns ("", false) for nil specs and non-file sources (e.g. "stdout").
func secretOutputFilePathRaw(so *spec.SecretOutputSpec) (string, bool) {
	if so == nil {
		return "", false
	}
	const prefix = "file:"
	if !strings.HasPrefix(so.Source, prefix) {
		return "", false
	}
	p := strings.TrimPrefix(so.Source, prefix)
	if p == "" {
		return "", false
	}
	return p, true
}

// matchOutputArtifact finds the harvested OutputArtifact corresponding to the
// requested capture path. `tar -c <abs-path>` strips the leading '/', so the
// recorded Path is the slash-stripped form of the requested path; we compare
// against that. As a fallback (single-file captures, basename-only tar names)
// we also match on basename, and finally — since a file: secret output
// captures exactly one path — accept the sole regular-file artifact.
func matchOutputArtifact(outputs []spiceboxv1alpha1.OutputArtifact, requestedPath string) *spiceboxv1alpha1.OutputArtifact {
	if len(outputs) == 0 {
		return nil
	}
	stripped := strings.TrimPrefix(requestedPath, "/")
	base := requestedPath
	if i := strings.LastIndex(requestedPath, "/"); i >= 0 {
		base = requestedPath[i+1:]
	}
	for i := range outputs {
		if outputs[i].Path == stripped || outputs[i].Path == requestedPath {
			return &outputs[i]
		}
	}
	for i := range outputs {
		if outputs[i].Path == base {
			return &outputs[i]
		}
	}
	// A file: secret output declares exactly one capture path, so a single
	// harvested artifact is unambiguous even when tar mangled its name.
	if len(outputs) == 1 {
		return &outputs[0]
	}
	return nil
}

// readArtifactBytes reads the full artifact body (up to a generous cap) as raw
// bytes. Used for secret values, where the captured file must be returned
// verbatim (not truncated/annotated like the LLM-facing readArtifact). Unlike
// readArtifact it returns the error so the caller can distinguish a fetch
// failure from a genuinely empty file.
func readArtifactBytes(ctx context.Context, art ArtifactClient, ref string) (string, error) {
	if art == nil {
		return "", fmt.Errorf("no artifact client")
	}
	if ref == "" {
		return "", fmt.Errorf("empty artifact ref")
	}
	rc, err := art.Get(ctx, ref)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	const maxSecretBytes = 16 << 20 // mirrors harvest.go's per-file cap
	b, err := io.ReadAll(io.LimitReader(rc, maxSecretBytes))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func readArtifact(ctx context.Context, art ArtifactClient, ref string, max int) string {
	if art == nil || ref == "" {
		return ""
	}
	rc, err := art.Get(ctx, ref)
	if err != nil {
		return fmt.Sprintf("(artifact %s: %v)", ref, err)
	}
	defer rc.Close()
	limited := io.LimitReader(rc, int64(max))
	b, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Sprintf("(artifact %s: read: %v)", ref, err)
	}
	if len(b) >= max {
		return string(b) + "\n...(truncated)"
	}
	return string(b)
}

// normalizeToolCallNameSegment maps a tool_use ID into the RFC 1123 subdomain
// alphabet (lowercase letters, digits, '-'). Underscores become '-'; uppercase
// letters become lowercase; any other character becomes '-'. The result is
// trimmed to leading/trailing alphanumeric so it can safely concatenate into a
// "<sess>-<turn>-<seg>" name.
func normalizeToolCallNameSegment(s string) string {
	if s == "" {
		return "anon"
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
		default:
			b = append(b, '-')
		}
	}
	return strings.Trim(string(b), "-")
}
