package runner

// pipeline_wiring.go constructs the per-Loop pipeline.Registry + Executor
// from the Loop's config fields and wires the five authz hooks with their
// dependency closures. Called by (*Loop).executor() (lazily on first use).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	mcptool "github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/grants"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/contentguardaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagedecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolguardaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// buildPipelineRegistry registers exactly the active authz hooks for this Loop's
// config, in the code-owned order. The hooks self-register as HookFactory
// entries (hookregistry.go, hooks_dataplane.go) and decide their own activation
// from the shared table (hooks.ActiveHooks — see hookActive); this just drains
// them for this Loop. SessionStart's ColdStartScope is deliberately NOT
// registered here — it is built per-call at the Run() site because its placement
// callback is bound to the live per-call host (see coldStartScopeDeps).
func (l *Loop) buildPipelineRegistry() *pipeline.Registry {
	reg := pipeline.NewRegistry()
	for _, f := range hookFactories {
		for _, h := range f.Build(l) {
			reg.Register(h, f.Order)
		}
	}
	return reg
}

// hookActive reports whether the named hook is ActiveYes in the shared
// activation table for this Loop's config. Computed once per Loop. The table is
// the single source of truth the registry and `oap agent authz` view both read,
// so they cannot drift.
func (l *Loop) hookActive(name string) bool {
	l.hookActiveOnce.Do(func() {
		l.hookActiveMap = map[string]bool{}
		for _, d := range hooks.ActiveHooks(l.activationConfig()) {
			l.hookActiveMap[d.Name] = d.Active == hooks.ActiveYes
		}
	})
	return l.hookActiveMap[name]
}

// activationConfig projects this Loop's AgentClass authz config into the neutral
// hooks.ActivationConfig consumed by the shared activation table. nil-safe.
func (l *Loop) activationConfig() hooks.ActivationConfig {
	cfg := hooks.ActivationConfig{
		ToolCallMode: l.toolAuthMode(),
	}
	if l.AgentClass != nil {
		sc := l.AgentClass.Spec.GetScope()
		cfg.ScopeEnabled = sc.Enabled
		cfg.ColdStart = sc.ColdStart
		// The EFFECTIVE policy — declared, or derived onto status by the class
		// reconciler. `oap agent authz` renders the same activation table
		// through the same accessor, so both describe a derived class alike.
		cfg.InteractPermission = l.AgentClass.EffectiveSessionInteractPermission()
		cfg.BoundEntityCount = len(l.AgentClass.Spec.GetSlots())
	}
	if l.LeakageConfig != nil {
		cfg.LeakageMode = l.LeakageConfig.ResolvedMode()
	}
	return cfg
}

// sessionCleanupDeps constructs the SessionCleanupDeps for the SessionCleanup
// hook. RemoveExternalState is nil today (no runner-owned external state to
// remove at SessionEnd; the operator finalizer owns the durable teardown).
func (l *Loop) sessionCleanupDeps() hooks.SessionCleanupDeps {
	return hooks.SessionCleanupDeps{
		RemoveExternalState: nil,
		Logger:              slog.Default(),
	}
}

// coldStartScopeDeps builds the ColdStartScopeDeps for the SessionStart hook,
// pre-resolving everything from Loop fields so the hook imports neither
// pkg/agent/runner nor pkg/apis/v1alpha1. The placement callback binds to the
// LIVE per-call host (its setColdStartPlacement), which is why this hook is
// built per-call at the Run() site rather than in the shared, host-agnostic
// cached registry. Precondition: callers gate on coldStartEligible(), so
// AgentClass + the scope block are non-nil here.
func (l *Loop) coldStartScopeDeps(host *runnerHost) hooks.ColdStartScopeDeps {
	sc := l.AgentClass.Spec.GetScope()

	maxMs := l.resolvedScopeLLMLatency()

	var boundEntities []scope.EnvelopeBoundEntity
	for _, be := range l.AgentClass.Spec.GetSlots() {
		boundEntities = append(boundEntities, scope.EnvelopeBoundEntity{
			ResourceType: be.ResourceType,
			Permission:   be.Permission,
		})
	}
	toolNames := make([]string, 0, len(l.Tools))
	for _, t := range l.Tools {
		toolNames = append(toolNames, t.Name())
	}

	// Pre-generate a stable request ID so the DecisionAsked and DecisionResolved
	// events share the same correlation key in the lifecycle log.
	reqID := newRequestID()

	return hooks.ColdStartScopeDeps{
		Requester: l.StartedByCanonical,
		AutoApply: sc.ColdStart == "extractAndAutoApply",
		// Consolidated approval-WAIT timeout (authz.approvalTimeout). The
		// cold-start human-click window sources from the same single value as the
		// tool_call and leakage_share asks.
		ApprovalTimeout: l.resolvedApprovalTimeout(),
		LLMLatency:      time.Duration(maxMs) * time.Millisecond,
		BoundEntities:   boundEntities,
		ToolNames:       toolNames,
		Publish: func(ctx context.Context, payload []byte) error {
			return l.ColdStartRequestPublish(ctx, l.SessionKey.Namespace, l.SessionKey.Name, payload)
		},
		WaitForTask: func(ctx context.Context, deadline time.Duration) {
			_ = authz.WaitForColdStartTask(ctx, l.Mem, l.bindingScope(), deadline)
		},
		GetTask: func(ctx context.Context) (coldstarttask.Content, bool, error) {
			return coldstarttask.Get(ctx, l.Mem, l.bindingScope())
		},
		SetPlacement: host.setColdStartPlacement,
		// Record the scope_review decision ask in the signed lifecycle log so the
		// operator fold sees ScopeReviewPending while turn-0 is blocked.
		OnScopeReviewAsked: func(ctx context.Context) {
			l.emitLifecycleEvent(ctx, lifecyclecore.DecisionAsked{
				RequestID: reqID,
				Kind:      lifecyclecore.DecisionScopeReview,
			})
		},
		// Record the resolved scope_review outcome. A timeout drives
		// Failed[ScopeReviewFailed] in the fold (timeoutFail); an explicit
		// failure (authzd error or ScopeReviewFailed status) records as
		// Approved=false, TimedOut=false and lets the subsequent HookHalt +
		// RunnerTerminal events drive the failure phase.
		OnScopeReviewResolved: func(ctx context.Context, approved, timedOut bool) {
			l.emitLifecycleEvent(ctx, lifecyclecore.DecisionResolved{
				RequestID: reqID,
				Approved:  approved,
				TimedOut:  timedOut,
			})
		},
		Logger: slog.Default(),
	}
}

// coldStartSessionStartExecutor builds a one-shot SessionStart pipeline executor
// bound to the live host, carrying up to two hooks, each registered only when its
// precondition holds: IdentityChoiceGate when l.IdentityGatePending (a first-boot
// ask|dynamic session must ask the initiating user to choose an identity), and
// ColdStartScope when coldStartEligible (the scope review for the initial
// prompt). The gate runs FIRST (lower order) so a userPassthrough handoff halts
// before ColdStartScope, and an agent choice is settled before scope narrowing.
//
// SessionStart needs a per-call executor rather than the cached one because both
// hooks' side channels (ColdStartScope's SetPlacement, the gate's identityHandoff
// flag) must reach the live per-call host; PreToolCall/PostToolCall have no such
// side channel and share the cached executor.
func (l *Loop) coldStartSessionStartExecutor(host *runnerHost, coldStartEligible bool) *pipeline.Executor {
	reg := pipeline.NewRegistry()
	if l.IdentityGatePending {
		reg.Register(newIdentityChoiceGate(l, host), hooks.OrderIdentityChoiceGate)
	}
	if coldStartEligible {
		reg.Register(hooks.NewColdStartScope(l.coldStartScopeDeps(host)), hooks.OrderColdStartScope)
	}
	return pipeline.NewExecutor(reg)
}

// pipelineRunner is the seam over pipeline.Executor.Run that the Loop depends
// on. *pipeline.Executor satisfies it; tests inject a fake whose Run returns a
// non-nil error (a host-primitive failure the executor can't turn into a
// verdict) to exercise the fail-closed gate paths.
type pipelineRunner interface {
	Run(ctx context.Context, p pipeline.Point, in pipeline.Input, host pipeline.Host) (pipeline.Outcome, error)
}

// executor returns the per-Loop pipeline runner, constructed lazily on first
// call. Safe for concurrent use: the executor is immutable once built.
//
// The registry is retained beside the executor (l.pipelineReg) because the
// read-side PostToolCall pass over ungated meta tools reuses its hook
// INSTANCES — see readSidePostRunner in leakageread_meta.go for why a second
// set would be a second info_leak_audience over one session.
func (l *Loop) executor() pipelineRunner {
	l.pipelineOnce.Do(func() {
		reg := l.buildPipelineRegistry()
		l.pipelineReg = reg
		l.pipelineExec = pipeline.NewExecutor(reg)
	})
	return l.pipelineExec
}

// hasMCPTools reports whether any MCP tools are registered on this Loop, across
// BOTH registries. Called at hook-build time (Loop setup, single goroutine) to
// gate the mcp_trust hook (SEP-1913 trust-annotation validation).
//
// App-visible-only tools live in a SEPARATE registry (l.AppTools), never in
// l.Tools, so an app-ONLY MCP server would leave this false, the mcp_trust hook
// unregistered, and that server's proxy-exec calls unvalidated. Ranging both
// closes the gap.
func (l *Loop) hasMCPTools() bool {
	for _, t := range l.Tools {
		if t.Kind() == "mcp" {
			return true
		}
	}
	for _, t := range l.AppTools {
		if t.Kind() == "mcp" {
			return true
		}
	}
	return false
}

// lookupTool resolves a tool by name across BOTH registries the Loop holds: the
// LLM-visible l.Tools (checked FIRST, so the LLM dispatch path is unchanged) and
// the MCP-UI app-visible l.AppTools. The two are disjoint by construction
// (mcp.Synthesize routes each tool to exactly one set), so Tools-first never
// masks an AppTools match. Spanning both is what lets the contained pipeline's
// per-call lookups (ResolvePermission, the approval-ask tool description,
// toolGuardLookup, mcpSpecLookup) see an app-tool's REAL permission / kind /
// origin / spec — without it a widget (proxy-exec) call's per-resource authz
// Check, approval routing, and toolguard breaker keying never evaluate it (see
// ResolvePermission for the mode-by-mode consequence).
//
// Concurrency: reached from the NATS app-tool handler goroutine
// (HandleAppToolCall → executeToolContained → the pipeline hooks), which races
// ToolRefresher's mid-turn `l.Tools = …` reassignment on the Run goroutine —
// hence the snapshot under toolsMu.RLock before ranging. l.AppTools needs no
// lock: it is populated once at Loop setup and never reassigned.
// resolveToolPermission resolves the Permission (StateImpact + Check) a call
// to toolName with argsMap runs under.
//
// A method rather than an inline closure because it now has two callers: the
// ToolCallAuthz deps, and the per-datum leakage checkpoint, which derives a
// call's DESTINATION from the very Check this returns. The file already warns
// that a duplicated copy of this rule cannot drift between the dispatch paths;
// a fourth copy for the leakage path would be exactly that.
func (l *Loop) resolveToolPermission(toolName string, argsMap map[string]any) (authz.Permission, error) {
	t, ok := l.lookupTool(toolName)
	if !ok {
		// Tool not found; return zero permission (Stateless → Allow).
		return authz.Permission{}, nil
	}
	return l.permissionForTool(t, argsMap)
}

