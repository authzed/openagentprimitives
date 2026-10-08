package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/approval/summarizer"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// ToolAuthMode is the runner-side mirror of AgentClass.spec.authz.toolCalls.mode.
// See AgentClassSpec docs for semantics. Constants live here so the
// loop's dispatch path doesn't import the v1alpha1 types just to switch
// on string literals.
const (
	ToolAuthModeEnforcing  = "enforcing"
	ToolAuthModePermissive = "permissive"
	ToolAuthModeDisabled   = "disabled"
)

// toolAuthMode returns the configured ToolAuthMode, defaulting to
// "enforcing" when unset (matches the CRD default).
func (l *Loop) toolAuthMode() string {
	switch l.ToolAuthMode {
	case ToolAuthModePermissive, ToolAuthModeEnforcing, ToolAuthModeDisabled:
		return l.ToolAuthMode
	}
	return ToolAuthModeEnforcing
}

// summarizeToolCall calls the approval-flow summarizer LLM (see
// pkg/agent/runner/approval/summarizer) and returns a one-sentence "What this
// tool call will do" string for the approver-facing prompt.
//
// Failures (timeout, rate-limit, transport, malformed output) fall back to ""
// — approval availability MUST NOT couple to summarizer availability, and
// channelsd renders a deterministic fallback for an empty summary. Bounded by
// summarizer.DefaultTimeout so a stuck call can't keep the runner from
// publishing the approval request.
func (l *Loop) summarizeToolCall(
	ctx context.Context,
	t tool.Tool,
	argsMap map[string]any,
	permission string,
	resourceType, resourceID string,
) string {
	if l.ApprovalSummarizer == nil {
		return ""
	}
	argsJSON, err := json.Marshal(argsMap)
	if err != nil {
		return ""
	}
	schemaJSON, _ := json.Marshal(t.InputSchema())
	req := summarizer.Request{
		Tool:            t.Name(),
		ToolDescription: t.Description(),
		InputSchemaJSON: string(schemaJSON),
		ArgsJSON:        string(argsJSON),
		ResourceType:    resourceType,
		ResourceID:      resourceID,
		Permission:      permission,
	}
	timeout := summarizer.DefaultTimeout
	if l.ApprovalSummarizerTimeout > 0 {
		timeout = l.ApprovalSummarizerTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := l.ApprovalSummarizer.Summarize(cctx, req)
	if err != nil {
		// No-silent-errors: log so an operator can correlate cache
		// misses with API/transport failures. Approval still goes
		// out — that's the whole point of the fallback.
		ctrllog.FromContext(ctx).Info("approval summarizer failed; using fallback",
			"tool", t.Name(), "provider", l.ApprovalSummarizer.Name(),
			"err", err.Error())
		return ""
	}
	return out
}

// containPhase records how a contained tool call terminated so the caller can
// do its own bookkeeping (deny counters, halt recording, lifecycle mapping)
// without re-deriving the pipeline logic. dispatchToolUses (the LLM loop) and
// the future proxy-exec (a browser widget calling an MCP app-tool out of the
// loop) both branch on the same phases.
type containPhase int

const (
	// containRanOK: the call ran Pre → Execute → Post with no gate denial or
	// halt (a user-interrupt cancellation also lands here — the synthesized
	// notice is not a failure). The returned Result is the (possibly
	// Post-overwritten) output the caller feeds back.
	containRanOK containPhase = iota
	// containPreErr: the PreToolCall executor errored (host-primitive failure
	// the executor could not turn into a verdict). Failed CLOSED — the tool did
	// NOT run; the Result is an IsError notice surfacing the cause.
	containPreErr
	// containPreDeny: a PreToolCall hook denied. The tool did NOT run; the
	// Result carries the (approval-aware) deny reason. Caller counts a pre-deny.
	containPreDeny
	// containPreHalt: a PreToolCall hook halted. Unless suppressHalt was set,
	// host.Halt already wrote the terminal Failed status inside the executor
	// (suppressHalt makes it record-only — see executeToolContained). Caller
	// records the halt.
	containPreHalt
	// containPostErr: the PostToolCall executor errored. Failed CLOSED — the
	// tool ran but its result is withheld and replaced with an IsError notice.
	containPostErr
	// containPostDeny: a PostToolCall hook denied. The tool ran but its result
	// is overwritten with the deny reason. Caller counts a post-deny.
	containPostDeny
	// containPostHalt: a PostToolCall hook halted. Caller records the halt.
	containPostHalt
)

// String renders the phase as a stable slug for structured logs (e.g. the
// detached app-tool exec's completion log). Not user-facing.
func (p containPhase) String() string {
	switch p {
	case containRanOK:
		return "ran_ok"
	case containPreErr:
		return "pre_err"
	case containPreDeny:
		return "pre_deny"
	case containPreHalt:
		return "pre_halt"
	case containPostErr:
		return "post_err"
	case containPostDeny:
		return "post_deny"
	case containPostHalt:
		return "post_halt"
	default:
		return "unknown"
	}
}

// containedOutcome pairs the termination phase with the deny/halt reason the
// caller needs for halt recording (the Result already carries the user-facing
// text for deny/error phases).
type containedOutcome struct {
	Phase  containPhase
	Reason string
	// Definition is the deciding hook's pipeline.Outcome.Definition: non-nil
	// when this deny was caused by the agent's own definition being
	// unevaluatable rather than by a policy refusing the call. It changes what
	// the caller is told (a broken view, not a forbidden one) and, once per
	// distinct fault, sends the details to the monitoring channel.
	Definition error
}

// executeToolRaw runs a single tool's Execute with the two framework overrides
// the contained (gated) and ungated meta paths share: the dispatch-error
// wrapper (a Go error from Execute becomes framework-generated, Trusted retry
// guidance — NOT tool output) and the user-Interrupt override. Returns the
// result and whether an Interrupt cancelled the call, which the caller uses to
// skip post-processing the cancelled context would otherwise fail closed on.
func (l *Loop) executeToolRaw(ctx context.Context, t tool.Tool, name string, args json.RawMessage, sess *tool.SessionContext) (tool.Result, bool) {
	res, err := t.Execute(ctx, args, sess)
	if err != nil {
		// A Go error from Execute means the runner-side dispatch itself failed,
		// not the tool's own validation. Wrap with the tool name so the model
		// knows which call to adjust. Framework-generated retry guidance, not
		// tool output — Trusted so the ungated path's content-guard does not
		// inspect it, which is safe because Trusted is only consulted for meta
		// tools.
		res = tool.Result{
			Content: fmt.Sprintf("tool %q dispatch failed: %v. This is typically transient — retry the same call once; if it fails again, mention the failure in your final summary and continue with what you have.",
				name, err),
			IsError: true,
			Trusted: true,
		}
	}
	// A user-initiated Interrupt fired while this call was in flight: override
	// whatever Execute returned (success, its own error content, or the
	// dispatch-failed wrapper above — any of the three, depending on how the
	// tool observes ctx cancellation) with the synthesized cancellation notice.
	// Checked unconditionally so it applies on both the err and non-err paths.
	if context.Cause(ctx) == errInterruptedByUser {
		return tool.Result{
			Content: "This tool call was canceled by the user before it finished; a newer user message follows.",
			IsError: false,
			Trusted: true,
		}, true
	}
	return res, false
}

// pinRefusalApplies reports whether attaching ref's explanation to a denial of
// this call is truthful. It is false for the one case where the explanation
// would MISLABEL the denial: the call resolved to the PINNED instance itself,
// so some other gate (an approval refusal, a scope rule) denied it, and
// appending "this session is pinned to <that same instance>; propose a plan to
// move" would send the model chasing a move it does not need.
//
// The id is resolved with authz.ResolveResourceID — the same exported resolver
// the backend checker dispatches through (template, CEL expr, and transforms),
// over the same unwrapped argsMap — so the comparison cannot drift from what
// the check itself named. Fails OPEN to attaching: with no pinned id recorded
// (ReadPin unavailable at record time) or an id that will not resolve (that
// denial carries its own unresolved-resource message, and the pin context is
// still true of the type), the explanation is kept rather than guessed away.
func pinRefusalApplies(ref slotPinRefusal, check authz.PermissionCheck, args map[string]any) bool {
	if ref.PinnedID == "" {
		return true
	}
	id, err := authz.ResolveResourceID(check, args)
	if err != nil || id == "" {
		return true
	}
	return id != ref.PinnedID
}

// withPinRefusalExplanation appends a recorded single-occupancy pin refusal to
// a denied tool result, so the model reads WHY the instance it named could not
// be used — the refusal text names the pinned instance and the route out. The
// base denial is kept and the explanation added after it: the refusal explains
// the denial, it does not replace the authorization record behind it.
// Idempotent — a refusal already present (a retry of the same denied call) is
// not appended twice.
func withPinRefusalExplanation(res tool.Result, refusal string) tool.Result {
	if refusal == "" || strings.Contains(res.Content, refusal) {
		return res
	}
	if res.Content == "" {
		res.Content = refusal
	} else {
		res.Content += "\n\n" + refusal
	}
	res.IsError = true
	return res
}

// approvalObservation reports one observable point of a single contained
// call's approval. It exists so a browser surface can render the lifecycle
// the design spec names; nothing about the approval flow itself changes.
type approvalObservation struct {
	// Asked is true for the ask-published observation. AddressedToViewer is
	// meaningful ONLY then.
	Asked bool
	// AddressedToViewer reports whether the resolved approver set contains the
	// principal that requested this call — the design spec's approval table
	// ("approver IS the viewer" vs "is someone else") reduced to one bool.
	// Deliberately a bool and not an identity: the browser must learn whether
	// it can act, never who else can.
	AddressedToViewer bool

	// Resolved is true for the await-returned observation.
	Resolved bool
	Approved bool
	// TimedOut distinguishes a lapsed deadline from a decision. The executor
	// turns both into a Deny verdict, so this is the ONLY place the two are
	// still distinguishable.
	TimedOut bool
}

// observeAuthFailure feeds one tool call's raw outcome to the credential-update
// corroboration recorder: an auth-SHAPED failure is recorded against the
// origin's provider, and a call the origin AUTHENTICATED — error or not —
// clears any observation it carries, since a credential that just worked is not
// one that needs replacing. Only origin-bearing tools participate; meta and
// toolkit-less sandbox tools have no credential behind them.
//
// It must stay BELOW the canceledByInterrupt early return in
// executeToolContained: a cancelled call is not proof the credential works, so
// it leaves any existing observation in place rather than retracting it.
//
// The observation comes from tool.Result's out-of-band carrier fields, each
// with an unambiguous "not observed" value credupdate.IsAuthShaped refuses to
// match on: HTTPStatus for MCP origins (written only by pkg/agent/tool/mcp's
// dispatch; zero means none observed, so a transport failure cannot counterfeit
// one), ExitCode/Stderr for CLI/toolkit origins, which have no HTTP status
// (pkg/agent/tool/sandbox), and the POSITIVE OriginAuthenticated from both —
// the origin's auth layer accepted the credential and ran the tool, so any
// error is the tool's opinion of the arguments. All are passed on EVERY failure
// whatever the tool kind: which can be set is a property of the producing tool,
// and the provider's authFailure: block decides which counts. The recorder, not
// this hook, reads OriginAuthenticated to choose recording vs retracting.
func (l *Loop) observeAuthFailure(ctx context.Context, t tool.Tool, res tool.Result) {
	ot, ok := t.(tool.OriginTool)
	if !ok {
		return
	}
	origin := ot.Origin()
	if origin == "" {
		return
	}
	var err error
	if res.IsError {
		err = l.AuthFailures.ObserveFailure(ctx, origin, credupdate.Observation{
			HTTPStatus:          res.HTTPStatus,
			ExitCode:            res.ExitCode,
			Stderr:              res.Stderr,
			OriginAuthenticated: res.OriginAuthenticated,
		})
	} else {
		err = l.AuthFailures.Clear(ctx, origin)
	}
	if err != nil {
		// Never fail the tool call over this: the observation is advisory
		// corroboration for a later human decision, not part of the call's
		// result. But never drop it silently either — a recorder that cannot
		// write means credential-update requests quietly lose corroboration.
		slog.Default().Info("recording credential auth-failure observation failed",
			"origin", origin, "tool", t.Name(), "isError", res.IsError, "err", err.Error())
	}
}

// containParams carries the per-call authz + attribution knobs
// executeToolContained needs beyond the tool + args. The zero value is the LLM
// loop's behavior (session subject, no halt suppression, no proxy attribution).
type containParams struct {
	// requester is the principal the authz hooks evaluate AND attribute the
	// approval to: l.authSubject for the LLM loop, the widget VIEWER for a
	// proxy-exec (app-tool call).
	requester identity.CanonicalUserID
	// subjects is the check subject-list (authz.Inputs.Subjects): l.authSubjects
	// for the LLM loop, []string{requester} for a proxy-exec. Threading it here
	// (rather than reading l.authSubjects in BuildInputs) keeps a detached
	// proxy-exec off the loop-mutated field — no race with advanceRequester.
	subjects []string
	// suppressHalt mirrors the SessionStart executor's host.suppressHaltWrite
	// model (cold-start site / host.go's doc comment): when true, a Halt verdict
	// from either pipeline point is RECORD-ONLY — host.Halt does NOT call l.fail
	// (the terminal-status write). For a detached (off-loop-goroutine) caller,
	// where an authz-hook Halt must fail the widget call, not terminalize the
	// agent session out from under the loop goroutine.
	suppressHalt bool
	// proxyExec marks a viewer-attributed proxy execution (app-tool call): the
	// approval-card Requester derives from `requester` (the viewer, D-D1) rather
	// than the session's LastInboundExternalID. false on the LLM path.
	proxyExec bool
	// uiDataBinding marks a proxy-exec whose result is bound for the BROWSER,
	// not the model: an agent-UI data binding (Loop.HandleUIDataBinding),
	// resolved server-side under the viewer's subject and never appended to the
	// transcript. Threaded onto pipeline.Input.UIDataBinding so toolguard's
	// GuardRecord meters the result against the UI ingress ceiling
	// (IngressLimitFor) rather than the model-sized one. false on every other
	// path, ordinary mcp-ui widget app-tool calls included.
	uiDataBinding bool

	// approvalEvent, when non-nil, receives every observable point of THIS
	// call's approval. A FUNC, never an interface: comparing a func value to nil
	// is honest, whereas an interface field assigned a typed-nil pointer reports
	// non-nil and panics on first call (AGENTS.md's typed-nil rule; see also
	// pkg/web/uibindings.Deps' NATSRequest).
	//
	// Invoked SYNCHRONOUSLY on the goroutine running executeToolContained, so a
	// closure it captures needs no synchronization of its own.
	approvalEvent func(approvalObservation)
}

// executeToolContained runs ONE tool call through the full containment
// pipeline — PreToolCall → Execute → PostToolCall — with the exact fail-closed
// error handling, Deny/Halt semantics, dispatch-error wrapping, and
// user-interrupt override dispatchToolUses uses for its gated tools. It builds
// ONE runner host and uses that same host for both Pre and Post so the
// approval-deny-reason handoff (takeApprovalDenyReason) is shared within the
// call. Returns the (possibly Post-overwritten) Result and an outcome the
// caller maps to its own deny counters / halt recording.
//
// The per-call principal + authz/attribution knobs travel in p (see
// containParams), whose zero value is the LLM loop's behavior: session subject,
// no halt suppression, no proxy attribution. It deliberately does NOT compute
// the gate predicate (the caller decides whether to contain), touch
// dispatch-level counters or the results slice, or run the meta content-guard
// (that is the ungated path's job).
func (l *Loop) executeToolContained(
	ctx context.Context, sess *tool.SessionContext, t tool.Tool,
	name string, args json.RawMessage, useID, justification string,
	p containParams,
) (tool.Result, containedOutcome) {
	// The toolguard half-open breaker probe is a claim taken at PreToolCall and
	// handed back at PostToolCall — but a Pre Deny (including a denied or
	// timed-out approval), a Pre Halt, and a user interrupt all return below
	// without ever reaching PostToolCall, and half-open has no cool-off timer to
	// undo a stranded claim. One ledger per contained call, released on every
	// exit including a panic, is what keeps that leak unrepresentable.
	ctx, releaseProbes := toolguard.WithProbeLedger(ctx)
	defer releaseProbes()

	host := newRunnerHost(l, hostSession{
		Namespace: sess.Namespace,
		Name:      sess.Name,
		Class:     l.AgentName,
	})
	host.suppressHaltWrite = p.suppressHalt
	// D-D3: a proxy-exec (widget app-tool) approval await must not freeze the
	// live turn's RunClock/progress/plan-card — see AwaitDecision's proxyExec
	// gate in host_approval.go.
	host.proxyExec = p.proxyExec
	host.approvalEvent = p.approvalEvent

	preIn := pipeline.Input{
		Session: pipeline.SessionRef{
			Namespace: sess.Namespace,
			Name:      sess.Name,
			Class:     l.AgentName,
		},
		Requester:     p.requester,
		Subjects:      p.subjects,
		ProxyExec:     p.proxyExec,
		UIDataBinding: p.uiDataBinding,
		Tool: &pipeline.ToolCallInfo{
			Name:          name,
			Args:          args,
			UseID:         useID,
			Justification: justification,
			GovernedMeta:  planGateGoverned(t),
		},
	}
	preOut, preErr := l.executor().Run(ctx, pipeline.PreToolCall, preIn, host)
	if preErr != nil {
		// A non-nil error is a host-primitive failure the executor could not
		// turn into a verdict; the returned Verdict is the zero value (Allow).
		// Treat it as fail-CLOSED — do NOT run the tool — and surface the cause,
		// rather than silently executing.
		slog.Default().Info("PreToolCall executor errored; failing closed (tool not run)",
			"session", sess.Namespace+"/"+sess.Name, "tool", name, "err", preErr.Error())
		return tool.Result{
			Content: fmt.Sprintf("pre-tool-call gate failed for %q: %v (tool not executed)", name, preErr),
			IsError: true,
		}, containedOutcome{Phase: containPreErr}
	}
	switch preOut.Verdict {
	case pipeline.Deny:
		// An approval-flow denial carries a richer, anti-confabulation message
		// (SYSTEM_TIMEOUT / SYSTEM_DECISION_DENIED naming the approver) than the
		// executor's generic "approval denied or timed out"; prefer it when the
		// host captured one.
		reason := preOut.Reason
		if denyMsg := host.takeApprovalDenyReason(); denyMsg != "" {
			reason = denyMsg
		}
		l.reportDefinitionError(sess, name, preOut)
		return tool.Result{Content: reason, IsError: true}, containedOutcome{Phase: containPreDeny, Reason: reason, Definition: preOut.Definition}
	case pipeline.Halt:
		// Unless suppressHalt is set, host.Halt already wrote the terminal
		// Failed status inside the executor; the caller records the halt so
		// the loop stops after this dispatch. With suppressHalt, host.Halt was
		// record-only (no l.fail) — the caller still records the halt to fail
		// THIS call, without terminalizing the session.
		return tool.Result{Content: preOut.Reason, IsError: true}, containedOutcome{Phase: containPreHalt, Reason: preOut.Reason}
	}

	// Attribute this call to an operation, so the audit graph is TOTAL — meta
	// tools included. Only for calls that did NOT name one: those that did are
	// recorded by their own dispatcher, and recording here too would
	// double-count every MCP and sandbox call.
	//
	// After the gates and before the run, matching where the dispatchers
	// record: a call the gate denied never executed (the Deny/Halt returns
	// above), and its refusal is already in the append-only authz-decision log
	// rather than here.
	if sess.Operations != nil && explicitOperationID(args) == "" {
		if opID := resolveAmbientOperation(sess, args); opID != "" {
			if idx, ok := sess.Operations.RecordCall(opID, tool.OperationCall{
				Tool:   name,
				Reason: justification,
				At:     time.Now(),
			}); ok {
				defer sess.Operations.CompleteCall(opID, idx)
			}
		}
	}

	// The tool must not see pt markup: strip any pt-untrusted envelope from the
	// args before the tool runs. Inert (args unchanged) when the args carry no
	// envelope.
	args = stripArgTags(args)

	// Re-check revocation IMMEDIATELY before the call runs.
	//
	// The revocation guard is a PreToolCall hook, and an approval await sits
	// between that hook and this line — so an operator revoking an origin while
	// a human is looking at the card had their revocation land, be recorded,
	// and then be ignored by the very call it was aimed at. The window is
	// bounded only by the approval timeout, which is hours.
	//
	// It is a live read of an in-process set, so this costs nothing and closes
	// the whole gap rather than just the approval leg: anything that delays a
	// call between the gates and the run is covered by the same line.
	if origin, revoked := l.originRevokedNow(name); revoked {
		reason := "revoked: " + origin
		slog.Default().Info("revocation: tool denied at dispatch; origin was revoked after the pre-call gates",
			"session", sess.Namespace+"/"+sess.Name, "tool", name, "origin", origin)
		return tool.Result{Content: reason, IsError: true},
			containedOutcome{Phase: containPreDeny, Reason: reason}
	}

	res, canceledByInterrupt := l.executeToolRaw(ctx, t, name, args, sess)

	// Skip PostToolCall for a user-cancelled call: it produced no real tool
	// output, so there is no resource to scope/leak-check — and running these
	// gates on the already-cancelled ctx would error (context.Canceled), fail
	// closed, and flip the synthesized IsError:false cancellation back to
	// IsError:true, breaking the "a preemption is not a tool failure" contract.
	if canceledByInterrupt {
		return res, containedOutcome{Phase: containRanOK}
	}

	// Credential-update corroboration, observed HERE and not after the Post
	// gates: this is the last point at which res is the RAW upstream outcome.
	// A PostToolCall Deny/Halt replaces res with a platform-authored IsError,
	// which would turn a successful upstream call into a fabricated "auth
	// failure" observation; and both Pre verdicts return above without ever
	// running the tool, so no observation is owed for them either.
	l.observeAuthFailure(ctx, t, res)

	// PostToolCall: toolguard outcome recording (all results), then scope
	// hard-deny + info-leakage gates (successful results only — those hooks
	// early-return on IsError). On Deny/Halt the result is overwritten with
	// IsError so the disallowed/leaked resource never reaches the caller.
	postIn := pipeline.Input{
		Session: pipeline.SessionRef{
			Namespace: sess.Namespace,
			Name:      sess.Name,
			Class:     l.AgentName,
		},
		Requester:     p.requester,
		Subjects:      p.subjects,
		ProxyExec:     p.proxyExec,
		UIDataBinding: p.uiDataBinding,
		Tool: &pipeline.ToolCallInfo{
			Name:         name,
			Args:         args,
			UseID:        useID,
			Result:       res.Content,
			IsError:      res.IsError,
			GovernedMeta: planGateGoverned(t),
			// The credential-halt observation the tool guard reads. Carried off
			// the RAW Execute result, like IsError beside it, so a later
			// PostToolCall verdict rewriting res cannot manufacture or erase
			// one.
			UnbilledFailure: res.UnbilledFailure,
		},
	}
	postOut, postErr := l.executor().Run(ctx, pipeline.PostToolCall, postIn, host)
	outcome := containedOutcome{Phase: containRanOK}
	if postErr != nil {
		// PostToolCall couldn't reach a verdict (host-primitive failure; Verdict
		// is the zero-value Allow). The PostToolCall gates include the
		// info-leakage check — failing open would let an unvetted result reach
		// the caller. Fail CLOSED: replace the result with an IsError so the raw
		// output is withheld.
		slog.Default().Info("PostToolCall executor errored; withholding tool result (fail-closed)",
			"session", sess.Namespace+"/"+sess.Name, "tool", name, "err", postErr.Error())
		res = tool.Result{
			Content: fmt.Sprintf("post-tool-call gate failed for %q: %v (result withheld)", name, postErr),
			IsError: true,
		}
		outcome = containedOutcome{Phase: containPostErr}
	}
	switch postOut.Verdict {
	case pipeline.Deny:
		l.reportDefinitionError(sess, name, postOut)
		res = tool.Result{Content: postOut.Reason, IsError: true}
		outcome = containedOutcome{Phase: containPostDeny, Reason: postOut.Reason, Definition: postOut.Definition}
	case pipeline.Halt:
		// Unless suppressHalt is set, host.Halt already wrote terminal Failed;
		// the caller records the halt. With suppressHalt, the write was
		// suppressed (record-only) — see the PreToolCall Halt case above.
		res = tool.Result{Content: postOut.Reason, IsError: true}
		outcome = containedOutcome{Phase: containPostHalt, Reason: postOut.Reason}
	}
	return res, outcome
}

// dispatchToolUses runs each tool_use in its own goroutine, preserving the
// input ordering for the response slice. Tool execution errors are returned as
// IsError results (not Go errors) so the model can self-correct; budget caps
// handle infinite loops. delivered is the set of tool_use IDs already
// dispatched in prior loop invocations (channel-attached resume path); those
// entries are short-circuited without re-executing the tool.
func (l *Loop) dispatchToolUses(ctx context.Context, uses []llm.ToolUseBlock, sess *tool.SessionContext, turnIndex int32, memTurnIndex int, delivered map[string]bool, lastAssistantBlocks []llm.ContentBlock) ([]tool.Result, bool) {
	// Capture any update_status text from this turn into Loop state so the next
	// turn's gated tool_use can use it as a cross-turn justification fallback.
	// Models commonly call update_status, read the result, then emit the gated
	// tool_use in a new turn, whose blocks contain only the tool_use — without
	// this capture the approval *Why* line would be empty.
	l.captureStatusFromUses(uses)

	// No cold-start wait here: Run drives the cold-start flow at the
	// initial-prompt site (the SessionStart executor's ColdStartScope hook),
	// blocking before turn 0 is placed. By the time any tool is dispatched the
	// decision has already resolved or failed open.

	byName := map[string]tool.Tool{}
	for _, t := range l.Tools {
		byName[t.Name()] = t
	}
	results := make([]tool.Result, len(uses))
	// Reset the per-dispatch halt flag (set after wg.Wait below, read by Run).
	l.dispatchHalted = false
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		anyTerm bool
		// Hook outcomes harvested under mu for translation into lifecycle
		// transitions after wg.Wait (off the dispatch goroutines). preDeny/
		// postDeny count hook denials at each point; halted/haltReason record
		// the first Halt verdict.
		preDeny    int
		postDeny   int
		halted     bool
		haltReason string
	)
	recordHalt := func(reason string) {
		mu.Lock()
		defer mu.Unlock()
		if !halted {
			halted, haltReason = true, reason
		}
	}
	for i := range uses {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Skip tool_uses that were already dispatched and delivered in a
			// prior runner invocation (channel-attached resume dedup).
			if delivered[uses[i].ID] {
				// A respond_to_user skipped here DID reach the user before the
				// restart, so it still counts as this round's delivery. Without
				// this the delivery guard would treat the round as silent and
				// make the model re-send a message the user already has.
				if uses[i].Name == "respond_to_user" {
					l.markDelivered()
				}
				results[i] = tool.Result{Content: "(already delivered; skipped on resume)", IsError: false}
				return
			}
			t, ok := byName[uses[i].Name]
			if !ok {
				// Help the model self-correct by listing what IS available.
				avail := make([]string, 0, len(byName))
				for n := range byName {
					avail = append(avail, n)
				}
				sort.Strings(avail)
				results[i] = tool.Result{
					Content: fmt.Sprintf("unknown tool %q. Available tools in this session: %s. Pick one of the above (names are case-sensitive) and retry; do not invent tool names.",
						uses[i].Name, strings.Join(avail, ", ")),
					IsError: true,
				}
				return
			}
			// Delivery guard: refuse to run a round-terminating tool that would
			// end this round having sent the user nothing, stranding the model's
			// answer in assistant prose. Short-circuited BEFORE Execute, not
			// corrected after — await_user_message blocks and
			// agent_work_complete submits the terminal result, so once either
			// runs the silence is unrecoverable. IsError feeds the correction
			// back as a tool_result the model can act on.
			_, respondAvailable := byName["respond_to_user"]
			if needsDeliveryNudge(l.ChannelAttached, respondAvailable, l.hasDeliveredThisRound(), lastAssistantBlocks, uses[i].Name) {
				slog.Default().Info("delivery guard: blocked round-terminating tool with nothing delivered to the user",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"tool", uses[i].Name, "toolUseID", uses[i].ID)
				// Status is a *StatusPatcher whose methods dereference it, so the
				// nil check is load-bearing, not defensive noise: the runner
				// always sets it, but dispatch-level tests construct a Loop
				// without one and blocking here must not panic them.
				if l.Status != nil {
					besteffort.Log(slog.Default().Info, "AppendRunnerNote (delivery guard)",
						l.Status.AppendRunnerNote(ctx, "delivery guard: blocked "+uses[i].Name+" — nothing was delivered to the user this round"),
						"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
				}
				results[i] = tool.Result{Content: DeliveryNudgeMessage, IsError: true, Trusted: true}
				return
			}

			callCtx := sandbox.WithIDs(ctx, sandbox.IDs{
				TurnIndex:    int(turnIndex),
				ToolUseID:    uses[i].ID,
				BlockIndex:   i + 1, // +1: SeqBlockStart(0) is reserved for the turn-start signal
				MemTurnIndex: memTurnIndex,
				SessionUID:   string(sess.AgentSessionUID),
			})
			// Register this call's cancel func so a user-initiated Interrupt can
			// cancel it in flight. Deregister + release (nil cause = normal
			// completion, does not clobber a cause already set by fire) on every
			// exit from this goroutine, including the early returns below.
			callCtx, cancelCall := context.WithCancelCause(callCtx)
			l.interrupts.registerTool(uses[i].ID, t, cancelCall)
			defer func() {
				l.interrupts.deregisterTool(uses[i].ID)
				cancelCall(nil)
			}()
			// Attach InteractiveHooks so interactive sandbox tools can bridge
			// their gateway stream to the channel. nil hooks → interactive
			// tools fail with a clear "channel not attached" message.
			if l.InteractiveHooks != nil {
				callCtx = sandbox.WithInteractiveHooks(callCtx, *l.InteractiveHooks)
			}
			// Inject a per-dispatch tool-progress hook so a running SYNC sandbox
			// tool can emit throttled liveness ticks while the LLM is blocked
			// awaiting it. Stamps CallID/Name into every update the sandbox tool's
			// poll loop produces via this closure.
			if l.OnToolProgress != nil {
				callID := uses[i].ID
				name := t.Name()
				callCtx = sandbox.WithToolProgressHook(callCtx, sandbox.ToolProgressHook{
					Emit: func(ctx context.Context, u sandbox.ToolProgressUpdate) {
						u.CallID = callID
						u.Name = name
						l.OnToolProgress(ctx, u)
					},
				})
			}
			// Auto-touch the channelsd watchdog before dispatching any
			// long-running tool. Sandbox/MCP calls can take tens of seconds;
			// without this hint a chatty agent that forgets update_status trips
			// the 30s warn while making real progress. Meta tools are skipped —
			// in-process, and either trivial or signalling on their own.
			if l.OnToolDispatch != nil {
				switch t.Kind() {
				case tool.KindSandbox, tool.KindMCP:
					l.OnToolDispatch(callCtx, t.Name(), t.Kind())
				}
			}
			// Local-mode progress logging: every tool dispatch shows up
			// on stderr so the user sees what the agent is doing rather
			// than a long silent pause. Channel-attached agents use
			// update_status / Notify for the same purpose.
			l.progressf("→ %s", uses[i].Name)

			// Flag-gated autofill (AP_BINDING_AUTOFILL, default off) from session
			// bindings, BEFORE the per-tool Check: the Check sees the filled args
			// (so the synthetic {repo} → "foo/bar" resolution matches the
			// autofilled value) and Execute sees the same JSON downstream.
			if l.BindingAutofillEnabled && l.AgentClass != nil &&
				len(l.AgentClass.Spec.GetSlots()) > 0 {
				specs, specErr := boundEntitySpecsForAutofill(l.AgentClass)
				if specErr != nil {
					// A class whose slot preconditions do not compile gets no
					// autofill and no extracted-slot promotion. Skipping both is
					// fail-closed — the per-tool Check below still gates the
					// call — but it must be said out loud, because a slot that
					// never binds otherwise looks like a user who never named an
					// instance.
					slog.Default().Info("binding autofill skipped: the class's slot preconditions did not compile",
						"tool", t.Name(), "err", specErr.Error())
					specs = nil
				}
				needed := autofillTypesForTool(l.AgentClass, t.Name())
				if len(needed) > 0 && l.CurrentInboxIdx >= 0 {
					waitCtx, cancel := context.WithTimeout(callCtx, l.BindingAutofillDeadline)
					// Error intentionally ignored: a deadline expiry is the
					// documented fallback to the raw-args path below, and
					// FillToolArgs (next call) logs its own errors with
					// context. Acting on the wait error here would mask
					// the more informative downstream message.
					_ = l.Engine.WaitForExtraction(waitCtx, l.bindingScope(), l.CurrentInboxIdx, l.BindingAutofillDeadline)
					cancel()

					// The candidates the wait was for are PROPOSALS, not
					// bindings: authzd records what the extractor saw in the
					// user's text and decides nothing. Promoting them here —
					// after re-deriving each against the class's declarations
					// and Checking it for the requester — is what turns them
					// into grants, and what makes the wait above mean
					// something. Advisory: on failure the args stay unfilled
					// and the per-tool Check below still gates the call.
					if perr := l.Engine.PromoteExtractedSlots(callCtx, l.bindingScope(), l.authzSessionRef(),
						specs, l.AuthSubject(), l.CurrentInboxIdx); perr != nil {
						if errors.Is(perr, authz.ErrSlotPinned) {
							// An authorization outcome, not a hiccup: a
							// single-occupancy pin refused this instance. Warn
							// (louder than a mechanism error), keep the Info-trail
							// grep key below for an operator, and record the refusal
							// so the per-tool Check's denial on this type explains
							// itself to the model rather than reading as a bare
							// "permission denied".
							slog.Default().Warn("promoting extracted slot candidates refused by a single-occupancy pin; surfacing to the model",
								"tool", t.Name(), "turn", l.CurrentInboxIdx, "err", perr.Error())
							l.recordSlotPinRefusal(callCtx, specs, perr.Error())
						} else {
							slog.Default().Info("promoting extracted slot candidates failed; continuing unbound",
								"tool", t.Name(), "turn", l.CurrentInboxIdx, "err", perr.Error())
						}
					} else {
						// A promotion that succeeded supersedes any recorded
						// refusal for these types: the facts that refused no
						// longer hold, and a stale "pinned to A" explanation
						// must not be appended to a later denial.
						l.clearSlotPinRefusalsForSpecs(specs)
						// Mirror any pin the promotion just produced onto
						// status.slotPins — display-only, read back from SpiceDB
						// per not-yet-mirrored single-occupancy type (see
						// slot_pin_mirror.go).
						l.mirrorBoundSlotPins(callCtx, specs)
					}
				}
				filled, ferr := l.Engine.FillToolArgs(callCtx, l.authzSessionRef(),
					specs, t.Name(), uses[i].Input)
				if ferr != nil {
					slog.Default().Info("autofill errored; falling back to raw args",
						"tool", t.Name(), "err", ferr.Error())
				} else if string(filled) != string(uses[i].Input) {
					uses[i].Input = filled
				}
			}

			// Per-tool SpiceDB permission check. Runs before Execute so
			// a denied tool never reaches the sandbox or MCP server.
			// authz.Check returns OutcomeDenied when Cli is nil and the
			// StateImpact requires a SpiceDB round-trip, so a missing
			// client surfaces as a deny rather than a bypass.
			var argsMap map[string]any
			if jerr := json.Unmarshal(uses[i].Input, &argsMap); jerr != nil {
				// Best-effort: bad JSON falls through to authz.Check, which
				// will either fail variant resolution (CEL on nil args) or
				// resource-ID template resolution, in either case denying.
				// Log so an operator can spot a malformed-args bug instead
				// of being left with only a generic permission-denied trail.
				slog.Default().Info("tool args JSON unmarshal failed; continuing to authz with nil args",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"tool", t.Name(),
					"err", jerr.Error(),
				)
			}

			// Tools that go through tool.WrapInputSchema (every MCP /
			// sandbox tool) emit args wrapped in {operation_id, _reason,
			// args}. Authz CEL in MCPServer CRs is written against the
			// INNER args, so unwrap here before variant/check evaluation.
			// Meta tools have flat schemas and are passed through as-is.
			argsMap = unwrapToolArgs(argsMap)

			// Resolve the effective Permission ONCE via ResolvePermissionForArgs
			// (permission_resolve.go — shared with the ToolCallAuthz hook's
			// ResolvePermission closure and the agent-UI readonly gate, so the
			// first-match-variant-else-fallback rule cannot drift between the
			// three dispatch paths). Every downstream consumer (ToolCallAuthz,
			// log labels, post-approval re-check) MUST see this same Permission;
			// otherwise approval envelopes carry the fallback's
			// resourceType/permission/grantBindsArgs and the post-approval retry
			// checks the wrong tuple.
			//
			// FAIL-CLOSED: a malformed variant (compile or eval error) means we
			// cannot tell which Permission applies, so the call is denied and the
			// error surfaced to the LLM as an IsError tool_result. Falling back
			// to the tool's top-level Permission would be a silent bypass.
			// permissionForTool, not ResolvePermissionForArgs: a sandbox tool
			// resolves its own per-call permission with the toolkit's parser,
			// and only that answer agrees with the one ToolCallAuthz uses. The
			// CEL-variant path under-reports (an argument-level variant carries
			// the subcommand predicate; the predicate cannot skip a global
			// flag's value), and this value decides whether a stateful call
			// gets a pre-dispatch snapshot at all.
			resolvedPerm, rerr := l.permissionForTool(t, argsMap)
			if rerr != nil {
				results[i] = tool.Result{
					Content: fmt.Sprintf("authz: variant resolution failed for %s — refusing to dispatch (fail-closed): %v", t.Name(), rerr),
					IsError: true,
				}
				slog.Default().Info("authz: variant resolution failed",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"tool", t.Name(),
					"err", rerr.Error(),
				)
				return
			}
			perm := resolvedPerm

			// Stamp the pre-dispatch snapshot request onto the IDs carried
			// in callCtx. This updates the context value that the sandbox
			// tool reads via IDsFromCtx when it builds the ToolCall CR.
			// PreDispatchSnapshotForImpact returns nil for non-stateful
			// tools and when the session UID is empty (kubectl / test paths).
			//
			// The snapshot's turn index is memTurnIndex (the durable, monotonic
			// transcript index), NOT turnCount (which resets to 0 on every
			// resume). The AgentSession restart reconciler compares this against
			// the user-chosen CutTurnIndex — a transcript index — in
			// AnalyzePostCut; a turnCount here would select the wrong bundle
			// snapshots to restore for any fork taken after a resume.
			pre := PreDispatchSnapshotForImpact(string(sess.AgentSessionUID), int32(memTurnIndex), int32(i), perm.StateImpact)
			callCtx = sandbox.WithIDs(callCtx, sandbox.IDs{
				TurnIndex:    int(turnIndex),
				ToolUseID:    uses[i].ID,
				BlockIndex:   i + 1, // +1: SeqBlockStart(0) is reserved for the turn-start signal
				MemTurnIndex: memTurnIndex,
				SessionUID:   string(sess.AgentSessionUID),
				PreDispatch:  pre,
			})

			mode := l.toolAuthMode()

			// Disabled mode: surface a one-time visible warning on the first
			// dispatch so the user knows their input is hitting an unprotected
			// agent. The authz skip itself is ToolCallAuthz's (it is not
			// registered when toolCalls.mode=disabled) and the Scope hook still
			// runs; this warning is the one disabled-mode side effect the
			// pipeline does not own.
			if mode == ToolAuthModeDisabled {
				l.disabledWarningOnce.Do(func() {
					if l.DisabledNotify != nil {
						l.DisabledNotify(callCtx)
					}
					slog.Default().Info("tool authz DISABLED for session",
						"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
						"tool", t.Name(),
					)
				})
			}

			// Pre/PostToolCall authz pipeline. The executor runs the active
			// hooks (McpTrust → ToolCallAuthz → Scope at Pre; Scope →
			// InfoLeakRead → InfoLeakAudience at Post) in code-owned order,
			// owning approval publish/await + notices + audit internally.
			//
			// Gate any non-meta tool (leakageGateApplies) OR any tool whose
			// RESOLVED permission requires a check. Meta tools with a trivial
			// (Stateless/Passthrough) permission bypass the pipeline entirely —
			// no external SpiceDB-keyed resource enters context, and
			// ToolCallAuthz/McpTrust would no-op anyway — while a meta tool
			// declaring a check-requiring permission (apply_workspace's External)
			// DOES flow through, since it must be human-approved before dispatch.
			// Uses `perm` (variant-resolved above), not t.Permission(), so this
			// agrees with the Check below and the executor's ToolCallAuthz hook.
			// A PlanGateGoverned meta tool (delegate) also flows through, even
			// though its dispatch permission is Stateless: the plan-gate hook
			// runs ONLY on this path, and it — via the resolver's PlanGateHandle
			// — is what governs the spawn under enforcing mode. Skip it here and
			// the marker is inert: the hook never sees the call, and an unplanned
			// delegation runs ungoverned (a real gap the plangate-governs-
			// unplanned-delegation bundle caught). ToolCallAuthz/McpTrust still
			// no-op on the Stateless permission, exactly as for any ungated meta
			// tool; only the plan gate is added.
			//
			// A PipelineRouted meta tool (record_observation) flows through for
			// the same reason with a different destination hook: the PreToolCall
			// info-leakage audience gate is what governs its write, and it too
			// runs only on this path. Both markers exist because routing is not
			// authorization — see tool.PipelineRouted for what declaring a
			// check-requiring StateImpact to get here costs instead.
			gatePipeline := gatesThroughPipeline(t, perm)
			if gatePipeline {
				// Harvest the agent's per-call justification: per-call _reason
				// envelope field first, then last-text / update_status fallbacks.
				var wrappedArgs struct {
					Reason string `json:"_reason"`
				}
				_ = json.Unmarshal(uses[i].Input, &wrappedArgs)
				justification := HarvestJustification(lastAssistantBlocks, uses[i].ID, wrappedArgs.Reason, l.LastStatusText())

				// Run the full containment pipeline (Pre → Execute → Post). The
				// method owns the fail-closed error handling, Deny/Halt result
				// mapping, dispatch-error wrap, and interrupt override; the
				// dispatch-level bookkeeping (deny counters, halt recording,
				// anyTerm) stays here, driven by the returned phase.
				res, oc := l.executeToolContained(callCtx, sess, t, uses[i].Name, uses[i].Input, uses[i].ID, justification,
					containParams{requester: l.authSubject, subjects: l.authSubjects})
				results[i] = res
				switch oc.Phase {
				case containPreErr:
					// Failed closed before Execute; nothing to count, and the tool
					// never ran, so return without the Terminal check.
					return
				case containPreDeny:
					// If a slot promotion for this resource type was refused by a
					// single-occupancy pin earlier this turn, the denial the model
					// is about to read is a consequence of that pin — append the
					// pin's own refusal text (pinned instance + route out) so the
					// model knows which instance IS usable and how to move, rather
					// than reading a bare "permission denied" as a system fault.
					// pinRefusalApplies skips a call that resolved to the PINNED
					// instance itself (denied by some other gate), where "move"
					// advice would mislabel the denial.
					if perm.Check != nil {
						if ref, ok := l.slotPinRefusalFor(perm.Check.ResourceType); ok &&
							pinRefusalApplies(ref, *perm.Check, argsMap) {
							results[i] = withPinRefusalExplanation(results[i], ref.Text)
						}
					}
					mu.Lock()
					preDeny++
					mu.Unlock()
					return
				case containPreHalt:
					recordHalt(oc.Reason)
					return
				case containPostDeny:
					mu.Lock()
					postDeny++
					mu.Unlock()
				case containPostHalt:
					recordHalt(oc.Reason)
				}
				// containRanOK / containPostErr: result stands (already assigned);
				// fall through to the Terminal check below.
			} else {
				// Ungated meta path: execute directly (with the shared
				// dispatch-error wrap + interrupt override), then treat the result
				// as untrusted unless it opts out via Trusted and run the content
				// guards here, standing in for the PostToolCall inspection the
				// gated path got.
				//
				// Toolguard is the other half the pipeline would have run, applied
				// around the execute (executeMetaToolGuarded) so a rule an admin
				// wrote against a meta tool is not silently inert — see
				// toolguard_meta.go for why only an AUTHORED rule qualifies here.
				res, oc := l.executeMetaToolGuarded(callCtx, sess, t, uses[i].Name, uses[i].Input, uses[i].ID,
					func(ctx context.Context) tool.Result {
						return l.inspectMetaToolCall(ctx, t.Name(), uses[i].Input,
							func(ctx context.Context) tool.Result {
								out, _ := l.executeToolRaw(ctx, t, uses[i].Name, uses[i].Input, sess)
								return out
							})
					})
				// The read-side half the pipeline would have run, applied AFTER
				// the guard's recorder so the two run in the order the gated
				// path's Post leg runs them. Inert for every meta tool whose
				// resolved read declaration names nothing — see
				// leakageread_meta.go for why the one that does could not simply
				// be gated instead.
				res, oc = l.readSidePostForMetaTool(callCtx, sess, uses[i].Name, uses[i].Input, uses[i].ID, res, oc)
				results[i] = res
				switch oc.Phase {
				case containPreDeny:
					mu.Lock()
					preDeny++
					mu.Unlock()
					return
				case containPreHalt:
					recordHalt(oc.Reason)
					return
				case containPostDeny:
					mu.Lock()
					postDeny++
					mu.Unlock()
				case containPostHalt:
					recordHalt(oc.Reason)
				}
			}

			// A respond_to_user that published without error is the one
			// signal that the user has actually seen something this round;
			// it opens the guard for this round's terminal calls.
			if uses[i].Name == "respond_to_user" && !results[i].IsError {
				l.markDelivered()
			}

			if results[i].Terminal {
				mu.Lock()
				anyTerm = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Promote what this round's tool results OBSERVED into slot bindings.
	//
	// POST-dispatch, and it has to be: an `observes` block runs inside a tool's
	// own Execute, after a successful call, so no fact exists to promote until
	// the calls above have returned. The extracted source's promotion is the
	// mirror image (pre-dispatch, at the autofill site) because the extractor
	// has already run over the user's message by the time a tool is dispatched.
	//
	// Once per ROUND rather than once per call, on the loop goroutine rather
	// than inside the per-call goroutines. Two reasons, and both would be bugs
	// the other way: bindSlots does a read-modify-write of the session_scope
	// document, which concurrent callers would race, and each call's callCtx is
	// cancelled by its own goroutine's deferred cancelCall — so an interrupted
	// tool would cancel a promotion that has nothing to do with it. Running
	// here also means one promotion covers every fact this round recorded,
	// however many tools ran.
	//
	// The binding lands before these results reach the model, so the agent's
	// NEXT tool call in the same turn already finds the instance granted — no
	// further user turn, which is the whole point of a mid-turn fill source.
	// Advisory: a failure leaves the slot unbound and the gated call is denied
	// by the ordinary path, so it is logged rather than propagated.
	l.promoteObservedSlots(ctx)

	// Translate the harvested hook outcomes into lifecycle transitions on the
	// loop goroutine (serialized via the sequencer). HookDeny surfaces every
	// gated tool call to status (no-silent-errors); HookHalt records the halt
	// and sets the dispatch-halted flag so the turn loop stops.
	for n := 0; n < preDeny; n++ {
		l.emitLifecycleEvent(ctx, lifecyclecore.HookDeny{Post: false})
	}
	for n := 0; n < postDeny; n++ {
		l.emitLifecycleEvent(ctx, lifecyclecore.HookDeny{Post: true})
	}
	if halted {
		// host.Halt already wrote the terminal Failed status (and ran SessionEnd
		// via l.fail). Record the HookHalt transition for the durable log, and
		// flag the loop to stop — a Halt must end the loop regardless of folded
		// state, so the stop decision is local, not fold-derived.
		l.emitLifecycleEvent(ctx, lifecyclecore.HookHalt{Reason: haltReason})
		l.dispatchHalted = true
	}
	return results, anyTerm
}
