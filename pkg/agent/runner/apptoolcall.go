package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// InteractChecker re-authorizes a browser viewer for a session before a
// proxy-exec (D-B4). webd already cookie-verifies the subject and runs its own
// CheckInteract before forwarding; the runner re-runs a fully-consistent
// agentsession#interact check for the claimed Requester so a forged Requester
// in the NATS envelope is caught here. *spicedb.Client satisfies it; tests
// inject a fake. A nil checker, an indeterminate result, or a deny fails CLOSED.
type InteractChecker interface {
	CheckInteract(ctx context.Context, ns, name string, subject identity.CanonicalUserID, fullyConsistent bool) (bool, error)
}

// appToolAutoJustification is the fixed _reason threaded onto every proxy-exec
// so the audit trail marks the call as an mcp-ui widget auto-invocation rather
// than an LLM tool_use.
const appToolAutoJustification = "mcp-ui app-tool auto-invocation"

// serverReadOnlyHint returns the tool's server-declared readOnlyHint via an
// optional interface. A tool that does not implement it (e.g. a non-MCP tool)
// defaults to false, which correctly denies auto-run — the readonly gate
// requires BOTH a Readonly StateImpact AND server agreement (D-B3).
func serverReadOnlyHint(t tool.Tool) bool {
	if h, ok := t.(interface{ ServerReadOnlyHint() bool }); ok {
		return h.ServerReadOnlyHint()
	}
	return false
}

// appToolSessionContext returns the SessionContext a proxy-exec runs under: a
// shallow copy of the loop's own, with Operations nilled. Nilling Operations
// makes Execute accept the fabricated operation_id (dispatch.go only demands a
// registered id when Operations != nil) and keeps the widget call out of the
// operation-activity tree (D-B6: bypass-the-agent). The copy preserves
// Mem/KG/State/SecretOut so the pipeline's audit writes and gates still work,
// and never mutates l.SessionContext in place.
//
// The minimal fallback (no SessionContext yet) is REACHABLE, not defensive:
// internal/cmd/runner starts the app_tool_call subscription before SetSessionContext, so
// a widget still open from a previous runner can fire into the boot window.
// That caller gets an empty AgentSessionUID (channelsd's status machine reads a
// missing UID as "same identity, no re-baseline") and nil
// State/SecretOut/ArtifactClient — every consumer here is nil-tolerant, but a
// new one that dereferences them would crash the NATS handler, which has no
// recover on the synchronous branch.
//
// Runs on the NATS-subscription goroutine, racing Run's entry-setup; the RLock
// spans the nil-check and the copy so they observe one snapshot (see
// sessionCtxMu's doc comment on Loop).
func (l *Loop) appToolSessionContext() *tool.SessionContext {
	l.sessionCtxMu.RLock()
	defer l.sessionCtxMu.RUnlock()
	if l.SessionContext == nil {
		return &tool.SessionContext{
			Namespace: l.SessionKey.Namespace,
			Name:      l.SessionKey.Name,
		}
	}
	sc := *l.SessionContext // shallow copy
	sc.Operations = nil
	return &sc
}

// appToolExecCtx stamps a synthetic sandbox.IDs onto the ctx a widget app-tool
// (proxy-exec) call runs under, so seqForEmit takes its IDsFromCtx branch and
// NEVER reads l.lastAssistantTurnIndex — which Run mutates on the loop
// goroutine, so a detached proxy-exec reaching seqForEmit (via an
// approval/lifecycle emit) without IDs would race it.
//
// SessionUID is the real owning-session UID, so channelsd's status machine keys
// these emits to the right session; ToolUseID carries the fabricated operation
// id so the authz-decision audit entry correlates to this request. A widget
// call is out-of-band from the LLM turn stream (D-B6), so there is no assistant
// turn to anchor to: MemTurnIndex=0 + BlockIndex=SeqBlockEnd is a deliberate
// session-scoped sentinel, and the resulting Seq only orders a widget's own
// approval-lifecycle emits rather than deriving from racy turn state.
//
// TurnIndex is the ONE synthetic field that must ADVANCE: the tool guard's
// per-turn window rolls over on a change of TurnIndex, so a pinned value makes
// every widget call the session ever made one endless turn — once any rule or
// ceiling sets maxCallsPerTurn, later widget calls are denied for the rest of
// the session, claiming the budget was exhausted "this turn". The per-origin
// AppToolRateLimiter (mcpUiAppTools.maxCallsPerMin) is this path's real volume
// control, deliberately separate from toolguard.
func (l *Loop) appToolExecCtx(ctx context.Context, sess *tool.SessionContext, useID string) context.Context {
	return sandbox.WithIDs(ctx, sandbox.IDs{
		SessionUID:   string(sess.AgentSessionUID),
		ToolUseID:    useID,
		TurnIndex:    int(appToolTurnSeq.Add(1)),
		MemTurnIndex: 0,
		BlockIndex:   channelevents.SeqBlockEnd,
	})
}