// permissionForTool is resolveToolPermission with the tool already looked up.
//
// A tool that can resolve its OWN call answers first. A sandbox tool is one
// tool for a whole CLI, and which permission applies depends on the subcommand
// in its argv — `gh pr create` is external where `gh pr view` is readonly. It
// resolves that with the toolkit's own parser, which already skips leading
// global flags; re-deriving it from CEL here would be a second implementation
// of argv parsing inside an authorization path, free to disagree with the
// first.
//
// The variant-vs-fallback resolution itself is ResolvePermissionForArgs
// (pkg/agent/runner/permission_resolve.go) — shared with dispatchToolUses and
// the agent-UI readonly gate in apptoolcall.go.
func (l *Loop) permissionForTool(t tool.Tool, argsMap map[string]any) (authz.Permission, error) {
	if pc, ok := t.(tool.PerCallPermission); ok {
		if got, ok := pc.PermissionForCall(argsMap); ok {
			return got, nil
		}
	}
	return ResolvePermissionForArgs(t, argsMap)
}

func (l *Loop) lookupTool(name string) (tool.Tool, bool) {
	l.toolsMu.RLock()
	tools := l.Tools // snapshot the slice header under the lock
	l.toolsMu.RUnlock()
	for _, t := range tools {
		if t.Name() == name {
			return t, true
		}
	}
	if t, ok := l.AppTools[name]; ok {
		return t, true
	}
	return nil, false
}

// mcpSpecLookup returns an MCPSpecResolver that resolves, for a given tool name,
// the mcpspec.Spec AND the server-side tool name (the validator's allowlist key)
// across BOTH tool registries (via lookupTool). The server-side name is required
// because validator.Check looks the tool up in the spec by that name, not the LLM
// <serverName>_<toolName> — mirroring the in-Execute validator call. App-tools
// are MCP tools sharing the same static spec, so McpTrust must SEP-1913-validate
// a widget (proxy-exec) call exactly like an LLM MCP call.
func (l *Loop) mcpSpecLookup() hooks.MCPSpecResolver {
	return func(toolName string) (*mcpspec.Spec, string) {
		t, ok := l.lookupTool(toolName)
		if !ok || t.Kind() != "mcp" {
			return nil, ""
		}
		if m, ok := t.(*mcptool.MCPTool); ok {
			return m.MCPSpec(), m.MCPServerToolName()
		}
		return nil, ""
	}
}

// toolCallAuthzDeps constructs the ToolCallAuthzDeps for the ToolCallAuthz hook.
func (l *Loop) toolCallAuthzDeps() hooks.ToolCallAuthzDeps {
	// BuildApprovalAsk is wired ONLY when an approval flow is available:
	// l.Approval plus l.InteractionRequestPublish, the publisher tool_call's
	// generic interaction_request rides. Without both (kubectl-driven, or a test
	// with no publish hook), the hook's BuildApprovalAsk==nil branch denies with
	// the SpiceDB message rather than emitting an ask whose publish would fail
	// and Halt the session.
	var buildApprovalAsk hooks.ApprovalAskBuilder
	if l.Approval != nil && l.InteractionRequestPublish != nil {
		buildApprovalAsk = func(ctx context.Context, in pipeline.Input, argsMap map[string]any, perm authz.Permission, justification string) (*pipeline.ApprovalAsk, error) {
			// in.Requester / in.ProxyExec attribute the approver-facing
			// Requester to the widget viewer on a proxy-exec (D-D1); the LLM
			// path (ProxyExec=false) keeps the session's LastInbound display.
			return l.buildToolCallApprovalAsk(ctx, in.Tool.Name, argsMap, perm, in.Tool.UseID, justification, in.Requester, in.ProxyExec)
		}
	}

	// BuildWaiverAsk is wired under the SAME condition as BuildApprovalAsk (an
	// approval flow is available). Without it a Refused precondition denies
	// fail-closed with the gate's own message — the plan-2 behaviour — rather
	// than raising a card whose publish would Halt the session.
	var buildWaiverAsk hooks.WaiverAskBuilder
	if l.Approval != nil && l.InteractionRequestPublish != nil {
		buildWaiverAsk = func(ctx context.Context, in pipeline.Input, argsMap map[string]any, perm authz.Permission, denial *authz.PreconditionDenial) (*pipeline.ApprovalAsk, error) {
			return l.buildPreconditionWaiverAsk(ctx, in, argsMap, perm, denial)
		}
	}

	return hooks.ToolCallAuthzDeps{
		Mode:    l.toolAuthMode(),
		Checker: &engineChecker{l: l},
		ResolvePermission: func(toolName string, argsMap map[string]any) (authz.Permission, error) {
			// lookupTool spans l.Tools AND l.AppTools: an app-tool (proxy-exec)
			// call must resolve its REAL Permission (StateImpact + Check +
			// variants) so ToolCallAuthz runs the per-resource check and routes
			// side-effecting calls to approval. An l.Tools-only scan yields the
			// zero Permission (StateImpact "") for app-tools and the real check
			// never runs: fail-OPEN under permissive mode (the zero-perm Eval
			// branch proceeds) and a spurious fail-CLOSED "unknown stateImpact"
			// deny under enforcing.
			return l.resolveToolPermission(toolName, argsMap)
		},
		// BuildInputs MUST set SessionScope = scope.Scope{} (identity — scope
		// narrowing is the Scope hook's job). Hard-deny enforcement is the Scope
		// hook's job too (Layer-3 SpiceDB disallow is retired). Spec §8: no
		// double-enforcement.
		// NormalizeArgs renders the tool's own parsed view of a call, ONCE, for
		// every consumer in the hook.
		//
		// A check reads NAMED arguments — resourceIDTemplate "{repo}" resolves a
		// key out of this map. MCP tools already arrive named; a sandbox tool
		// arrives with an argv ARRAY, so the template found nothing and every
		// per-resource check on one failed. The permission resolved and the
		// RESOURCE did not.
		//
		// This lived INSIDE BuildInputs, which meant only the check saw the
		// parsed view: the approval ask was handed the raw envelope, resolved
		// `{remote}` against a map with no remote in it, and produced an empty
		// resource id. Observed live — the card rendered the push URL correctly
		// (it shows argv), the human approved, and channelsd then refused the
		// decision with "slot binding needs resourceType, resourceID and
		// permission". One call must have ONE view.
		//
		// The tool renders it because only it knows how to parse its own call. A
		// tool that cannot is passed through untouched.
		NormalizeArgs: func(in pipeline.Input, argsMap map[string]any) map[string]any {
			if na, ok := l.lookupTool(in.Tool.Name); ok {
				if p, ok := na.(tool.NamedCallArgs); ok {
					if named, ok := p.NamedArgs(argsMap); ok {
						return named
					}
				}
			}
			return argsMap
		},
		BuildInputs: func(in pipeline.Input, argsMap map[string]any, perm authz.Permission) authz.Inputs {
			// The check subject + subject-list are the PER-CALL principal
			// (in.Requester / in.Subjects), NOT the loop-level l.authSubject.
			// On the app-tool (proxy-exec) path that is the widget viewer, so
			// the check runs against the viewer and a detached caller reads a
			// per-call value instead of racing advanceRequester's writes. The
			// LLM path threads in l.authSubject / l.authSubjects.
			ai := authz.Inputs{
				Args:     argsMap,
				Subject:  in.Requester.String(),
				Subjects: in.Subjects,
				// Threaded so an EXTERNAL permission can be satisfied by a slot grant
				// this session already holds — the grant leg is asked about the
				// SESSION, and without it external denies and prompts every call.
				AgentSessionRef:   l.SessionKey.Namespace + "/" + l.SessionKey.Name,
				SlotResourceTypes: l.PlanGateSlotTypes,
				// SessionScope: scope.Scope{} (zero value = identity pass-through)
			}
			// No session-grant fallback to wire: routeViaSessionGrant is gone and
			// the fallback with it. The tool's natural Check resolves
			// post-approval on its own, through the slot grant the approval
			// writes on the resource.
			return ai
		},
		BuildApprovalAsk: buildApprovalAsk,
		BuildWaiverAsk:   buildWaiverAsk,
		RecordDecision:   l.recordAuthzDecision,
		// ForceApproval wires the per-call escalation for MCP tools whose backing
		// dependency drifted from its pin baseline under the "approve" mode.
		// Nil-safe: only set when PinDrift is non-nil so the hook's nil check is
		// sufficient — assigning a typed-nil function would still leave the field
		// non-nil, but assigning nil directly keeps it as a true nil func value.
		ForceApproval: func() func(string) (string, bool) {
			if l.PinDrift == nil {
				return nil
			}
			return l.PinDrift.Drifted
		}(),
		// ExplainPrecondition turns "permission denied on github_pr:X" into the
		// author's own sentence about WHY the slot holding X is empty. Built
		// once here rather than per call: the nil-ness of the returned func is
		// what the hook reads to decide the class has no gate at all.
		ExplainPrecondition: l.explainSlotPrecondition(),
		Logger:              slog.Default(),
	}
}

// applyProvenance fills pin-provenance fields on d from a ProvenanceRecord.
// ok is false when no record exists for the tool; in that case d is unchanged.
// This is a pure helper (no I/O, no side effects) so it can be unit-tested
// independently of the memory layer.
func applyProvenance(d *authzdecision.Decision, rec ProvenanceRecord, ok bool) {
	if !ok {
		return
	}
	d.PinKind = rec.Pin.Kind
	d.PinName = rec.Name
	d.PinStrength = rec.Pin.Strength
	d.PinDigest = rec.Pin.Digest
	d.PinVersion = rec.Pin.Version
	d.PinDrifted = rec.DriftSummary != ""
	d.PinBypassReason = rec.BypassReason
}

// recordedEnforceMode is the enforcement mode that ACTUALLY applied to this
// decision, which is not always the one the class declared.
//
// A slot-precondition denial stamps Result.EnforceOverride = EnforceAlways at
// dispatch — that stamp is the whole reason the call did not proceed under
// toolCalls.mode: permissive — while perm.Check.EnforceMode is whatever the
// class wrote, usually empty. Recording the declaration would leave the audit
// saying the denial was disableable by an operator setting when it was not, and
// the record is the only thing left to read months later.
//
// A pure helper (no I/O, no side effects) for the same reason applyProvenance
// is one: it can be unit-tested without standing up the memory layer.
func recordedEnforceMode(perm authz.Permission, res authz.Result) string {
	if res.EnforceOverride != "" {
		return string(res.EnforceOverride)
	}
	if perm.Check == nil {
		return ""
	}
	return string(perm.Check.EnforceMode)
}

// recordAuthzDecision writes the per-tool authz decision to the authzdecision
// memory kind. nil-safe via the Mem/Check guards; the hook calls it exactly once
// per checked tool call.
func (l *Loop) recordAuthzDecision(ctx context.Context, toolName string, perm authz.Permission, res authz.Result, in authz.Inputs) {
	if l.Mem == nil || perm.Check == nil {
		return
	}
	scope := memory.Scope{Kind: "session", ID: l.SessionKey.Namespace + "/" + l.SessionKey.Name}
	useID := ""
	if ids, ok := sandbox.IDsFromCtx(ctx); ok {
		useID = ids.ToolUseID
	}
	// The resolved instance, not just its type. Without it the log says a
	// contact_access check happened on SOME crm_company and an auditor cannot
	// tell which — and authzdecision.DeniesForResource, which filters on the
	// for_resource link, matches nothing at all.
	//
	// Resolved the same way the Check resolved it, so the record describes the
	// decision that was actually made. A resolution failure means the Check
	// denied before it had an id; record the decision with an empty id rather
	// than dropping the whole audit row.
	resourceID, err := authz.ResolveResourceID(*perm.Check, in.Args)
	if err != nil {
		slog.Default().Info("recordAuthzDecision: resource id did not resolve; recording without it",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
			"tool", toolName, "resourceType", perm.Check.ResourceType, "err", err.Error())
	}

	d := authzdecision.Decision{
		Outcome:      authzdecision.OutcomeOf(res.Outcome == authz.OutcomeAllowed),
		Subject:      in.Subject,
		ResourceType: perm.Check.ResourceType,
		ResourceID:   resourceID,
		Permission:   perm.Check.Permission,
		EnforceMode:  recordedEnforceMode(perm, res),
		Message:      res.Message,
	}
	if l.Provenance != nil {
		rec, ok := l.Provenance.Get(toolName)
		applyProvenance(&d, rec, ok)
	}
	if err := authzdecision.Record(ctx, l.Mem, scope, useID, d); err != nil {
		slog.Default().Info("authzdecision.Record",
			"session", scope.ID, "err", err.Error())
	}
}

// loopLookuper adapts the Loop's SpiceDBLookupSubjects (resource, permission)
// signature to the authz.Lookuper interface (a COMBINED "type:id#rel"
// subjectRef), so the raise-time approver resolution reuses
// authz.ResolveApprovers rather than re-implementing the eligibility rule here.
type loopLookuper struct {
	fn func(ctx context.Context, resource, permission string) ([]string, error)
}

func (a loopLookuper) LookupSubjects(ctx context.Context, subjectRef string) ([]string, error) {
	resource, perm, _ := strings.Cut(subjectRef, "#")
	return a.fn(ctx, resource, perm)
}

// LookupInteractSubjects/LookupSubjectIncludes are part of authz.Lookuper but
// unused by ResolveApprovers; satisfy the interface with errors so a misuse is
// loud rather than silently wrong.
func (a loopLookuper) LookupInteractSubjects(context.Context, string, string) ([]string, error) {
	return nil, fmt.Errorf("loopLookuper: LookupInteractSubjects not supported")
}

func (a loopLookuper) LookupSubjectIncludes(context.Context, string, identity.CanonicalUserID) (bool, error) {
	return false, fmt.Errorf("loopLookuper: LookupSubjectIncludes not supported")
}

// sensitiveArgDeclarer is the optional interface a tool implements to declare
// which of its argument paths carry secrets. MCP tools satisfy it from
// MCPServer.spec.tools[].args.sensitiveFields; tools with no such concept
// (sandbox, meta) simply do not implement it.
//
// The approval card cannot reuse pkg/tools/mcp/validator's Decision for this:
// that runs inside Execute, AFTER the human has already been shown the call — so
// the pre-dispatch path reads the declaration directly.
type sensitiveArgDeclarer interface {
	SensitiveArgFields() []string
}

// redactSensitiveArgs returns the args to SHOW: a copy of argsMap with every
// path the tool declares sensitive replaced by a redaction token, using the same
// walker pkg/tools/mcp/validator applies to its Decision (whole-value
// replacement, so no byte of a declared-sensitive value survives serialization
// whatever its JSON shape).
//
// Returns argsMap unchanged — no copy, no allocation — when the tool declares
// nothing. The caller's map is never mutated: redaction happens on a deep copy,
// because the raw values still have to authorize and execute the call.
//
// sessionKey and toolName exist only for the log line: a declared path that
// does not parse is an authoring bug in the MCPServer spec, and this is the
// one layer that knows WHICH session and tool showed a human an over-redacted
// card because of it. The redactor itself has already failed closed.
func redactSensitiveArgs(t tool.Tool, argsMap map[string]any, sessionKey, toolName string) map[string]any {
	d, ok := t.(sensitiveArgDeclarer)
	if !ok {
		return argsMap
	}
	fields := d.SensitiveArgFields()
	if len(fields) == 0 {
		return argsMap
	}
	out := redact.DeepCopyJSONMap(argsMap)
	r := redact.New()
	for _, path := range fields {
		if err := redact.RedactValueAtPath(out, path, r, path); err != nil {
			slog.Default().Info("approval card: declared sensitive arg path did not parse",
				"session", sessionKey, "tool", toolName, "path", path, "err", err.Error())
		}
	}
	return out
}

// buildToolCallApprovalAsk constructs the ApprovalAsk for a tool_call gate.
// useID is the LLM tool_use block ID; justification is the agent's stated reason
// from the _reason envelope field. reqSubject + proxyExec drive the
// approver-facing Requester: on a proxy-exec (app-tool) call it is the widget
// VIEWER (reqSubject); on the LLM path it is the session's LastInbound requester.
func (l *Loop) buildToolCallApprovalAsk(ctx context.Context, toolName string, argsMap map[string]any, perm authz.Permission, useID, justification string, reqSubject identity.CanonicalUserID, proxyExec bool) (*pipeline.ApprovalAsk, error) {
	resourceType := ""
	resourceID := ""
	permission := ""
	var grantBindsArgs []string
	if perm.Check != nil {
		resourceType = perm.Check.ResourceType
		permission = perm.Check.Permission
		grantBindsArgs = perm.Check.GrantBindsArgs
		// Fail closed: an unresolvable resource id would leave resourceID
		// empty, which the owner-set block below reads as "this permission
		// names no resource" and routes the approval to the SESSION
		// approve-set instead of the resource's owners. For
		// stateImpact: external that mis-routed approval is the only
		// authorization the call ever gets (toolCallPostApprove skips the
		// re-check for External), so an approver with no standing over the
		// resource would be the one authorizing it. Refusing loses no call that
		// would otherwise succeed: readonly/readwrite are already denied by
		// CheckToolCall on the same resolution failure.
		resolved, err := authz.ResolveResourceID(*perm.Check, argsMap)
		if err != nil {
			slog.Default().Info("approval flow: resource id unresolvable; refusing to route the approval",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
				"tool", toolName, "resourceType", resourceType, "err", err.Error())
			return nil, fmt.Errorf("approval flow: cannot resolve the %s resource for tool %q: %w", resourceType, toolName, err)
		}
		resourceID = resolved
	}

	argsHash := grants.ArgsHashFiltered(l.ArgsHashKey, argsMap, grantBindsArgs)

	// Everything an approver, the summarizer LLM, or the durable approval record
	// sees is built from a REDACTED copy: the raw argsMap is the LLM's
	// unvalidated input, and pkg/tools/mcp/validator's scrub only runs inside
	// Execute, on its own deep copy, long after this card is published. The raw
	// map still travels on the payload (args_map) because it stays in-process —
	// toolCallPostApprove re-checks and the grant write resolve off it, and
	// argsHash above must bind the values that will actually execute.
	//
	// lookupTool spans l.Tools AND l.AppTools so a side-effecting app-tool's
	// approval card gets its real description/summary rather than a blank one.
	var toolDesc string
	var foundTool tool.Tool
	if t, ok := l.lookupTool(toolName); ok {
		toolDesc = t.Description()
		foundTool = t
	}
	displayArgs := redactSensitiveArgs(foundTool, argsMap, l.SessionKey.Namespace+"/"+l.SessionKey.Name, toolName)
	argsJSON, _ := json.Marshal(displayArgs)

	// Raise-time approver resolution: when the permission Check names a resource,
	// the eligible approvers are that resource's OWNERS — the counterparty whose
	// consent this gate solicits. The requester's consent is implicit in the call
	// itself, so session standing is deliberately NOT also required (see
	// authz.ResolveApprovers: intersecting `session owners ∩ resource owners`
	// here made every requester≠owner call unapprovable). Without a resource the
	// session approve-set is the pool. Fail-closed ("no one has standing to
	// approve") when the pool is empty, BEFORE prompting; click-time enforcement
	// (channelsd CheckApproverAuthorized) is the backstop. Only computed when
	// SpiceDBLookupSubjects is wired, so unit tests without SpiceDB still build
	// asks.

	// Who may approve is DECLARED by the resource type, never assumed.
	//
	//   - standing `required`   -> the pool is the declared approverPermission on
	//     this instance. An empty pool is a real refusal: SpiceDB governs the
	//     type, so nobody holding the permission means nobody may approve.
	//   - standing `session-only` -> no local permission governs it, so the
	//     session's own approvers decide and their decision IS the authority.
	//   - undeclared            -> refuse. The class published no answer for this
	//     type and there is no safe assumption: guessing session-only widens who
	//     may approve, guessing required dead-ends the tool forever.
	//
	// This replaced "<type>:<id>#owner" hardcoded here. That made crm_company,
	// whose `owner` is a real computed permission backed by seeded tuples, look
	// identical to git_repo, whose `owner` is a bare relation declared only so the
	// checks are answerable and never populated. The router could not tell
	// governance from schema shape, so a push to a git remote resolved to an empty
	// set and died with "no one has standing to approve" — observed live
	// 2026-08-21 on an approved two-phase plan.
	var resourceOwnerSets []string
	if perm.Check != nil {
		set, ok := approverSetFor(l.ResourceStandings, resourceType, resourceID)
		if !ok {
			return nil, fmt.Errorf("approval flow: resource type %q declares no standing, so there is no way to know who may approve tool %q; declare standing on the type's schema fragment", resourceType, toolName)
		}
		if set != "" {
			resourceOwnerSets = []string{set}
		}
	}
	sessionApproveSet := "agentsession:" + l.SessionKey.Namespace + "/" + l.SessionKey.Name + "#approve"
	if l.SpiceDBLookupSubjects != nil {
		_, empty, err := authz.ResolveApprovers(ctx, loopLookuper{l.SpiceDBLookupSubjects}, sessionApproveSet, resourceOwnerSets)
		if err != nil {
			return nil, fmt.Errorf("approval flow: resolve approvers for tool %q: %w", toolName, err)
		}
		if empty {
			if len(resourceOwnerSets) > 0 {
				return nil, fmt.Errorf("approval flow: no one has standing to approve tool %q (%s has no subjects to route the approval to)", toolName, resourceOwnerSets[0])
			}
			return nil, fmt.Errorf("approval flow: no one has standing to approve tool %q (session has no approvers)", toolName)
		}
	}
	// Delivery routes to the same pool eligibility was computed from: the
	// resource owner-set when present, else the session approve-set.
	approverSubject := sessionApproveSet
	if len(resourceOwnerSets) > 0 {
		approverSubject = resourceOwnerSets[0]
	}

	// Requester attribution: on a proxy-exec (app-tool) call the approver must see
	// the WIDGET VIEWER ("alice's widget wants to delete X"), not the session's
	// LastInbound requester. The viewer's canonical subject is
	// "user:<base64url(email)>"; DecodeForDisplay recovers the email, mirroring
	// interact/mcp_ui_action.go's round-trip. The LLM path keeps the
	// channel-native LastInbound display.
	var requester channelevents.ExternalIdentity
	// On a proxy-exec the post-approval re-verify must re-check
	// the VIEWER (reqSubject), not the loop's session subject — thread the viewer
	// principal onto the payload → pendingToolCall → toolCallPostApprove (D-D1).
	// Left empty on the LLM path so toolCallPostApprove keeps its l.authSubject
	// fallback (unchanged).
	var checkSubject string
	var checkSubjects []string
	if proxyExec {
		email := identity.DecodeForDisplay(reqSubject.String())
		requester = channelevents.ExternalIdentity{
			Kind:       "idp",
			Email:      identity.Email(email),
			ExternalID: identity.RawExternalID(email),
		}
		checkSubject = reqSubject.String()
		checkSubjects = []string{reqSubject.String()}
	} else {
		requesterExt := l.LastInboundExternalID
		if requesterExt == "" && l.AgentSession != nil {
			requesterExt = l.AgentSession.Annotations[spiceboxv1alpha1.AnnotationStartedByExternalID]
		}
		requester = channelevents.ExternalIdentity{
			Kind:       identity.Kind(l.AddressableChannelKind()),
			ExternalID: identity.RawExternalID(requesterExt),
		}
	}

	// Build summary via the summarizer LLM, from the same redacted args the
	// card shows: the summarizer is a second LLM whose whole purpose is to
	// describe the call to a human, so it has no more business seeing a
	// declared-sensitive value than the human does.
	var summary string
	if foundTool != nil {
		summary = l.summarizeToolCall(ctx, foundTool, displayArgs, permission, resourceType, resourceID)
	}

	var labels map[string]string
	if l.LabelStore != nil {
		labels = l.LabelStore.SnapshotForArgs(argsMap, resourceType, resourceID)
	}

	payload := ToolCallApprovalPayload{
		SessNS:           l.SessionKey.Namespace,
		SessName:         l.SessionKey.Name,
		ToolName:         toolName,
		ToolDescription:  toolDesc,
		Permission:       permission,
		ResourceType:     resourceType,
		ResourceID:       resourceID,
		StateImpact:      string(perm.StateImpact),
		ArgsMap:          argsMap,
		ArgsJSON:         string(argsJSON),
		ArgsHash:         argsHash,
		Perm:             perm,
		ApproverSubject:  approverSubject,
		Requester:        requester,
		AgentDisplayName: l.agentClassDisplayName(),
		Labels:           labels,
		UseID:            useID,
		Justification:    justification,
		Subject:          checkSubject,
		Subjects:         checkSubjects,
	}

	return &pipeline.ApprovalAsk{
		Kind:      "tool_call",
		Summary:   summary,
		Timeout:   l.resolvedApprovalTimeout(),
		OnTimeout: timeoutPolicyFor("tool_call"),
		Payload:   payload.ToMap(),
	}, nil
}