// appToolTurnSeq numbers app-tool "turns". Process-global rather than
// per-Loop because the only property required of it is that consecutive calls
// differ: the tool-guard registry it feeds is per-Loop and compares each
// admission against the key's own last-seen turn, so a counter shared with
// another Loop still rolls that window over exactly once per call. Deliberately
// small and monotonic — the value also composes ToolCall CR names on the
// sandbox dispatch path, which must stay RFC 1123 short.
var appToolTurnSeq atomic.Int64

// SetSessionContext publishes the loop's SessionContext under sessionCtxMu.
// internal/cmd/runner uses this for the initial assignment (it cannot touch the
// unexported mutex directly), so that first write is serialized against the
// off-loop readers (the app-tool NATS handler, the SIGTERM stop handler) the
// same way Run's entry-setup write is — completing the invariant that ALL
// l.SessionContext writes go through sessionCtxMu. See sessionCtxMu's doc.
func (l *Loop) SetSessionContext(sc *tool.SessionContext) {
	l.sessionCtxMu.Lock()
	defer l.sessionCtxMu.Unlock()
	l.SessionContext = sc
}

// HandleAppToolCall is the pure, testable core of the app_tool_call responder
// (no NATS). It runs a browser MCP-UI widget's app-tool call and returns the
// result (readonly) or a pending acknowledgement (side-effecting). Order is
// strictly fail-closed:
//
//  1. Unmarshal the wire bytes; malformed → error.
//  2. Re-authorize the Requester (D-B4): a nil checker, an indeterminate
//     result, or a deny → denied, before any lookup or run.
//  3. Look up the tool in the app-only registry; missing → not_found.
//  4. Readonly auto-run predicate (D-B3): ResolvePermissionForArgs picks the
//     permission governing THIS call; auto-run synchronously only when its
//     StateImpact is Readonly AND the server readOnlyHint agrees. Malformed
//     args or an unevaluable variant denies rather than falling back.
//  5. Per-origin rate limit (mcpUiAppTools.maxCallsPerMin) via
//     AppToolRateLimiter — a DEDICATED limiter on this path only, deliberately
//     NOT a toolguard rule (which would also gate the origin's LLM-visible
//     tools and strip its circuit breaker). Applies to BOTH paths (a
//     side-effecting call fires a spammable approval ask); over budget →
//     rate_limited, no run/spawn.
//  6. Build the {operation_id, _reason, args} envelope, then branch:
//     - Readonly: run the tool synchronously through the FULL containment
//     pipeline via executeToolContained and map the outcome to a status.
//     - Side-effecting: SPAWN a bounded, detached executeToolContained (which
//     fires the pipeline's approval ask and runs on approval) off this
//     goroutine and return requires_approval (pending) immediately. The
//     detached result is DISCARDED (D-B6) — logged, never transcript — and a
//     detached Halt is record-only (suppressHalt, D-D4): it fails the widget
//     call, it does NOT terminalize the session off the loop goroutine.
//
// The result is returned over the wire ONLY — never appended to the LLM
// transcript / turn / l.Tools (D-B6); appending is dispatchToolUses's job, and
// it is not invoked here.
//
// This numbered order is the authority for handleAppToolCallReq, the shared
// core below, and for its other two shells (HandleUIDataBinding,
// HandleUIAction): a surface only adds per-call collaborators, it never
// reorders or skips a numbered gate.
func (l *Loop) HandleAppToolCall(ctx context.Context, ia InteractChecker, ns, name string, data []byte) channelevents.AppToolCallResponse {
	req, ok := parseAppToolCallRequest(data)
	if !ok {
		return channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusError, Message: "malformed request"}
	}
	return l.handleAppToolCallReq(ctx, ia, ns, name, req, appToolSurface{})
}