// buildPreconditionWaiverAsk constructs the waiver ApprovalAsk for a slot
// precondition that REFUSED the call the agent tried to make — the ONE
// precondition verdict a human may consent past (spec §7). It is the raise-time
// half; buildPreconditionWaiverPending (host_approval.go) is the publish-time
// half, and the two resolve their approver set through the SAME
// preconditionWaiverApproverSets helper so they cannot route to different clickers.
//
// The card body is the gate-authored denial.Message (the CRD RefusalMessage),
// carried as a TRUSTED field — never the agent's justification (agent-authored,
// prompt-injectable) and never a raw fact value (out of a signed payload or a
// tool result, so equally untrusted). The Kind is categories.PreconditionWaiver
// (RULING P3-1), so approving it goes through the waiver handler that binds the
// slot grant, not the tool_call grant path.
func (l *Loop) buildPreconditionWaiverAsk(ctx context.Context, in pipeline.Input, argsMap map[string]any, perm authz.Permission, denial *authz.PreconditionDenial) (*pipeline.ApprovalAsk, error) {
	if denial == nil {
		// The hook calls this only for a Refused precondition, which is always a
		// non-nil denial. A nil here is a programming error the agent cannot fix —
		// fail loud rather than publish an empty card.
		return nil, fmt.Errorf("precondition waiver: nil denial for tool %q (programming error)", in.Tool.Name)
	}
	toolName := in.Tool.Name
	resourceType := ""
	resourceID := ""
	permission := ""
	var grantBindsArgs []string
	if perm.Check != nil {
		resourceType = perm.Check.ResourceType
		permission = perm.Check.Permission
		grantBindsArgs = perm.Check.GrantBindsArgs
		// Fail closed on an unresolvable id, exactly as buildToolCallApprovalAsk:
		// an empty resourceID would bind the waiver's grant to no instance and
		// route consent to the session approve-set instead of the refused
		// resource's owners. Refusing loses no call that would otherwise succeed —
		// the check already denied it. Resolved the SAME normalized way the check
		// resolved it, so the grant binds the instance the human was shown.
		resolved, err := authz.ResolveResourceID(*perm.Check, argsMap)
		if err != nil {
			slog.Default().Info("precondition waiver: resource id unresolvable; refusing to route the waiver",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
				"tool", toolName, "resourceType", resourceType, "err", err.Error())
			return nil, fmt.Errorf("precondition waiver: cannot resolve the %s resource for tool %q: %w", resourceType, toolName, err)
		}
		resourceID = resolved
	}

	// Approver resolution: the refusing rule's declared Approvers when set, else
	// the resource's resolved standing. The SAME helper the publish half calls.
	resourceOwnerSets, declared := preconditionWaiverApproverSets(denial.Approvers, l.ResourceStandings, resourceType, resourceID)
	if !declared {
		return nil, fmt.Errorf("precondition waiver: resource type %q declares no standing, so there is no way to know who may waive tool %q", resourceType, toolName)
	}
	sessionApproveSet := "agentsession:" + l.SessionKey.Namespace + "/" + l.SessionKey.Name + "#approve"
	if l.SpiceDBLookupSubjects != nil {
		_, empty, err := authz.ResolveApprovers(ctx, loopLookuper{l.SpiceDBLookupSubjects}, sessionApproveSet, resourceOwnerSets)
		if err != nil {
			return nil, fmt.Errorf("precondition waiver: resolve approvers for tool %q: %w", toolName, err)
		}
		if empty {
			if len(resourceOwnerSets) > 0 {
				return nil, fmt.Errorf("precondition waiver: no one has standing to waive tool %q (%s has no subjects to route the waiver to)", toolName, strings.Join(resourceOwnerSets, ", "))
			}
			return nil, fmt.Errorf("precondition waiver: no one has standing to waive tool %q (session has no approvers)", toolName)
		}
	}

	argsHash := grants.ArgsHashFiltered(l.ArgsHashKey, argsMap, grantBindsArgs)

	// Everything an approver sees is built from a REDACTED copy: the raw argsMap
	// is the LLM's unvalidated input, exactly as buildToolCallApprovalAsk guards.
	var toolDesc string
	var foundTool tool.Tool
	if t, ok := l.lookupTool(toolName); ok {
		toolDesc = t.Description()
		foundTool = t
	}
	displayArgs := redactSensitiveArgs(foundTool, argsMap, l.SessionKey.Namespace+"/"+l.SessionKey.Name, toolName)
	argsJSON, _ := json.Marshal(displayArgs)

	return &pipeline.ApprovalAsk{
		Kind:    categories.PreconditionWaiver,
		Summary: denial.Message,
		Timeout: l.resolvedApprovalTimeout(),
		// TimeoutDeny (the default decisionKindFor path): a lapsed waiver leaves
		// the call DENIED, which is correct per spec §7 — silence is not consent
		// to override a fact-gated refusal.
		OnTimeout: timeoutPolicyFor(categories.PreconditionWaiver),
		Payload: map[string]any{
			"sess_ns":                l.SessionKey.Namespace,
			"sess_name":              l.SessionKey.Name,
			"tool_name":              toolName,
			"tool_description":       toolDesc,
			"permission":             permission,
			"resource_type":          resourceType,
			"resource_id":            resourceID,
			"state_impact":           string(perm.StateImpact),
			"args_hash":              argsHash,
			"args_json":              string(argsJSON),
			"use_id":                 in.Tool.UseID,
			"refusal_message":        denial.Message,
			"precondition_approvers": denial.Approvers,
		},
	}, nil
}

// scopeDeps constructs the ScopeDeps for the Scope hook.
func (l *Loop) scopeDeps() hooks.ScopeDeps {
	return hooks.ScopeDeps{
		Enabled: l.AgentClass != nil && l.AgentClass.Spec.GetScope().Enabled,
		LookupReads: func(toolName string) *hooks.ToolReadsDecl {
			if l.LookupToolMapping == nil {
				return nil
			}
			m := l.LookupToolMapping(toolName)
			if m == nil || m.NoTaint || m.Reads == nil {
				return nil
			}
			bypass := m.Reads.BypassRequesterCheck != nil && *m.Reads.BypassRequesterCheck
			return &hooks.ToolReadsDecl{
				ResourceType:         m.Reads.ResourceType,
				Permission:           m.Reads.Permission,
				IDArg:                m.Reads.IDArg,
				ResultIDField:        m.Reads.ResultIDField,
				BypassRequesterCheck: bypass,
			}
		},
		GetScope: func(ctx context.Context) (scope.Scope, bool, error) {
			if l.Mem == nil {
				return scope.Scope{}, false, nil
			}
			return sessionscope.Get(ctx, l.Mem, l.bindingScope())
		},
		Logger: slog.Default(),
	}
}

// memoryPoolReadsDecl is the built-in plural read declaration for the memory
// search tool — the one tool whose single result can come from several
// resources at once.
//
// A session's search spans its own scope plus every resource pool it reached
// through a slot grant (the memory server widens the scope list server-side),
// so one result mixes entries whose audiences differ. The declaration reports
// one resource per POOL, with that pool's slice of the result, and the read
// hook mints one tag per pool from it.
//
// Two choices here are the whole point of the wiring:
//
//   - The permission is view_memory on the RESOURCE, never the permission the
//     session's slot grant named. A session may hold `(customer, read)`; the
//     pool's readers are whoever holds view_memory on that customer. The slot
//     says the agent may work with the resource, and says nothing about who may
//     see the resource's memory.
//   - The session's OWN scope produces no resource, and it is the only scope
//     that may be skipped. Its audience is the session, which the session-wide
//     taint floor already covers; a pool ref for it would claim a per-datum
//     audience that does not exist.
//
// Everything else is REFUSED, because declaring this tool is what takes its
// coarse floor away. Before the declaration existed the tool had no mapping at
// all, so the read hook floored every result it produced
// (agentsession#unknown_provenance — the session's participants). Replacing a
// blanket answer with a precise one obliges the precise one to cover every
// shape the result can carry: another session's entries (the `scopes` argument
// still reaches an in-process searcher), a scope claiming to be a pool whose id
// will not parse, an entry a provider left with no scope at all. Skipping those
// the way the session's own are skipped would leave them with no taint, no tag
// and no audit — strictly less than the floor they replaced, and in the
// under-blocking direction. An error here routes to the hook's unattributable
// path: deny in enforcing, audit in logging.
//
// Built-in rather than CRD-settable (see ToolReadsDecl.ResultResources): the
// split needs the result's format, which package meta owns, and a CRD field
// naming plural arbitrary-typed resources would let a spec author decide the
// audience of its own results.
//
// Returns nil for every other tool, leaving the single-resource path exactly as
// it was. record_observation is the one other name it answers, and with the
// opposite shape — a declared opt-out rather than a declaration; see there.
func (l *Loop) memoryPoolReadsDecl(toolName string) *hooks.ToolReadsDecl {
	if meta.IsGoalTool(toolName) {
		return &hooks.ToolReadsDecl{ResultResources: func(result string) ([]hooks.ToolReadResource, error) {
			var response struct {
				Resource string `json:"resource"`
			}
			if err := json.Unmarshal([]byte(result), &response); err != nil {
				return nil, err
			}
			scope, err := memory.ParseResourceRef(response.Resource)
			if err != nil {
				return nil, err
			}
			typ, id, ok := memory.ResourceRef(scope)
			if !ok || typ != meta.GoalResourceType {
				return nil, hooks.ErrUnattributableResource
			}
			return []hooks.ToolReadResource{{Type: typ, ID: id, Permission: memory.PermissionViewMemory, Content: result}}, nil
		}}
	}

	if toolName == meta.RecordObservationToolName {
		// The declared OPT-OUT, in the same shape the CRD's NoTaint takes
		// (infoleakread.go's "NoTaint equivalent"): a decl carrying only
		// BypassRequesterCheck, never nil.
		//
		// Load-bearing, not tidiness. record_observation reaches the contained
		// pipeline (tool.PipelineRouted), so info_leak_read sees it at
		// PostToolCall — and nil there means UNDECLARED, which floors a
		// `agentsession:<ns>/<name>#unknown_provenance` taint onto the session.
		// That taint is then what the PreToolCall pool-write gate measures on
		// the NEXT call, so the tool's own first write refuses its second:
		// "refusing to send to dossier:d-1, which would newly expose this data
		// to user:bob". The floor exists for a tool that might have brought
		// data IN and did not say so; a write brings nothing in, which is the
		// spec's own ruling that nothing is added at PostToolCall.
		//
		// The audience gate reads the same declaration and answers
		// Decision{} on it (no ResourceType, no ResultResources), so both
		// PostToolCall hooks agree — as they must, per the comment on the
		// audience deps' own LookupReads.
		return &hooks.ToolReadsDecl{BypassRequesterCheck: true}
	}
	if toolName != meta.SearchMemoryToolName {
		return nil
	}
	own := memory.Scope{Kind: "session", ID: l.SessionKey.Namespace + "/" + l.SessionKey.Name}
	return &hooks.ToolReadsDecl{
		ResultResources: func(result string) ([]hooks.ToolReadResource, error) {
			groups, err := meta.SplitSearchResultByScope(result)
			if err != nil {
				return nil, err
			}
			var out []hooks.ToolReadResource
			for _, g := range groups {
				objType, objID, ok := memory.ResourceRef(g.Scope)
				if !ok {
					if g.Scope == own {
						continue
					}
					return nil, unattributableScope(g.Scope)
				}
				out = append(out, hooks.ToolReadResource{
					Type:       objType,
					ID:         objID,
					Permission: memory.PermissionViewMemory,
					Content:    g.Result,
				})
			}
			return out, nil
		},
	}
}

// memoryPoolWritesDecl declares that the observation-recording tool writes
// into the pool named by its `resource` argument.
//
// Built-in only, and for a stronger reason than the read declaration's. A CRD
// field naming a tool's write destination would let a spec author decide where
// the session's data LANDS — not merely what a result's audience is — and the
// audience gate would then compare the session's reads against whatever pool
// that author pointed at. Naming the argument in code keeps the destination a
// property of the tool.
//
// Returns nil for every other tool, which is what leaves the PreToolCall
// pool-write gate inert for everything but this one.
func (l *Loop) memoryPoolWritesDecl(toolName string) *hooks.ToolWritesDecl {
	if toolName != meta.RecordObservationToolName && !meta.IsGoalWrite(toolName) {
		return nil
	}
	return &hooks.ToolWritesDecl{DestinationArg: "resource"}
}

// unattributableScope explains a scope the pool declaration cannot turn into a
// resource ref, naming WHICH of the two failures it is.
//
// The distinction is the one SplitSearchResultByScope's doc comment makes in
// the other direction: "no pools" and "I could not tell" must not be the same
// answer. A scope that CLAIMS to be a pool and will not parse is a defect in
// whatever wrote it — today only pools.Reader, whose inputs are SpiceDB object
// refs, but a future pool-write path is exactly where a malformed one would
// come from — while any other Kind is a scope this session was not expecting to
// read at all. An operator needs to know which.
//
// Both wrap hooks.ErrUnattributableResource, which is what tells the read hook
// this is a REFUSAL of a result it read rather than a result it could not read.
// Without the mark, an upstream setting isError on the call would excuse the
// refusal entirely — the gate's off-switch in the other side's hands.
func unattributableScope(s memory.Scope) error {
	if memory.IsResourceScope(s) {
		return fmt.Errorf("search_memory: entry scope claims to be a resource pool but its id %q does not parse: %w",
			s.ID, hooks.ErrUnattributableResource)
	}
	return fmt.Errorf("search_memory: entry scope %q/%q is neither this session's own nor a resource pool: %w",
		s.Kind, s.ID, hooks.ErrUnattributableResource)
}

// infoLeakReadDeps constructs the InfoLeakReadDeps for the InfoLeakRead hook.
func (l *Loop) infoLeakReadDeps() hooks.InfoLeakReadDeps {
	// This adapter converts identity.Subject → string: InfoLeakReadDeps.Requester
	// needs the PREFIXED form for SplitObject, and l.RequesterCanonicalID (despite
	// its name) returns identity.Subject, not a bare canonical. Preserve its
	// nil-ness — the hook gates on "Requester != nil" (infoleakread.go), so a nil
	// l.RequesterCanonicalID must yield a nil Requester here too, not a wrapper
	// that panics when invoked. perCall is threaded straight through from
	// pipeline.Input.Requester: the hook resolves the principal it was HANDED,
	// never one this Loop reads back out of its own mutable state.
	var requester func(ctx context.Context, perCall identity.CanonicalUserID) (string, error)
	if l.RequesterCanonicalID != nil {
		requester = func(ctx context.Context, perCall identity.CanonicalUserID) (string, error) {
			id, err := l.RequesterCanonicalID(ctx, perCall)
			return id.String(), err
		}
	}
	return hooks.InfoLeakReadDeps{
		Mode:       l.LeakageConfig.ResolvedMode(),
		SessionRef: l.SessionKey.Namespace + "/" + l.SessionKey.Name,
		LookupReads: func(toolName string) *hooks.ToolReadsDecl {
			// Built-in declarations first: they belong to framework tools that
			// have no CRD mapping, so a lookup gated on LookupToolMapping being
			// wired would never reach them.
			if d := l.memoryPoolReadsDecl(toolName); d != nil {
				return d
			}
			if l.LookupToolMapping == nil {
				return nil
			}
			m := l.LookupToolMapping(toolName)
			if m == nil {
				return nil
			}
			if m.NoTaint {
				// Declared opt-out. Route to the hook's BYPASS path — a decl with
				// only BypassRequesterCheck set (infoleakread.go's "NoTaint
				// equivalent") — NOT nil. nil now means "undeclared" and falls to
				// the coarse floor; returning it here would floor-taint a tool
				// whose whole point is to opt out of tracking.
				return &hooks.ToolReadsDecl{BypassRequesterCheck: true}
			}
			if m.Reads == nil {
				return nil
			}
			bypass := m.Reads.BypassRequesterCheck != nil && *m.Reads.BypassRequesterCheck
			return &hooks.ToolReadsDecl{
				ResourceType:         m.Reads.ResourceType,
				Permission:           m.Reads.Permission,
				IDArg:                m.Reads.IDArg,
				ResultIDField:        m.Reads.ResultIDField,
				BypassRequesterCheck: bypass,
			}
		},
		Requester:       requester,
		Check:           l.SpiceDBCheck,
		AppendTaint:     l.TaintMemoryAppend,
		MintPtTag:       l.PtTagMint,
		UntrustedSource: l.LookupToolUntrustedSource,
	}
}

// infoLeakAudienceDeps constructs the InfoLeakAudienceDeps for the InfoLeakAudience hook.
func (l *Loop) infoLeakAudienceDeps() hooks.InfoLeakAudienceDeps {
	return hooks.InfoLeakAudienceDeps{
		Mode: l.LeakageConfig.ResolvedMode(),
		LookupReads: func(toolName string) *hooks.ToolReadsDecl {
			// The same built-in declaration info_leak_read sees. Both hooks run
			// at PostToolCall over one result, and a gate that could not see
			// the pools the read hook just tagged would let a pool's entries
			// reach a channel member outside that pool's audience with the tag
			// recorded and never consulted.
			if d := l.memoryPoolReadsDecl(toolName); d != nil {
				return d
			}
			if l.LookupToolMapping == nil {
				return nil
			}
			m := l.LookupToolMapping(toolName)
			if m == nil || m.NoTaint || m.Reads == nil {
				return nil
			}
			bypass := m.Reads.BypassRequesterCheck != nil && *m.Reads.BypassRequesterCheck
			return &hooks.ToolReadsDecl{
				ResourceType:         m.Reads.ResourceType,
				Permission:           m.Reads.Permission,
				IDArg:                m.Reads.IDArg,
				ResultIDField:        m.Reads.ResultIDField,
				BypassRequesterCheck: bypass,
			}
		},
		LookupWrites: l.memoryPoolWritesDecl,
		ResolveAudience: func(ctx context.Context) ([]string, channelkinds.Capability, error) {
			if l.ChannelKindImpl == nil {
				return nil, channelkinds.CapabilityUnsupported, nil
			}
			ar := l.ChannelKindImpl
			info := channelkinds.SessionInfo{
				Namespace:        l.SessionKey.Namespace,
				Name:             l.SessionKey.Name,
				SessionInitiator: l.StartedByCanonical,
			}
			if l.AgentSession != nil && l.AgentSession.Spec.InputChannel != nil {
				info.Channel = l.AgentSession.Spec.InputChannel
			}
			subs, err := ar.ResolveAudience(ctx, info)
			if err != nil {
				return nil, ar.AudienceCapability(), err
			}
			return subs, ar.AudienceCapability(), nil
		},
		LookupSubjects: l.SpiceDBLookupSubjects,
		TaintList:      l.TaintMemoryList,
		FineGrained:    l.fineGrainedLeakageDeps(),
		BuildApprovalAsk: func(ctx context.Context, leakedTo []string, taint []infoleakagetaint.TaintRecord, proposedText string) (*pipeline.ApprovalAsk, error) {
			return l.buildLeakageApprovalAsk(ctx, leakedTo, taint, proposedText)
		},
		OnUnsupportedChannel: l.LeakageConfig.ResolvedOnUnsupportedChannel(),
		SingleUserBypass:     l.LeakageConfig.ResolvedSingleUserBypass(),
		// Notice-only mode: "logging" mode's would-block event delivers a
		// read-only FYI to the requester via the generic interaction model
		// (categories.InfoLeakageNotice), gated by its own CRD knob.
		NoticeToRequester: l.LeakageConfig.ResolvedLoggingNoticeToRequester(),
		PublishNotice: func(ctx context.Context, requester identity.CanonicalUserID, leakedTo []string, taint []infoleakagetaint.TaintRecord) error {
			return l.publishLeakageNotice(ctx, requester, leakedTo, taint)
		},
		// Decision store: durable per-session leakage approve/deny set
		// (infoleakage_decision kind) so a denial survives a runner restart and
		// is shared across hook instances over the same session scope.
		RecordDecision: func(ctx context.Context, resourceType, resourceID, decision string) error {
			if l.Mem == nil {
				return nil
			}
			return infoleakagedecision.Record(ctx, l.Mem, l.bindingScope(), decision, resourceType, resourceID)
		},
		IsApproved: func(ctx context.Context, resourceType, resourceID string) (bool, error) {
			if l.Mem == nil {
				return false, nil
			}
			return infoleakagedecision.IsApproved(ctx, l.Mem, l.bindingScope(), resourceType, resourceID)
		},
		IsDenied: func(ctx context.Context, resourceType, resourceID string) (bool, error) {
			if l.Mem == nil {
				return false, nil
			}
			return infoleakagedecision.IsDenied(ctx, l.Mem, l.bindingScope(), resourceType, resourceID)
		},
		Logger: slog.Default(),
	}
}