// appToolSurface names which browser surface a call arrived from, and
// carries the per-surface collaborators handleAppToolCallReq's shared core
// needs. The zero value is the mcp-ui widget surface.
type appToolSurface struct {
	// dataBinding gates the readonly-only precondition and routes the
	// browser-sized ingress ceiling. true only for HandleUIDataBinding: an
	// agent-UI action's whole purpose is to mutate, so it takes the SAME
	// zero-value surface HandleAppToolCall does and falls through to the
	// approval path for a side-effecting call.
	dataBinding bool

	// uiAction is the agent-UI action surface's per-call lifecycle recorder.
	// A CONCRETE POINTER, never an interface, so a nil check means what it
	// says and the typed-nil-into-an-interface trap AGENTS.md names cannot
	// arise here (pkg/web/uibindings.Deps picks among the same nil-check
	// shapes for the same reason). nil for the widget and data-binding
	// surfaces, whose every settle/transition/onApproval call against it is
	// therefore a nil-receiver no-op; HandleUIAction always builds and passes
	// a real one (see uiactionrecord.go).
	uiAction *uiActionRecorder
}

// HandleUIDataBinding resolves ONE agent-UI data binding whose source is
// "tool". Deliberately a thin shell over handleAppToolCallReq rather than a
// second implementation: the viewer-bound re-authorization, the three-way
// AppTools grant, the per-origin rate limit, and the full containment pipeline
// are the SAME code the mcp-ui widget path runs. Only two things differ, both
// inside that shared core — the bound tool must satisfy the readonly auto-run
// predicate, and its result is metered against the browser-sized ingress
// ceiling (toolguard's IngressLimitFor) rather than the model-sized one. D-B6
// is unchanged: the result goes over the wire only, never to the transcript.
func (l *Loop) HandleUIDataBinding(ctx context.Context, ia InteractChecker, ns, name string, data []byte) channelevents.AppToolCallResponse {
	req, ok := parseAppToolCallRequest(data)
	if !ok {
		return channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusError, Message: "malformed request"}
	}
	return l.handleAppToolCallReq(ctx, ia, ns, name, req, appToolSurface{dataBinding: true})
}

// HandleUIAction invokes ONE agent-UI action binding — another thin shell over
// handleAppToolCallReq, adding two things inside that shared core:
//
//   - the per-call lifecycle recorder writes a ui_action memory record at
//     every observable transition, which makes the outcome addressable and is
//     the only way a DETACHED side-effecting result reaches the browser at all
//     (see uiactionrecord.go's uiActionRecorder); and
//   - the readonly gate a data binding gets is NOT applied — an action's whole
//     purpose is to mutate, so this shell supplies a zero-dataBinding surface
//     (only uiAction differs from HandleAppToolCall's) and a side-effecting
//     action takes the existing approval path unchanged.
//
// D-B6 is unchanged: the result goes over the wire and, via the recorder, into
// a ui_action record; it is never appended to the transcript, the turn, or
// l.Tools. It becomes addressable to the BROWSER, not to the model.
func (l *Loop) HandleUIAction(ctx context.Context, ia InteractChecker, ns, name string, data []byte) channelevents.UIActionResponse {
	req, ok := parseUIActionRequest(data)
	if !ok {
		return channelevents.UIActionResponse{State: string(uiaction.StateFailed), Message: "malformed request"}
	}

	rec := newUIActionRecorder(l, ns, name, req.RequestID, req.Action, uiaction.RequesterKey(req.Requester))
	appReq := channelevents.AppToolCallRequest{
		ToolName:  req.ToolName,
		Args:      req.Args,
		Requester: req.Requester,
		RequestID: req.RequestID,
	}
	resp := l.handleAppToolCallReq(ctx, ia, ns, name, appReq, appToolSurface{uiAction: rec})

	state := uiaction.StateFor(resp.Status, resp.IsError, false)
	msg := resp.ViewerMessage
	if msg == "" {
		// resp.Message is the containment pipeline's own diagnostic text
		// (permission names, resource IDs, tool names) when ViewerMessage was
		// left unset — never safe to relay to a browser (see
		// AppToolCallResponse.ViewerMessage's doc comment). DisplayCopy is
		// this surface's own generic, state-keyed fallback.
		msg = uiaction.DisplayCopy(state, false)
	}
	return channelevents.UIActionResponse{
		RequestID: req.RequestID,
		State:     string(state),
		Message:   msg,
	}
}