// buildLeakageApprovalAsk builds the ApprovalAsk for a leakage_share gate.
func (l *Loop) buildLeakageApprovalAsk(ctx context.Context, leakedTo []string, taint []infoleakagetaint.TaintRecord, proposedText string) (*pipeline.ApprovalAsk, error) {
	// Raise-time approver resolution (see buildToolCallApprovalAsk and the
	// rationale on authz.ResolveApprovers): the eligible approvers are the
	// tainted data's OWNERS — the union across the tainted resources' owner
	// sets; any one owner vouches for the share. Session standing is
	// deliberately NOT required. Fail-closed when no tainted resource has any
	// owner, BEFORE prompting. Skipped without SpiceDB wired.
	resourceOwnerSets := make([]string, 0, len(taint))
	seenOwnerSets := make(map[string]struct{}, len(taint))
	for _, tr := range taint {
		// Same declared-not-assumed rule as the tool-call path: the subject-set
		// comes from the type's approverPermission, never a hardcoded "#owner".
		//
		// A tainted resource whose type is session-only contributes NO set — no
		// local permission governs it, so it cannot vouch for a share and the
		// session approve-set is what remains. An UNDECLARED type is refused
		// outright: leaking data governed by a type nobody classified is the last
		// place to guess.
		set, declared := approverSetFor(l.ResourceStandings, tr.ResourceType, tr.ResourceID)
		if !declared {
			return nil, fmt.Errorf("info-leakage: tainted resource type %q declares no standing, so there is no way to know who may vouch for sharing it", tr.ResourceType)
		}
		if set == "" {
			continue
		}
		if _, dup := seenOwnerSets[set]; dup {
			continue
		}
		seenOwnerSets[set] = struct{}{}
		resourceOwnerSets = append(resourceOwnerSets, set)
	}
	sessionApproveSet := "agentsession:" + l.SessionKey.Namespace + "/" + l.SessionKey.Name + "#approve"
	if l.SpiceDBLookupSubjects != nil {
		_, empty, err := authz.ResolveApprovers(ctx, loopLookuper{l.SpiceDBLookupSubjects}, sessionApproveSet, resourceOwnerSets)
		if err != nil {
			return nil, fmt.Errorf("info-leakage: resolve approvers: %w", err)
		}
		if empty {
			if len(resourceOwnerSets) > 0 {
				return nil, fmt.Errorf("info-leakage: no one has standing to approve (no tainted resource has an owner: %s)", strings.Join(resourceOwnerSets, ", "))
			}
			return nil, fmt.Errorf("info-leakage: no one has standing to approve (session has no approvers)")
		}
	}
	// Delivery routes to ALL data owner-sets when present (union, any one may
	// approve; the channel kind fans out and caps), else the session
	// approve-set. approverSubjects carries the full list; approverSubject
	// stays the first ref for older consumers that read only the singular.
	approverSubjects := []string{sessionApproveSet}
	if len(resourceOwnerSets) > 0 {
		approverSubjects = resourceOwnerSets
	}
	approverSubject := approverSubjects[0]

	ttl := l.resolvedLeakageApprovalTTL()

	// Encode taint as leakageTaintRecord slice for the host.
	taintRecs := make([]leakageTaintRecord, 0, len(taint))
	for _, t := range taint {
		taintRecs = append(taintRecs, leakageTaintRecord{
			ResourceType: t.ResourceType,
			ResourceID:   t.ResourceID,
			Permission:   t.Permission,
			ToolName:     t.ToolName,
		})
	}

	// on_approved / on_denied let the host record the per-resource decision back
	// into the InfoLeakAudience hook's session-scoped sets after the approver
	// acts, so the respond-time gate doesn't re-prompt for a resource the
	// pre-feed gate already resolved.
	var onApproved, onDenied func(resourceType, resourceID string)
	if l.infoLeakAudienceHook != nil {
		onApproved = l.infoLeakAudienceHook.RecordApproved
		onDenied = l.infoLeakAudienceHook.RecordDenied
	}

	return &pipeline.ApprovalAsk{
		Kind:      "leakage_share",
		Summary:   "",
		Timeout:   l.resolvedApprovalTimeout(),
		OnTimeout: timeoutPolicyFor("leakage_share"),
		Payload: map[string]any{
			"leaked_to":         leakedTo,
			"taint":             taintRecs,
			"ttl":               ttl.String(),
			"proposed_text":     proposedText,
			"approver":          approverSubject,
			"approver_subjects": approverSubjects,
			"on_approved":       onApproved,
			"on_denied":         onDenied,
		},
	}, nil
}

// effectiveAuthz returns the authz windows pkg/platform/settings resolved across
// the four settings tiers, which the AgentSession reconciler stamps onto
// status.effectiveSettings before this pod exists — or nil when the Loop has no
// stamped session (kubectl-driven callers, the in-process harness, tests).
//
// Reading this rather than the AgentClass is the whole point: the class is only
// ONE of the four tiers, so gating on it leaves a cluster or namespace admin's
// tightened defaults.authz inert while status still reports it as enforced.
func (l *Loop) effectiveAuthz() *spiceboxv1alpha1.EffectiveAuthz {
	if l.AgentSession == nil || l.AgentSession.Status.EffectiveSettings == nil {
		return nil
	}
	return &l.AgentSession.Status.EffectiveSettings.Authz
}

// resolvedApprovalTimeout returns the consolidated authz.approvalTimeout for
// this session: the resolved 4-tier value, else the class, else 10m.
func (l *Loop) resolvedApprovalTimeout() time.Duration {
	if a := l.effectiveAuthz(); a != nil && a.ApprovalTimeout.Duration > 0 {
		return a.ApprovalTimeout.Duration
	}
	if l.AgentClass == nil {
		return (*spiceboxv1alpha1.AuthzBlock)(nil).ResolvedApprovalTimeout()
	}
	return l.AgentClass.Spec.GetAuthz().ResolvedApprovalTimeout()
}

// resolvedLeakageApprovalTTL returns how long an approved information-leakage
// share may be reused before the agent must ask again: the resolved 4-tier
// value, else the class policy, else 10m.
func (l *Loop) resolvedLeakageApprovalTTL() time.Duration {
	if a := l.effectiveAuthz(); a != nil && a.InformationLeakageApprovalTTL.Duration > 0 {
		return a.InformationLeakageApprovalTTL.Duration
	}
	return l.LeakageConfig.ResolvedApprovalTTL()
}

// resolvedScopeLLMLatency returns the cold-start extractor+composer latency
// budget in milliseconds: the resolved 4-tier value, else the class scope
// block, else 5000.
func (l *Loop) resolvedScopeLLMLatency() int32 {
	if a := l.effectiveAuthz(); a != nil && a.ScopeMaxLLMLatencyMs > 0 {
		return a.ScopeMaxLLMLatencyMs
	}
	var maxMs int32
	if l.AgentClass != nil {
		maxMs = l.AgentClass.Spec.GetScope().MaxLLMLatencyMs
	}
	if maxMs <= 0 {
		maxMs = 5000
	}
	return maxMs
}

// engineChecker adapts the Loop's Engine/AuthzCli into the ToolCallChecker interface.
type engineChecker struct{ l *Loop }

func (e *engineChecker) CheckToolCall(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result {
	if e.l.Engine != nil {
		return e.l.Engine.CheckToolCall(ctx, p, in)
	}
	return toolcheck.Checker{Cli: e.l.AuthzCli, Cache: e.l.AuthzCache}.CheckToolCall(ctx, p, in)
}

// toolGuardLookup maps a tool name to (kind, origin) for the guard hooks,
// spanning l.Tools AND l.AppTools (via lookupTool) so the circuit breaker keys
// on an app-tool's real origin. origin is "" for origin-less tools (sandbox,
// meta); ("", "") for unknown names.
func (l *Loop) toolGuardLookup() func(name string) (string, string) {
	return func(name string) (string, string) {
		t, ok := l.lookupTool(name)
		if !ok {
			return "", ""
		}
		origin := ""
		if ot, ok := t.(tool.OriginTool); ok {
			origin = ot.Origin()
		}
		return string(t.Kind()), origin
	}
}

// lookupOrigin maps a tool name to its Origin() string for the revocation
// guard, reusing the same tool source + name-matching as toolGuardLookup
// (l.Tools + l.AppTools) so the two guards agree on what each name is.
// Returns "" for origin-less tools (sandbox, meta) and unknown names.
func (l *Loop) lookupOrigin() func(toolName string) string {
	lookup := l.toolGuardLookup()
	return func(toolName string) string {
		_, origin := lookup(toolName)
		return origin
	}
}

// originRevokedNow answers "is this tool's origin revoked AT THIS INSTANT",
// for the re-check executeToolContained runs immediately before a call runs.
//
// The revocation guard is a PreToolCall hook, and an approval await sits
// between it and the run — so a revocation landing while a human looked at the
// card was recorded and then ignored by the call it was aimed at, for a window
// bounded only by the approval timeout. This is a live read of an in-process
// set, so re-asking costs nothing.
//
// Returns ("", false) when revocation is not wired at all or the tool has no
// origin (a meta or sandbox tool belongs to no upstream), which is the same
// answer the guard itself gives for those.
func (l *Loop) originRevokedNow(toolName string) (string, bool) {
	if l.RevokedOrigins == nil {
		return "", false
	}
	origin := l.lookupOrigin()(toolName)
	if origin == "" {
		return "", false
	}
	return origin, l.RevokedOrigins.IsRevoked(origin)
}

func (l *Loop) toolGuardDeps() toolguard.GuardDeps {
	return toolguard.GuardDeps{
		Policy:     l.ToolGuardPolicy,
		Registry:   l.toolGuardReg,
		LookupTool: l.toolGuardLookup(),
		TurnIndex: func(ctx context.Context) int {
			ids, _ := sandbox.IDsFromCtx(ctx)
			return ids.TurnIndex
		},
		RecordAudit: l.recordToolGuardEvent,
		Logger:      slog.Default(),
	}
}

func (l *Loop) toolGuardRecordDeps() toolguard.RecordDeps {
	return toolguard.RecordDeps{
		Policy:      l.ToolGuardPolicy,
		Registry:    l.toolGuardReg,
		LookupTool:  l.toolGuardLookup(),
		RecordAudit: l.recordToolGuardEvent,
		PatchStatus: l.patchToolGuardStatus,
		Logger:      slog.Default(),
	}
}

// recordToolGuardEvent writes one toolguard event to the toolguard_audit
// memory kind. nil-safe on Mem; errors are logged, never returned (audit is
// best-effort, the gate decision already happened).
func (l *Loop) recordToolGuardEvent(ctx context.Context, ev toolguard.Event) {
	if l.Mem == nil {
		return
	}
	scope := memory.Scope{Kind: "session", ID: l.SessionKey.Namespace + "/" + l.SessionKey.Name}
	// Suppress the "0s" cool-off noise on non-transition events (deny/warn/halt
	// carry no cool-off); only breaker_opened/half_open events set CoolOff > 0.
	coolOff := ""
	if ev.CoolOff > 0 {
		coolOff = ev.CoolOff.String()
	}
	if err := toolguardaudit.Record(ctx, l.Mem, scope, toolguardaudit.Content{
		Event: ev.Event, Tool: ev.Tool, Origin: ev.Origin, Key: ev.Key,
		UseID: ev.UseID, Trips: ev.Trips, CoolOff: coolOff,
		RetryAt: ev.RetryAt, Limit: ev.Limit, ObservedBytes: ev.ObservedBytes, Action: ev.Action,
		Provenance: ev.Provenance,
	}); err != nil {
		slog.Default().Info("toolguardaudit.Record",
			"session", scope.ID, "tool", ev.Tool, "event", ev.Event, "err", err.Error())
	}
}

// patchToolGuardStatus pushes the open-breaker snapshot onto
// AgentSession.status.toolGuard. Best-effort with logging.
func (l *Loop) patchToolGuardStatus(ctx context.Context, snap []toolguard.OpenBreakerInfo) {
	if l.Status == nil {
		return
	}
	tg := &spiceboxv1alpha1.ToolGuardStatus{}
	for _, ob := range snap {
		tg.OpenBreakers = append(tg.OpenBreakers, spiceboxv1alpha1.OpenBreaker{
			Key:      ob.Key,
			OpenedAt: metav1.NewTime(ob.OpenedAt),
			RetryAt:  metav1.NewTime(ob.RetryAt),
			Trips:    ob.Trips,
		})
	}
	if err := l.Status.PatchToolGuard(ctx, tg); err != nil {
		slog.Default().Info("toolguard: status patch failed",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
	}
}

// contentInspectorID returns a stable label for the idx-th inspector for audit.
// (Instances don't carry their ID; the runner tracks the parallel ID slice.)
func (l *Loop) contentInspectorID(idx int) string {
	if idx < len(l.ContentInspectorIDs) {
		return l.ContentInspectorIDs[idx]
	}
	return "content_guard"
}

// planGateRecorder adapts the Loop's memory handle to the hook's recorder
// interface. The gate holds its own recorder rather than emitting
// pipeline.AuditRecords because those carry map[string]any Fields, which would
// erase plangateaudit.Content's typing — the same reason the authzdecision and
// approval families write directly.
type planGateRecorder struct{ l *Loop }

func (r planGateRecorder) Record(ctx context.Context, c plangateaudit.Content) error {
	if r.l.Mem == nil {
		return nil
	}
	scope := memory.Scope{Kind: "session", ID: r.l.SessionKey.Namespace + "/" + r.l.SessionKey.Name}
	return plangateaudit.Record(ctx, r.l.Mem, scope, c)
}

// FreezeAndRecordPhases is the runner's OnPhasesDeclared: it turns the agent's
// authored phase list into the frozen plan the gate reads.
//
// Under `logging` this records what WOULD have been approved — the frozen list,
// its tier, and the card — without publishing anything or asking anyone. That
// is the dataset that decides whether enforcement can be switched on by
// default: it answers, from real sessions, how often a plan would have been
// wrong before anyone is blocked by one.
func (l *Loop) FreezeAndRecordPhases(ctx context.Context, authored []plangate.AuthoredPhase) ([]string, error) {
	frozen, problems := plangate.FreezeFrom(authored, l.PlanGateSurface, l.PlanGateSlotTypes)
	var notices []string
	for _, p := range problems {
		// A dropped handle or edge changes what the human would be approving,
		// so it is never silent.
		slog.Default().Info("plan_gate: dropped part of a declared phase",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
			"phase", p.PhaseIndex, "detail", p.Detail)
		notices = append(notices, fmt.Sprintf("phase %d: %s", p.PhaseIndex, p.Detail))
	}
	// Returned to the CALLER, which hands them back to the agent on the
	// update_plan result. The log alone reaches an operator reading it later;
	// it never reaches the party that can fix the plan. An agent whose handles
	// were all dropped holds a phase with an EMPTY ceiling and does not know —
	// under enforcing that is every subsequent call denied, with the cause
	// nowhere near the denial. Observed live: an agent wrote raw tool names
	// where handles belong and lost all ten without a word.
	if len(notices) > 0 {
		notices = append(notices, l.describePlannableHandles())
	}

	// A phase that acts on a declared slot type must SAY which instance — or say
	// explicitly that it cannot yet. Silence is what is refused.
	//
	// Asking in the system prompt did not work: a live session recorded a prompt
	// containing "Name the RESOURCE, not just the permission" verbatim and the
	// agent declared zero slots across three phases, so every card named a
	// category and the user approved "some repository". This is the requirePlan
	// shape instead, and it is checked BEFORE the frozen plan is stored below —
	// a refused plan must not go on to govern anything.
	//
	// Mode-dependent for the same reason requirePlan is. `logging` runs every
	// step except blocking, and its whole purpose is a dataset gathered without
	// changing how sessions behave; refusing there would change them. Under
	// logging the agent is told and the plan stands.
	if missing := plangate.MissingSlotDeclarations(frozen, l.PlanGateSlotTypes); len(missing) > 0 {
		if l.PlanGateMode == plangate.ModeEnforcing {
			details := make([]string, 0, len(missing))
			for _, m := range missing {
				details = append(details, m.RefusalText())
			}
			return notices, fmt.Errorf("%s", strings.Join(details, " "))
		}
		for _, m := range missing {
			slog.Default().Info("plan_gate: phase names no instance for a declared slot type",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
				"phase", m.PhaseIndex, "resourceType", m.ResourceType)
			notices = append(notices, m.RefusalText())
		}
	}

	impact := map[permsurface.Handle]authz.StateImpact{}
	for _, d := range l.PlanGateSurface {
		impact[d.Handle] = d.StateImpact
	}

	l.planGateMu.Lock()
	l.planGateFrozen = frozen
	l.planGateHasPlan = len(frozen.Phases) > 0
	l.planGateMu.Unlock()

	// The tier-0 auto-approve budget is SESSION-CUMULATIVE: the union of handles
	// auto-approved without a human. Tracked here so each phase's ComputeTier
	// sees what the earlier phases in this same freeze already spent — without
	// it, every all-readonly phase priced independently and a plan declaring
	// many of them auto-cleared unbounded readonly breadth with nobody asked.
	// Seeded from prior tier-0 clearances in the log so re-planning cannot
	// reset the budget either.
	autoApproved := l.priorAutoApprovedHandles(ctx)

	// One record per phase, carrying its ceiling and price, so the fold and the
	// dataset both work from the log alone.
	total := 0
	for i, ph := range frozen.Phases {
		hs := make([]plangate.HandleImpact, 0, len(ph.Permissions))
		for _, h := range ph.Permissions {
			hs = append(hs, plangate.HandleImpact{Handle: h, StateImpact: impact[h]})
		}
		total += len(ph.Permissions)

		// Every dimension the digest covers, projected in ONE place. Spelled out
		// here per-field three times, it went stale three times — each new
		// authority dimension left a plan that could not rebuild as itself, so
		// the fold discarded its own log.
		authority := plangate.PhaseAuthorityRecord(frozen, i, l.PlanGateSlotStanding)

		// The budget bounds the UNION of auto-approved handles, and ComputeTier
		// prices it as `AlreadyAutoApproved + len(thisPhase)`. So "already" must
		// EXCLUDE handles this phase re-declares, or a phase re-counts a handle
		// already in the set and the sum over-counts the union — a re-plan (or
		// two phases) sharing a readonly handle would stop auto-approving reach
		// the session already holds. Count only the already-approved handles not
		// in this phase.
		alreadyExclThisPhase := 0
		for h := range autoApproved {
			if !phaseHasHandle(ph, h) {
				alreadyExclThisPhase++
			}
		}

		tier := plangate.ComputeTier(plangate.TierInput{
			Handles:               hs,
			MaxAutoApproveHandles: l.PlanGateMaxAutoApprove,
			// The union already spent OUTSIDE this phase (prior phases this
			// freeze, prior clearances earlier freezes); adding len(thisPhase)
			// yields |already ∪ thisPhase|.
			AlreadyAutoApproved:  alreadyExclThisPhase,
			MaxSingleCardHandles: l.PlanGateMaxCardHandles,
			// Every slot request counts as ungranted. The "already granted ⇒
			// stays auto" case in the slots spec is a real optimization, but it
			// needs a SpiceDB read of the session's current grants that is not
			// wired yet — and the two failure directions are not symmetric.
			// Guessing "already granted" would auto-approve a phase whose
			// approval mints a grant nobody was asked about; guessing "not
			// granted" costs one avoidable click. Take the click.
			UngrantedSlots: len(ph.Slots),
		})

		declared := authority
		declared.Event = plangateaudit.EventPlanApproved
		declared.PlanDigest = frozen.Digest()
		declared.Tier = strconv.Itoa(int(tier))
		declared.PhaseCount = len(frozen.Phases)
		declared.HandleCount = total
		declared.Mode = l.PlanGateMode
		declared.Provenance = "plan_gate"
		declared.At = time.Now().UTC()
		if err := (planGateRecorder{l: l}).Record(ctx, declared); err != nil {
			return notices, err
		}

		// Tier 0 clears itself: an all-readonly phase within the session budget
		// is auto-approved, which is what keeps a read-only recon phase from
		// costing a human anything. Every other tier waits for a person — the
		// gate raises that approval lazily, when the phase is first used, so a
		// phase the agent never enters never spends anyone's attention.
		if tier.AutoApproves() {
			// The same authority projection, which is also the GRANT: an
			// approval that names no subset has to fall back to reading the whole
			// declared phase as granted, a reading only records predating partial
			// approval should get. It already carries the phase's authority key,
			// which is what makes this clearance survive a re-plan that leaves
			// the phase alone.
			cleared := authority
			cleared.Event = plangateaudit.EventPhaseApproved
			cleared.PlanDigest = frozen.Digest()
			cleared.Tier = strconv.Itoa(int(tier))
			cleared.Mode = l.PlanGateMode
			cleared.Provenance = "plan_gate:tier0"
			cleared.At = time.Now().UTC()
			if err := (planGateRecorder{l: l}).Record(ctx, cleared); err != nil {
				return notices, err
			}
			// This phase's handles are now spent against the session budget, so
			// the NEXT phase in this freeze sees them.
			for _, h := range ph.Permissions {
				autoApproved[h] = struct{}{}
			}
		}
	}
	return notices, nil
}

// phaseHasHandle reports whether ph's ceiling holds h — used to keep the
// tier-0 budget a union rather than a sum (a handle a phase re-declares must
// not be counted both in the already-approved set and again as this phase's).
func phaseHasHandle(ph plangate.Phase, h permsurface.Handle) bool {
	for _, ph_h := range ph.Permissions {
		if ph_h == h {
			return true
		}
	}
	return false
}

// priorAutoApprovedHandles is the set of handles already tier-0 auto-approved in
// this session's plan-gate log, so a re-plan cannot reset the session-cumulative
// budget by declaring the same readonly breadth under a fresh digest. A tier-0
// clearance is a phase_approved record with provenance "plan_gate:tier0"; its
// ceiling names the handles it cleared. Best-effort: an unreadable log seeds an
// empty set (the within-freeze accumulation still bounds a single update_plan),
// which is the same fail-open the rest of the freeze path takes on a read error
// rather than wedging planning.
func (l *Loop) priorAutoApprovedHandles(context.Context) map[permsurface.Handle]struct{} {
	out := map[permsurface.Handle]struct{}{}
	// PlanGateRecords reads the log under the plan_gate read capability (the
	// same reader the fold uses) and returns nil, loudly, on a read failure —
	// so an unreadable log seeds empty, and the within-freeze accumulation still
	// bounds a single update_plan.
	for _, r := range l.PlanGateRecords() {
		if r.Event != plangateaudit.EventPhaseApproved || r.Provenance != "plan_gate:tier0" {
			continue
		}
		for _, raw := range r.Ceiling {
			if h, perr := permsurface.ParseHandle(raw); perr == nil {
				out[h] = struct{}{}
			}
		}
	}
	return out
}

// ActiveFrozenPlan returns the frozen plan currently in force, for select_phase.
func (l *Loop) ActiveFrozenPlan(context.Context) (plangate.Plan, bool) {
	l.planGateMu.Lock()
	defer l.planGateMu.Unlock()
	return l.planGateFrozen, l.planGateHasPlan
}

// RecordPhaseSelection appends the runtime's attestation that the agent moved.
func (l *Loop) RecordPhaseSelection(ctx context.Context, index int) error {
	l.planGateMu.Lock()
	frozen := l.planGateFrozen
	l.planGateMu.Unlock()

	idx := int32(index)
	return (planGateRecorder{l: l}).Record(ctx, plangateaudit.Content{
		Event:      plangateaudit.EventPhaseSelected,
		PlanDigest: frozen.Digest(),
		PhaseIndex: &idx,
		Mode:       l.PlanGateMode,
		Provenance: "select_phase",
		At:         time.Now().UTC(),
	})
}

// PlanGateRecords reads this session's plan-gate log for the fold.
func (l *Loop) PlanGateRecords() []plangateaudit.Content {
	if l.Mem == nil {
		return nil
	}
	scope := memory.Scope{Kind: "session", ID: l.SessionKey.Namespace + "/" + l.SessionKey.Name}
	recs, err := plangateaudit.List(memory.WithSystemApproval(context.Background(), "plan_gate"), l.Mem, scope)
	if err != nil {
		// The fold treats an empty log as "no plan yet", which fails closed to
		// phase 0's ceiling rather than open. Loud, because a gate that cannot
		// read its own state is operating blind.
		slog.Default().Info("plan_gate: could not read the audit log; folding against an empty history",
			"session", scope.ID, "err", err.Error())
		return nil
	}
	return recs
}

// recordContentGuardEvent writes one finding to the contentguard_audit kind.
// Best-effort; logged, never returned (the gate decision already happened).
func (l *Loop) recordContentGuardEvent(ctx context.Context, ev contentguard.Event) {
	if l.Mem == nil {
		return
	}
	scope := memory.Scope{Kind: "session", ID: l.SessionKey.Namespace + "/" + l.SessionKey.Name}
	detailsJSON := ""
	if len(ev.Details) > 0 {
		if b, err := json.Marshal(ev.Details); err == nil {
			detailsJSON = string(b)
		}
	}
	if err := contentguardaudit.Record(ctx, l.Mem, scope, contentguardaudit.Content{
		Inspector: ev.Inspector, Action: ev.Action, Tool: ev.Tool,
		Point: ev.Point, Reason: ev.Reason, Details: detailsJSON,
	}); err != nil {
		slog.Default().Info("contentguardaudit.Record",
			"session", scope.ID, "inspector", ev.Inspector, "tool", ev.Tool, "err", err.Error())
	}
}

// ActiveFrozenPlanNow is the no-context accessor the plan-gate hook uses to
// read the CURRENT frozen plan on every call.
func (l *Loop) ActiveFrozenPlanNow() (plangate.Plan, bool) {
	l.planGateMu.Lock()
	defer l.planGateMu.Unlock()
	return l.planGateFrozen, l.planGateHasPlan
}

// AuthoredPhasesFrom projects the plans state kind's phase list into plangate's
// neutral input shape.
//
// plangate deliberately does not import the plans package — it is a pure value
// package, so the caller projects, exactly as permsurface.Candidate works. This
// is that projection, and it lives HERE rather than in cmd/runner because the
// e2e in-process factory needs the identical one.
//
// It used to exist twice: cmd/runner's copy and an e2e copy whose comment
// promised it "mirrors cmd/runner's so the factory exercises the same
// projection production does". A mirror is a promise nothing enforces, and this
// repo has already been bitten by exactly that shape — the interaction-resume
// switch drifted between the runner and the harness and the drift was invisible
// until an approved plan hung. A phase field added to one copy and not the other
// is silently dropped in whichever copy the test does not use, which is the
// worst direction: the suite stays green while production loses the field.
func AuthoredPhasesFrom(in []plans.Phase) []plangate.AuthoredPhase {
	out := make([]plangate.AuthoredPhase, 0, len(in))
	for _, p := range in {
		ap := plangate.AuthoredPhase{ID: p.ID, Label: p.Label, Why: p.Why}
		for _, r := range p.Requires {
			ap.Requires = append(ap.Requires, plangate.AuthoredRequires{Phase: r.Phase, Why: r.Why})
		}
		if p.Max != nil {
			ap.Max = &plangate.AuthoredMax{Count: p.Max.Count, Why: p.Max.Why}
		}
		if p.Budget != nil {
			ap.Budget = &plangate.AuthoredBudget{Calls: p.Budget.Calls, Why: p.Budget.Why}
		}
		for _, perm := range p.Permissions {
			ap.Permissions = append(ap.Permissions, plangate.AuthoredPermission{Handle: perm.Handle, Why: perm.Why})
		}
		// The instance axis. update_plan has accepted slots since the schema
		// landed; until now every projection dropped them, so a phase could
		// request a slot and the frozen plan would never know.
		for _, s := range p.Slots {
			ap.Slots = append(ap.Slots, plangate.AuthoredSlot{Type: s.Type, ID: s.ID, Why: s.Why})
		}
		out = append(out, ap)
	}
	return out
}

// RecordPhaseCompletion appends the agent's declaration that a phase finished.
//
// The agent has always declared completion; this is the runtime attesting THAT
// it did, and when. In the append-only log the declaration is ordered and
// monotonic — unlike a field in the plan document, which update_plan resubmits
// wholesale on every call and can therefore be flipped back.
func (l *Loop) RecordPhaseCompletion(ctx context.Context, index int, outcome string) error {
	l.planGateMu.Lock()
	frozen := l.planGateFrozen
	l.planGateMu.Unlock()

	idx := int32(index)
	return (planGateRecorder{l: l}).Record(ctx, plangateaudit.Content{
		Event:      plangateaudit.EventPhaseCompleted,
		PlanDigest: frozen.Digest(),
		PhaseIndex: &idx,
		// UNTRUSTED and never read by any decision — audit and display only.
		PhaseOutcome: outcome,
		Mode:         l.PlanGateMode,
		Provenance:   "complete_phase",
		At:           time.Now().UTC(),
	})
}

// PhaseCompletionMap is what select_phase consults: which phases are finished.
//
// Folded rather than held, like every other plan-gate fact. A phase counts only
// when the runtime saw it ENTERED and the agent declared it done — see
// plangate.State.CompletedPhases for why both halves are required.
func (l *Loop) PhaseCompletionMap(context.Context) map[int]bool {
	st, ok := l.foldPlanGate()
	if !ok {
		// Nothing establishable. Absence is treated as incomplete by the
		// consumer, which is the fail-closed direction for a gate that only
		// withholds.
		return nil
	}
	return st.CompletedPhases()
}

// PhaseEntryCounts reports HOW MANY TIMES the runtime has seen each phase
// entered, so select_phase can enforce MaxSpec.Count.
//
// Distinct from PhaseEnteredMap, which answers the boolean complete_phase
// needs. The ledger behind both is State.Spent; until this existed, every
// reader collapsed it to `> 0` and the entry limit the approver was shown on
// the card was compared to nothing.
func (l *Loop) PhaseEntryCounts(context.Context) map[int]int {
	st, ok := l.foldPlanGate()
	if !ok {
		return nil
	}
	out := make(map[int]int, len(st.Spent))
	for idx, n := range st.Spent {
		out[idx] = n
	}
	return out
}

// PhaseEnteredMap reports which phases the runtime has seen entered, so
// complete_phase can refuse a phase that was never run.
func (l *Loop) PhaseEnteredMap(context.Context) map[int]bool {
	st, ok := l.foldPlanGate()
	if !ok {
		return nil
	}
	out := map[int]bool{}
	for idx, n := range st.Spent {
		if n > 0 {
			out[idx] = true
		}
	}
	return out
}

// foldPlanGate replays this session's plan-gate log against the frozen plan.
func (l *Loop) foldPlanGate() (plangate.State, bool) {
	l.planGateMu.Lock()
	frozen, has := l.planGateFrozen, l.planGateHasPlan
	l.planGateMu.Unlock()
	if !has || len(frozen.Phases) == 0 {
		return plangate.State{}, false
	}
	st, err := plangate.Fold(frozen, l.PlanGateRecords())
	if err != nil {
		slog.Default().Info("plan_gate: could not fold the log for phase completion",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		return plangate.State{}, false
	}
	return st, true
}

// describePlannableHandles renders what a phase may actually declare.
//
// It is appended to a drop notice rather than published continuously: told
// only "that handle is invalid", an agent can do no better than guess again,
// and the guesses look plausible enough to keep failing the same way.
//
// The empty case is called out separately because it is a different situation
// wearing the same symptom. When nothing on the agent is gated — every tool
// declaring stateless or passthrough, which permsurface excludes by design —
// EVERY handle is dropped no matter what the agent writes, and a list of valid
// alternatives would be an empty list implying the agent merely guessed wrong.
func (l *Loop) describePlannableHandles() string {
	if len(l.PlanGateSurface) == 0 {
		return "this session has no gated tools, so no handle can be declared: " +
			"every tool available here is stateless or passthrough, which carries no " +
			"authorization. Declare phases without a permissions list."
	}
	return "declarable handles on this session: " +
		strings.Join(permsurface.Handles(l.PlanGateSurface), ", ")
}

// planGateHandleResolver resolves the handle a CALL is gated on.
//
// Prefers the tool's own per-call answer, because a sandbox tool covers a whole
// CLI: `gh pr view` and `gh pr create` arrive at one tool with different
// severity, and its base permission is the toolkit default. Resolving from the
// tool alone gave both no handle and the gate skipped them entirely. Falls back
// to the base handle so MCP and meta tools are unaffected.
func (l *Loop) planGateHandleResolver() hooks.HandleResolver {
	return func(toolName string, args map[string]any) (permsurface.Handle, bool) {
		t, ok := l.lookupTool(toolName)
		if !ok {
			return permsurface.Handle{}, false
		}
		if pc, ok := t.(tool.PerCallPermission); ok && args != nil {
			if perm, ok := pc.PermissionForCall(args); ok {
				if h, ok := tool.HandleForPermission(perm, toolName); ok {
					return h, true
				}
				return permsurface.Handle{}, false
			}
		}
		// PlanGateHandle, not BaseHandle: a tool that opts into plan-gate
		// governance (delegate) yields a `tool:<name>` handle here even with a
		// Stateless dispatch permission, so the gate sees the spawn under
		// `enforcing` mode. Every other tool derives its handle from its
		// Permission exactly as before.
		return tool.PlanGateHandle(t)
	}
}