// parseAppToolCallRequest unwraps the channelevents Envelope both browser
// surfaces publish and returns the inner request. ok=false covers both a
// malformed envelope and a malformed payload; the two are indistinguishable
// to a caller and both mean the same thing (nothing trustworthy arrived).
func parseAppToolCallRequest(data []byte) (channelevents.AppToolCallRequest, bool) {
	var env channelevents.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return channelevents.AppToolCallRequest{}, false
	}
	var req channelevents.AppToolCallRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		return channelevents.AppToolCallRequest{}, false
	}
	return req, true
}

// parseUIActionRequest unwraps the channelevents Envelope the agent-UI action
// surface publishes and returns the inner request. A sibling of
// parseAppToolCallRequest, not an overload — its payload type
// (channelevents.UIActionRequest) differs, carrying the declared Action name
// alongside the tool. ok=false covers both a malformed envelope and a
// malformed payload, same as parseAppToolCallRequest.
func parseUIActionRequest(data []byte) (channelevents.UIActionRequest, bool) {
	var env channelevents.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return channelevents.UIActionRequest{}, false
	}
	var req channelevents.UIActionRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		return channelevents.UIActionRequest{}, false
	}
	return req, true
}

// handleAppToolCallReq is the shared privileged core all three browser
// surfaces run through: HandleAppToolCall and HandleUIAction (both
// appToolSurface{} — a write is exactly as unrestricted as an ordinary
// app-tool call; only the lifecycle recorder differs) and HandleUIDataBinding
// (appToolSurface{dataBinding: true}). See HandleAppToolCall's doc comment for
// the numbered, fail-closed order. s.dataBinding adds exactly one precondition
// (below, between (4) and (5)) and one containment-pipeline signal
// (uiDataBinding on the readonly branch's containParams); nothing else about
// the order or the checks changes between callers.
func (l *Loop) handleAppToolCallReq(ctx context.Context, ia InteractChecker, ns, name string, req channelevents.AppToolCallRequest, s appToolSurface) channelevents.AppToolCallResponse {
	// (2) Independent interact re-authorization (D-B4). Fail closed on a nil
	// checker (structurally, or a wiring bug), an indeterminate error, or an
	// explicit deny. Each rejection logs before returning: the browser-visible
	// half is a deliberately vague string that says nothing about WHICH
	// condition rejected the call, so the log is where an operator gets it.
	//
	// Every rejection ALSO settles the ui_action record (a nil-receiver no-op
	// for the two non-action surfaces). That record is the durable half of the
	// lifecycle — the one a reload and an `action:` data binding read — so a
	// deny that answered but never recorded leaves the binding resolving to
	// the no-record copy forever.
	if ia == nil {
		slog.Default().Info("app-tool call denied: no interact checker wired",
			"session", ns+"/"+name, "tool", req.ToolName, "requester", req.Requester)
		resp := viewerDenied("not authorized to interact with this session")
		s.uiAction.settle(ctx, resp)
		return resp
	}
	// SELF-ASSERTED. req.Requester is a field the NATS publisher wrote; nothing
	// here binds it to a browser session. The check below verifies only that the
	// CLAIMED user could interact, not that the claim is true. Naming that at
	// the call site is the point of CanonicalFromTrusted: it is greppable, and
	// it is a finding.
	subj := identity.CanonicalFromTrusted(strings.TrimPrefix(req.Requester, "user:"),
		"self-asserted Requester on the app_tool_call envelope (unverified; see G3)")
	ok, err := ia.CheckInteract(ctx, ns, name, subj, true)
	if err != nil || !ok {
		// err != nil is indeterminate, not a decision — it fails closed the
		// same way, but the two are very different to diagnose, so keep them
		// distinguishable in the log.
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		slog.Default().Info("app-tool call denied: interact re-authorization failed",
			"session", ns+"/"+name, "tool", req.ToolName, "requester", subj.String(),
			"allowed", ok, "err", errStr)
		resp := viewerDenied("not authorized to interact with this session")
		s.uiAction.settle(ctx, resp)
		return resp
	}

	// (3) Lookup in the app-only registry. toolName is req.ToolName run through
	// the SAME synthesize.NormalizeName every synthesized tool name is keyed by
	// (see apptools_grant.go's MaterializeAppTools): AgentUI.spec.tools and
	// AgentClassUIGrant.grantedTools carry a CRD pattern that keeps them
	// normalized, but a Binding.Ref is plain author-typed text, so a data
	// binding's un-normalized ref ("Widgets.CreateIssue") must still resolve to
	// the registry key ("widgets-createissue") synthesize.Build produced. A
	// no-op for widget callers, whose ToolName is already a synthesized
	// t.Name().
	toolName := synthesize.NormalizeName(req.ToolName)
	t, found := l.AppTools[toolName]
	if !found {
		// The most likely cause is not a bad request but the three-way grant:
		// the tool exists on some origin and never made the intersection, so
		// the widget's button is dead. Log the whole registry so the gap
		// between what the UI asked for and what it got is visible in one line.
		slog.Default().Info("app-tool call rejected: tool not in the browser-callable registry",
			"session", ns+"/"+name, "tool", req.ToolName, "requester", subj.String(),
			"registered", slices.Sorted(maps.Keys(l.AppTools)))
		resp := channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusNotFound, Message: "unknown app tool"}
		s.uiAction.settle(ctx, resp)
		return resp
	}

	// (3b) Widget origin pin. l.AppTools is ONE flat map across every origin the
	// session has, so a lookup by name alone let a widget served by server A
	// invoke server B's tool. A needs no app-tool grant of its own for that —
	// any ui:// resource from any MCP server is promoted into a widget — so the
	// whole escalation rides on B having earned one.
	//
	// req.WidgetOrigin is the CALLER's server, set server-side by webd from this
	// session's status.activeWidgets. Empty means the call did not come from a
	// widget (an agent-declared UI binding), and those stay unpinned: their
	// declaration is agent-authored and already gated by the three-way grant.
	//
	// A cross-origin callee answers exactly as an absent one does, so this is
	// not a tool-enumeration oracle for a widget probing the other servers in
	// the session. The log line is where the difference is visible.
	if req.WidgetOrigin != "" {
		calleeOrigin := ""
		if ot, ok := t.(tool.OriginTool); ok {
			calleeOrigin = ot.Origin()
		}
		if calleeOrigin != req.WidgetOrigin {
			slog.Default().Info("app-tool call rejected: widget called a tool from another origin",
				"session", ns+"/"+name, "tool", req.ToolName, "requester", subj.String(),
				"widgetOrigin", req.WidgetOrigin, "calleeOrigin", calleeOrigin)
			resp := channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusNotFound, Message: "unknown app tool"}
			s.uiAction.settle(ctx, resp)
			return resp
		}
	}

	// (4) Readonly auto-run predicate (D-B3): auto-run synchronously iff the
	// PERMISSION THAT GOVERNS THIS CALL — the first PermissionVariant whose
	// When matches req.Args, else the tool's static fallback, via the same
	// ResolvePermissionForArgs the other two dispatch paths use — has
	// StateImpact Readonly AND the server readOnlyHint agrees. A side-effecting
	// call continues through the rate limit, then fires its approval detached
	// below.
	//
	// Resolved against req.Args directly, not the {operation_id,_reason,args}
	// envelope built in step (6): that envelope does not exist yet, and
	// req.Args is exactly the level the containment pipeline's own
	// unwrapEnvelope hands ResolvePermission, so resolving anywhere else would
	// let the two disagree on any args template containing an "operation_id"
	// key. A malformed req.Args, or a CEL compile/eval failure resolving a
	// variant, leaves this call's authorization posture UNKNOWN and denies
	// outright — falling back to t.Permission() is precisely the fail-open a
	// variant exists to prevent.
	argsMap := map[string]any{}
	if len(req.Args) > 0 {
		if uerr := json.Unmarshal(req.Args, &argsMap); uerr != nil {
			slog.Default().Info("app-tool call denied: args could not be parsed to resolve the permission that governs this call",
				"session", ns+"/"+name, "tool", req.ToolName, "requester", subj.String(), "err", uerr.Error())
			resp := viewerDenied("this view can only read data; that control performs an action")
			s.uiAction.settle(ctx, resp)
			return resp
		}
	}
	perm, rerr := ResolvePermissionForArgs(t, argsMap)
	if rerr != nil {
		slog.Default().Info("app-tool call denied: permission variant could not be resolved",
			"session", ns+"/"+name, "tool", req.ToolName, "requester", subj.String(), "err", rerr.Error())
		resp := viewerDenied("this view can only read data; that control performs an action")
		s.uiAction.settle(ctx, resp)
		return resp
	}
	autoRun := perm.StateImpact == authz.Readonly && serverReadOnlyHint(t)

	// A DATA binding may only READ. Every entry in a declaration's
	// Node.Bindings is a data binding — a mutation is an action binding on a
	// different field — so a side-effecting tool named here is a declaration
	// bug or an injection attempt, never a call to park for approval. Placed
	// after the interact re-authorization and the registry lookup so an
	// unauthorized caller learns nothing about which tools exist, and before
	// the detached-approval branch can spawn anything.
	if s.dataBinding && !autoRun {
		slog.Default().Info("ui data binding rejected: bound tool is not readonly",
			"session", ns+"/"+name, "tool", req.ToolName, "requester", subj.String(),
			"stateImpact", perm.StateImpact, "serverReadOnlyHint", serverReadOnlyHint(t))
		return viewerDenied("this view can only read data; that control performs an action")
	}

	// (5) Per-origin rate limit (mcpUiAppTools.maxCallsPerMin), app-tool path
	// only — deliberately NOT via toolguard, so a toolguard rule can't strip
	// the origin's circuit breaker. Applies to BOTH branches: a side-effecting
	// call fires an approval ask (Slack/webchat), itself spammable. An origin
	// with no configured limit, or a tool with no Origin() (non-MCP), is
	// unlimited; a nil AppToolRateLimiter (unwired caller/test) always allows.
	origin := ""
	if ot, ok := t.(tool.OriginTool); ok {
		origin = ot.Origin()
	}
	if !l.AppToolRateLimiter.Allow(origin) {
		slog.Default().Info("app-tool call rejected: per-origin rate limit exceeded",
			"session", ns+"/"+name, "tool", req.ToolName, "requester", subj.String(),
			"origin", origin, "autoRun", autoRun)
		const rateLimited = "app-tool rate limit exceeded; try again shortly"
		resp := channelevents.AppToolCallResponse{
			Status:        channelevents.AppToolCallStatusRateLimited,
			Message:       rateLimited,
			ViewerMessage: rateLimited,
		}
		s.uiAction.settle(ctx, resp)
		return resp
	}

	// (6) Build the SAME wrapped envelope the LLM passes to executeToolContained:
	// {operation_id, _reason, args}. The fabricated operation_id need only be
	// non-empty (Execute accepts it because the proxy-exec SessionContext has
	// Operations nil); _reason marks the audit trail as an app-tool
	// auto-invocation.
	operationID := "mcp-ui-app-tool-" + req.RequestID
	envArgs, merr := json.Marshal(struct {
		OperationID string          `json:"operation_id"`
		Reason      string          `json:"_reason"`
		Args        json.RawMessage `json:"args"`
	}{
		OperationID: operationID,
		Reason:      appToolAutoJustification,
		Args:        req.Args,
	})
	if merr != nil {
		// Marshalling a struct this function just built can only fail on
		// req.Args, which arrived as json.RawMessage and is re-emitted
		// verbatim — so this is invalid JSON that reached the wire, or a
		// programming error. Log the cause, and settle the record so the
		// control does not sit disabled awaiting a transition that never comes.
		slog.Default().Info("app-tool call rejected: could not build the tool-call envelope",
			"session", ns+"/"+name, "tool", req.ToolName, "requester", subj.String(), "err", merr.Error())
		const malformed = "this action could not be sent"
		resp := channelevents.AppToolCallResponse{
			Status:        channelevents.AppToolCallStatusError,
			Message:       "malformed request",
			ViewerMessage: malformed,
		}
		s.uiAction.settle(ctx, resp)
		return resp
	}

	// The check subject + approval Requester attribute to the VIEWER (subj), not
	// the session subject (D-D1); the synthetic-IDs ctx keeps seqForEmit off the
	// loop-mutated lastAssistantTurnIndex. Both branches share one SessionContext
	// snapshot taken on THIS goroutine.
	//
	// What makes that snapshot safe is the VALUE copy under sessionCtxMu, not
	// pointer discipline: Run's entry-setup keeps the pointer internal/cmd/runner
	// pre-populated (the production path) and mutates SubmitResult/Mem/KG on it
	// in place. The copy is taken under RLock against that write and is
	// thereafter this call's own struct. Any NEW in-place mutation of
	// l.SessionContext's fields MUST therefore also hold sessionCtxMu.Lock —
	// nothing else protects this copy.
	sess := l.appToolSessionContext()

	// (7a) Readonly auto-run: synchronous on the caller's goroutine
	// (suppressHalt stays false — an in-band readonly Halt may terminalize like
	// an LLM readonly call). uiDataBinding carries the data-binding gate above
	// through to the containment pipeline so toolguard metres the result
	// against the browser-sized ingress ceiling (IngressLimitFor) instead of
	// the model-sized one; false for an ordinary widget app-tool call AND for
	// an agent-UI action, which is metered like any other tool call.
	if autoRun {
		callCtx := l.appToolExecCtx(ctx, sess, operationID)
		res, oc := l.executeToolContained(callCtx, sess, t, toolName, envArgs, operationID, appToolAutoJustification,
			containParams{requester: subj, subjects: []string{subj.String()}, proxyExec: true, uiDataBinding: s.dataBinding})
		resp := appToolResponseFor(res, oc)
		s.uiAction.settle(ctx, resp)
		return resp
	}

	// (7b) Side-effecting: fire the approval and run on approval DETACHED from
	// this NATS goroutine, returning `requires_approval` (pending) immediately.
	// The detached exec reuses the FULL containment pipeline — the ToolCallAuthz
	// hook routes the call to the approval ask and Await, then runs it on
	// approval — so none of the approval flow is re-implemented here.
	// Unreachable with s.dataBinding=true (the gate above already denied any
	// non-autoRun data binding); reached by ordinary widget app-tool calls and
	// by side-effecting agent-UI actions, which take this same approval path.
	//
	// ctx is the NATS subscription's rootCtx (session-scoped, long-lived), so
	// the detached call outlives m.Respond but dies at session/pod shutdown,
	// and is further bounded by resolvedApprovalTimeout (+ defer cancel) so it
	// cannot leak past the approval window. suppressHalt:true makes an off-loop
	// Halt record-only (no l.fail): it fails THIS call, it does NOT terminalize
	// the agent session out from under the loop goroutine (D-D4).
	if l.approvalAskSpawnHookForTest != nil {
		l.approvalAskSpawnHookForTest()
	}
	// Recorded on THIS (synchronous) goroutine, before the spawn: the browser's
	// own synchronous response (below) already says "submitted", so this write
	// is what makes that same fact addressable by requestID — a reconnecting
	// browser that missed the response can still learn it from memory.
	s.uiAction.transition(ctx, uiaction.StateSubmitted, "", false)
	detachedCtx, cancel := context.WithTimeout(ctx, l.resolvedApprovalTimeout())
	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("app-tool detached exec panicked",
					"session", ns+"/"+name, "tool", req.ToolName, "err", r)
			}
		}()
		cp := containParams{requester: subj, subjects: []string{subj.String()}, proxyExec: true, suppressHalt: true}
		if s.uiAction != nil {
			// approvalEvent is installed only when a recorder exists, keeping
			// the zero value of containParams.approvalEvent genuinely nil for
			// the widget and data-binding surfaces, rather than a safe-but-live
			// method value bound to a nil receiver.
			cp.approvalEvent = s.uiAction.onApproval
		}
		callCtx := l.appToolExecCtx(detachedCtx, sess, operationID)
		res, oc := l.executeToolContained(callCtx, sess, t, toolName, envArgs, operationID, appToolAutoJustification, cp)
		// D-B6: the detached result is DISCARDED from the LLM's view — never
		// appended to the transcript / turn / l.Tools. Log the terminal phase for
		// audit/debug; settle (a nil-receiver no-op when s.uiAction is nil) is
		// what actually gives this outcome an address the browser can read back.
		slog.Default().Info("app-tool side-effecting detached exec completed",
			"session", ns+"/"+name, "tool", req.ToolName, "phase", oc.Phase.String(), "isError", res.IsError)
		s.uiAction.settle(callCtx, appToolResponseFor(res, oc))
	}()
	return channelevents.AppToolCallResponse{
		Status:  channelevents.AppToolCallStatusRequiresApproval,
		Message: "approval requested — pending; ask in chat for the result",
	}
}

// viewerDenied is a denial whose copy is authored HERE, by this file, for a
// human viewer — so it is safe for a browser-facing caller
// (pkg/web/uibindings/tool's errorForStatus) to render verbatim. Every other deny
// site relays a reason from elsewhere and must NOT set ViewerMessage; see
// AppToolCallResponse.ViewerMessage for why empty is the fail-safe default.
func viewerDenied(msg string) channelevents.AppToolCallResponse {
	return channelevents.AppToolCallResponse{
		Status:        channelevents.AppToolCallStatusDenied,
		Message:       msg,
		ViewerMessage: msg,
	}
}

// appToolResponseFor maps a contained-execute outcome to the wire response.
// A ranOK call surfaces the tool result (IsError reflects the tool's own
// outcome, not a transport failure); a Deny → denied; every halt/error phase
// → error. The deny/error Message prefers the outcome Reason, falling back to
// the result content.
//
// Both are the CONTAINMENT PIPELINE's own text — an authz hook's formatDeny, a
// session-scope refusal naming a resource ID, a toolguard byte-budget message
// written for the model — diagnostic, not viewer copy. Hence ViewerMessage is
// left unset on every arm: a browser-facing caller reads that field, finds it
// empty, and substitutes its own fixed copy.
func appToolResponseFor(res tool.Result, oc containedOutcome) channelevents.AppToolCallResponse {
	switch oc.Phase {
	case containRanOK:
		encoded, err := json.Marshal(res.Content)
		if err != nil {
			return channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusError, Message: "result encoding failed"}
		}
		return channelevents.AppToolCallResponse{
			Status:  channelevents.AppToolCallStatusOK,
			Result:  encoded,
			IsError: res.IsError,
		}
	case containPreDeny, containPostDeny:
		// A deny caused by the agent's own unevaluatable definition is NOT an
		// access decision; reporting it as one sends the viewer to ask for a
		// permission that would not have helped. Message keeps the diagnostic
		// text for the log; ViewerMessage stays unset so the browser
		// substitutes copy that names the problem without quoting a CEL rule at
		// someone who did not write it. The operator gets the rule itself on
		// the monitoring channel (reportDefinitionError).
		if oc.Definition != nil {
			return channelevents.AppToolCallResponse{
				Status:  channelevents.AppToolCallStatusMisconfigured,
				Message: reasonOr(oc.Reason, res.Content),
			}
		}
		return channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusDenied, Message: reasonOr(oc.Reason, res.Content)}
	default: // containPreHalt, containPostHalt, containPreErr, containPostErr
		return channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusError, Message: reasonOr(oc.Reason, res.Content)}
	}
}

// reasonOr returns primary when non-empty, else fallback.
func reasonOr(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
