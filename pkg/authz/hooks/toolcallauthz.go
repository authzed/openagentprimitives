package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// ToolCallChecker is the narrow CheckToolCall surface (satisfied by
// engine.Engine and by toolcheck.Checker).
type ToolCallChecker interface {
	// CheckToolCall decides whether the tool call described by p and in may
	// dispatch. Returns a Result and no error: an RPC failure, an unwired
	// client and a genuine deny are all OutcomeDenied, so the hook always
	// fails CLOSED.
	CheckToolCall(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result
}

// ApprovalAskBuilder builds the ApprovalAsk for a tool-call gate. The runner
// supplies this (it owns the channel-envelope payload shape + summarizer LLM
// + approver-subject resolution); the hook only decides THAT an ask is needed.
//
// It receives the whole pipeline.Input so the runner can attribute the ask to
// the PER-CALL principal: in.Tool.Name / in.Tool.UseID identify the call, and
// in.Requester / in.ProxyExec drive the approver-facing Requester (the widget
// VIEWER on a proxy-exec, the session's LastInbound requester otherwise —
// D-D1). justification is the agent's stated reason, possibly annotated by the
// hook (pin-drift), which is why it is passed separately rather than read from
// in.Tool.Justification.
type ApprovalAskBuilder func(ctx context.Context, in pipeline.Input, args map[string]any, perm authz.Permission, justification string) (*pipeline.ApprovalAsk, error)

// WaiverAskBuilder builds the ApprovalAsk for the ONE precondition a human may
// waive: a REFUSED verdict. It is a separate builder from ApprovalAskBuilder,
// not a widened one, because the two carry different trust: a tool_call ask
// renders the agent's justification (agent-authored, possibly prompt-injected),
// where a waiver card renders the gate's RefusalMessage (author-authored, the
// CRD text) — the two must never be confused, so the denial travels as its own
// argument rather than smuggled through justification. The ask it returns is of
// Kind categories.PreconditionWaiver, and approving it goes through the waiver
// handler (which is what waives the gate), never the tool_call grant path.
type WaiverAskBuilder func(ctx context.Context, in pipeline.Input, args map[string]any, perm authz.Permission, denial *authz.PreconditionDenial) (*pipeline.ApprovalAsk, error)

// ToolCallAuthzDeps is the dependency struct for ToolCallAuthz. Constructed
// by the runner's buildPipeline and passed at hook construction.
type ToolCallAuthzDeps struct {
	Mode              string // "enforcing" | "permissive" | "disabled"
	Checker           ToolCallChecker
	ResolvePermission func(toolName string, args map[string]any) (authz.Permission, error)
	// BuildInputs builds the authz.Inputs for the SpiceDB check. MUST set
	// SessionScope = scope.Scope{} (identity — scope narrowing is the Scope
	// hook's job). It receives the whole pipeline.Input so the check subject +
	// subject-list derive from the PER-CALL principal (in.Requester / in.Subjects),
	// never the loop-mutated l.authSubject (D-D1: a proxy-exec checks the viewer).
	BuildInputs func(in pipeline.Input, args map[string]any, perm authz.Permission) authz.Inputs
	// BuildApprovalAsk constructs the ApprovalAsk when approval is needed.
	// May be nil in tests that don't need the approval path.
	BuildApprovalAsk ApprovalAskBuilder
	// BuildWaiverAsk constructs the waiver ApprovalAsk when a slot precondition
	// REFUSED the call — the ONE precondition verdict a human may consent past.
	// May be nil (kubectl-driven, or a test that does not exercise the waiver
	// path); a Refused precondition with no waiver builder wired denies
	// fail-closed with the gate's own message, exactly as the unappealable plan-2
	// gate did. Undetermined and Unevaluatable NEVER reach this — needsApproval
	// returns false for both.
	BuildWaiverAsk WaiverAskBuilder

	// NormalizeArgs renders the tool's own view of a call's arguments, once,
	// for every consumer in this hook.
	//
	// A check reads NAMED arguments; a sandbox tool arrives as
	// {operation_id, _reason, args: [argv]} and unwrapEnvelope leaves that
	// untouched because `args` is an ARRAY. The conversion used to live inside
	// BuildInputs, so the CHECK saw parsed args and the APPROVAL ASK saw the raw
	// envelope — two views of one call. The ask then resolved `{remote}` against
	// a map with no remote in it and produced an empty resource id, which
	// surfaced later as an unusable approval in another process.
	//
	// nil is identity, which is correct for MCP tools (already named) and for
	// tests that do not exercise argv parsing.
	NormalizeArgs func(in pipeline.Input, argsMap map[string]any) map[string]any
	// RecordDecision records the authz decision to the authzdecision memory
	// kind. nil-safe (no-op when nil).
	RecordDecision func(ctx context.Context, toolName string, perm authz.Permission, res authz.Result, in authz.Inputs)
	// ForceApproval, when non-nil, escalates a tool call to per-call user
	// approval regardless of its resolved permission — the dependency-pinning
	// "approve" drift mode. The returned reason feeds the approval ask's
	// justification context so the approver sees why the escalation happened.
	//
	// NOTE: when Deps.Mode is "disabled", Eval returns before ForceApproval is
	// consulted — pin-drift per-call escalation does not apply to authz-disabled
	// sessions. Description-level warnings (added by the runner at session start)
	// are still visible to the LLM, but no approval gate fires.
	ForceApproval func(toolName string) (reason string, ok bool)

	// ExplainPrecondition recomputes, for a call the checker DENIED, the slot
	// precondition that explains why the slot it names is empty — returning nil
	// when none does.
	//
	// It is a dep rather than something this hook computes because the
	// recomputation reads the session's recorded facts, and the hook holds no
	// memory store; the runner supplies the closure over its own, exactly as it
	// does for RecordDecision. A nil FUNC means no gate is DECLARED at all (no
	// class slots, or a test that does not exercise the gate) and leaves every
	// denial as the checker phrased it. A session whose fact store is missing
	// must NOT be wired as nil: that is a declared gate this hook cannot
	// evaluate, and the func returns an Unevaluatable denial for it.
	//
	// It must have NO side effects. A verdict is a pure function of durable
	// facts and a declared expression, and is recomputed rather than cached
	// precisely so that dispatch and the bind-time filter cannot come to
	// disagree about why a slot is empty.
	ExplainPrecondition func(ctx context.Context, perm authz.Permission, args map[string]any) *authz.PreconditionDenial

	Logger *slog.Logger
}

// ToolCallAuthz is the PreToolCall hook that performs the positive SpiceDB
// permission check. It does NOT evaluate scope narrowing or Layer-3 disallow
// (that is the Scope hook's job). On denied/external → ApprovalAsk. Gated by
// toolCalls.mode: disabled ⇒ no-op Allow.
type ToolCallAuthz struct{ d ToolCallAuthzDeps }

// NewToolCallAuthz creates a ToolCallAuthz hook.
func NewToolCallAuthz(d ToolCallAuthzDeps) *ToolCallAuthz {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &ToolCallAuthz{d: d}
}

func (h *ToolCallAuthz) Name() string             { return "tool_call_authz" }
func (h *ToolCallAuthz) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreToolCall} }

func (h *ToolCallAuthz) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil {
		return pipeline.Decision{}
	}
	if h.d.Mode == "disabled" {
		return pipeline.Decision{} // disabled-mode: positive check is a no-op (Scope still runs)
	}

	// Malformed (LLM-supplied, prompt-injectable) args must NOT resolve the
	// permission against a nil map: variant resolution (ResolveVariant) keys
	// off args, so garbage JSON could fall through to a more-permissive base
	// permission and pass the check. Treat unparseable args as fail-closed.
	// This hook runs at OrderToolCallAuthz (20) — BEFORE the Scope hook (30) —
	// so it is NOT backstopped by Scope's own malformed-args deny; it must
	// guard itself. A no-arg call is "{}" (parses clean); only genuinely
	// broken JSON denies.
	var argsMap map[string]any
	if len(in.Tool.Args) > 0 {
		if err := json.Unmarshal(in.Tool.Args, &argsMap); err != nil {
			h.d.Logger.Info("authz: tool args unparseable; refusing to dispatch (fail-closed)",
				"tool", in.Tool.Name, "err", err.Error())
			return pipeline.Decision{
				Verdict: pipeline.Deny,
				Reason:  fmt.Sprintf("authz: tool args unparseable for %s — refusing to dispatch (fail-closed)", in.Tool.Name),
			}
		}
	}
	argsMap = unwrapEnvelope(argsMap)

	// Permission resolution reads the RAW argv, and must run BEFORE
	// normalization.
	//
	// PermissionForCall identifies the subcommand via argvFrom(args), which
	// reads the `args` key. Normalizing first removes it, no subcommand ever
	// matches, and every sandbox call falls back to the toolkit default —
	// `passthrough`. That is a total authorization bypass, and it shipped
	// briefly while fixing a different bug about these same two views.
	perm, err := h.d.ResolvePermission(in.Tool.Name, argsMap)
	if err != nil {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("authz: variant resolution failed for %s — refusing to dispatch (fail-closed): %v", in.Tool.Name, err),
		}
	}

	// ONE normalization, AFTER the permission is known and before anything reads
	// the arguments for their VALUES. The check, the recorded decision and the
	// approval ask must all agree about what this call acts ON.
	//
	// Two views, and each consumer needs a specific one: identifying WHICH
	// subcommand ran is an argv question; identifying WHICH RESOURCE it touches
	// is a named-argument question. Collapsing them in either direction breaks
	// one of the two.
	if h.d.NormalizeArgs != nil {
		if named := h.d.NormalizeArgs(in, argsMap); named != nil {
			argsMap = named
		}
	}

	var inputs authz.Inputs
	if h.d.BuildInputs != nil {
		inputs = h.d.BuildInputs(in, argsMap, perm)
	}
	// This hook is the POSITIVE check only. Hard-deny (Layer-2 session_scope
	// Disallow) is the Scope hook's job; CheckToolCall performs no disallow
	// sub-check.

	res := h.d.Checker.CheckToolCall(ctx, perm, inputs)
	res = h.explainPrecondition(ctx, res, perm, argsMap)
	// AFTER the precondition recomputation, deliberately: the durable authz
	// decision must record the same bytes the model is handed, or the audit
	// says the call was refused for a reason the agent was never told.
	if h.d.RecordDecision != nil {
		h.d.RecordDecision(ctx, in.Tool.Name, perm, res, inputs)
	}

	needsApprovalResult := needsApproval(res, perm)
	// forceReason is non-empty when the dependency-pinning "approve" drift mode
	// escalates a tool call to per-call user approval. Consulted unconditionally
	// so that a tool already gated (external stateImpact, denied check) still
	// carries the [PIN DRIFT: …] marker in the justification when its dependency
	// has also drifted.
	var forceReason string
	// A precondition-explained denial is NOT force-escalatable, and this is the
	// one place the two gates could collide. Pin drift asks a human to approve
	// running a tool whose DEPENDENCY changed; a precondition refuses the call
	// over the RESOURCE it names. Letting drift raise a card here would not
	// merely ask a pointless question — approving a tool_call card writes the
	// slot binding through the tool_approval path, which carries no Requires and
	// so does not re-check the gate, so a human answering a question about a
	// dependency digest would silently grant the fork review nobody asked them
	// about. That is why a Refused verdict routes instead to the WAIVER card,
	// the one path that both explains the risk and — via
	// BindApproved(PreconditionsWaived) — is allowed to bind past it.
	if h.d.ForceApproval != nil {
		if reason, ok := h.d.ForceApproval(in.Tool.Name); ok {
			if res.Precondition == nil {
				needsApprovalResult, forceReason = true, reason
			} else {
				// Never silently: an operator who chose "approve" drift mode is
				// owed the reason their escalation did not fire.
				h.d.Logger.Info("authz: pin-drift escalation suppressed — a slot precondition already refuses this call, and its approval would waive the precondition",
					"tool", in.Tool.Name, "driftReason", reason,
					"verdict", preconditionVerdictLabel(res.Precondition))
			}
		}
	}
	if needsApprovalResult {
		// Permissive: denied readonly/readwrite proceeds (external still asks).
		// Force-approved calls bypass permissive: the operator explicitly chose
		// "approve" mode, so we must always pause for the human.
		if forceReason == "" && h.d.Mode == "permissive" && perm.StateImpact != authz.External && res.EnforceOverride != authz.EnforceAlways {
			h.d.Logger.Info("permissive: would deny in enforcing mode",
				"tool", in.Tool.Name, "reason", res.Message)
			return pipeline.Decision{}
		}
		// A REFUSED precondition routes to the WAIVER builder, never the ordinary
		// tool_call ask. needsApproval returns true for a non-nil Precondition ONLY
		// when its verdict is Refused (Undetermined and Unevaluatable both returned
		// false above), so reaching here with res.Precondition set IS the waiver
		// case. The card must carry the gate's RefusalMessage and be of Kind
		// precondition_waiver, because approving it goes through the waiver handler
		// — the ONE path allowed to waive a fact-gated refusal. Routing it to the
		// tool_call builder would write the slot binding through the grant path and
		// waive the gate WITHOUT the card that explains the consent. forceReason is
		// always empty here: pin-drift escalation is suppressed on any non-nil
		// precondition (see the ForceApproval block above).
		if res.Precondition != nil {
			if h.d.BuildWaiverAsk != nil {
				ask, aerr := h.d.BuildWaiverAsk(ctx, in, argsMap, perm, res.Precondition)
				if aerr != nil {
					return pipeline.Decision{Verdict: pipeline.Deny, Reason: aerr.Error()}
				}
				return pipeline.Decision{Approval: ask}
			}
			// No waiver builder wired (kubectl-driven, or a test not exercising the
			// waiver path): deny fail-closed with the gate's own message, exactly as
			// the unappealable plan-2 gate did.
			return pipeline.Decision{Verdict: pipeline.Deny, Reason: res.Message}
		}
		if h.d.BuildApprovalAsk != nil {
			// Append the drift reason to the justification so the approver sees
			// that this call is escalated due to pin drift, not a normal authz
			// denial. The agent's own justification (if any) is preserved first.
			justification := in.Tool.Justification
			if forceReason != "" {
				if justification != "" {
					justification = justification + " [PIN DRIFT: " + forceReason + "]"
				} else {
					justification = "[PIN DRIFT: " + forceReason + "]"
				}
			}
			ask, aerr := h.d.BuildApprovalAsk(ctx, in, argsMap, perm, justification)
			if aerr != nil {
				return pipeline.Decision{Verdict: pipeline.Deny, Reason: aerr.Error()}
			}
			return pipeline.Decision{Approval: ask}
		}
		// No BuildApprovalAsk wired (test path or kubectl-driven without approval)
		// but approval is needed — deny fail-closed. When the denial is driven
		// purely by a forced-approval (pin drift) and the checker returned no
		// message, use forceReason so the denial isn't opaque.
		denyReason := res.Message
		if denyReason == "" && forceReason != "" {
			denyReason = "[PIN DRIFT: " + forceReason + "]"
		}
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: denyReason}
	}

	if res.IsError() {
		// Denied, no approval possible. Permissive readonly/readwrite proceeds.
		if h.d.Mode == "permissive" && perm.StateImpact != authz.External && res.EnforceOverride != authz.EnforceAlways {
			h.d.Logger.Info("permissive: would deny in enforcing mode (no approval flow)",
				"tool", in.Tool.Name, "reason", res.Message)
			return pipeline.Decision{}
		}
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: res.Message}
	}
	return pipeline.Decision{}
}

// explainPrecondition turns a bare denial into an explained one when a SLOT
// PRECONDITION is what emptied the slot the call names, and returns res
// unchanged otherwise.
//
// # EnforceAlways is not optional, and it is the whole reason this exists
//
// Under toolCalls.mode: permissive, Eval returns Decision{} for a denied
// readonly/readwrite BEFORE any of this matters — the deny is logged and the
// call PROCEEDS. Without the override stamped here, a precondition refusal
// would be one log line and a checked-out fork. A precondition must not be
// disableable by a setting about something else, for the reason TrifectaConfig
// gives about its own mode: an operator who turned off permission CHECKING has
// not thereby decided that untrusted code may be run.
//
// The stamp goes on an UNEVALUATABLE result too — a gate whose facts could not
// be read, whose id would not resolve, whose expression failed at eval. A gate
// that cannot be evaluated has to be treated as a gate that said no, which is
// the position pipeline.Decision.Definition already takes; leaving it unstamped
// would mean a transient store failure on a gated type restores the permissive
// bypass, which is the exact hole this hook exists to close. Only the message
// and the log wording differ: no verdict is claimed, because none was computed.
//
// # Only a denial, and never an unresolved one
//
// An ALLOWED call has nothing to explain — and, since a satisfied precondition
// is what let its instance occupy the slot in the first place, nothing to say.
// An UnresolvedResource denial is skipped because that call never named an
// instance at all: there is no subject to look facts up for, it already has its
// own back-to-the-agent route, and re-explaining it as a precondition verdict
// would replace an accurate message ("this call did not identify which repo it
// acts on") with a misleading one.
func (h *ToolCallAuthz) explainPrecondition(ctx context.Context, res authz.Result, perm authz.Permission, argsMap map[string]any) authz.Result {
	if h.d.ExplainPrecondition == nil || !res.IsError() || res.UnresolvedResource || perm.Check == nil {
		return res
	}
	expl := h.d.ExplainPrecondition(ctx, perm, argsMap)
	if expl == nil {
		return res
	}
	if expl.Unevaluatable {
		// Deliberately a different sentence, and deliberately without a verdict
		// key: none was computed. Saying "explains this denial" here would claim
		// a rule decided something, and the reader would go looking for the fact
		// it decided on.
		h.d.Logger.Info("authz: a slot precondition governs this call and could NOT be evaluated; the call is refused regardless of toolCalls.mode",
			"tool", perm.ToolName, "resourceType", perm.Check.ResourceType,
			"permission", perm.Check.Permission, "reason", expl.Message)
	} else {
		h.d.Logger.Info("authz: a slot precondition explains this denial; the call is refused regardless of toolCalls.mode",
			"tool", perm.ToolName, "resourceType", perm.Check.ResourceType,
			"permission", perm.Check.Permission, "verdict", expl.Verdict.String())
	}
	res.Precondition = expl
	// The author's own message REPLACES the SpiceDB one. "alice does not have
	// read on github_pr:demo-org/demo-repo#6" is true and useless: it points the
	// agent at a permission grant nobody is going to write, when the actual
	// answer is to establish a fact or to stop.
	res.Message = expl.Message
	res.EnforceOverride = authz.EnforceAlways
	return res
}

// preconditionVerdictLabel names a precondition denial for a log line without
// claiming a verdict it does not have.
//
// precondition.Undetermined is the ZERO Verdict, so Verdict.String() on an
// unevaluatable denial reports "undetermined" — which reads as "some fact has
// not been observed yet" and sends whoever is reading the log hunting for a
// missing observation that was never the problem.
func preconditionVerdictLabel(d *authz.PreconditionDenial) string {
	if d.Unevaluatable {
		return "unevaluatable"
	}
	return d.Verdict.String()
}

// needsApproval decides whether a checked call pauses for a human.
func needsApproval(res authz.Result, perm authz.Permission) bool {
	// An ALLOWED check needs no human, whatever the state impact.
	//
	// Safe for External only because its check consults the slot-grant leg
	// ALONE (toolcheck.checkExternalSlotGrant): an allowed external means a human
	// cleared a card naming this instance and permission for this session, not
	// that the requester happens to own the resource. Reading the composed
	// permission here instead would let ambient ownership through — see that
	// function's comment for the bypass that caught it.
	if res.Outcome == authz.OutcomeAllowed {
		return false
	}
	// Checked BEFORE the External arm, which otherwise pauses unconditionally.
	//
	// This is the same route UnresolvedResource takes, for the same reason its
	// doc comment gives: a denial a human cannot act on goes back to the agent,
	// because "the only party who can fix it is the agent". A human cannot
	// supply a fact nobody has observed either — an Undetermined verdict is
	// answerable only by the call that establishes the missing observation, and
	// a card asking someone to approve it would name a rule that has not
	// decided anything yet.
	//
	// A REFUSED verdict is the ONE precondition a human may waive: it is turned
	// into a waiver card that explains what consenting to it means (spec §7).
	// The other two verdicts stay unappealable here, and neither is an oversight.
	//
	// The split is the security core of this hook, so its ORDER is load-bearing.
	// Unevaluatable is read FIRST because it is a SEPARATE bool, not a verdict:
	// an unevaluatable denial's Verdict is the zero value (precondition.Undetermined),
	// so a switch on Verdict alone would misread it as Undetermined — and, far
	// worse, if the Refused arm ran before this guard, an unevaluatable result
	// that happened to carry Refused would raise a waiver card for a verdict that
	// was never computed. Approving THAT card waives a gate whose facts could not
	// be read: the exact bypass this split exists to prevent. Nobody can approve
	// their way past a fact store that will not read, so it goes back to the
	// agent as a tool result.
	if res.Precondition != nil {
		if res.Precondition.Unevaluatable {
			return false
		}
		// Undetermined is unappealable for the same reason UnresolvedResource is:
		// a fact nobody has observed is answerable only by the agent making the
		// call that establishes it, not by a human clicking approve on a rule that
		// has not decided anything. Only Refused raises the waiver.
		return res.Precondition.Verdict == precondition.Refused
	}
	if perm.StateImpact == authz.External {
		return true
	}
	// Checked BEFORE the state-impact switch, and after the External arm on
	// purpose: External pauses on its own, before any resource is resolved, so
	// it never carries this flag. Everything below is a denial an approver could
	// act on; this one is not — the call never said which resource it meant, so
	// the card would carry an empty object id and granting it would not let the
	// retry through. It belongs back with the agent as a tool result.
	if res.UnresolvedResource {
		return false
	}
	switch perm.StateImpact {
	case authz.Readonly, authz.Readwrite:
		return perm.Check != nil
	}
	return false
}

// unwrapEnvelope strips the {operation_id,_reason,args} wrapper to the inner
// args map. Mirrors runner.unwrapToolArgs (argsenvelope.go): fires only when
// the canonical envelope shape is intact (operation_id present + args is a map).
// Meta tools have flat input schemas and are passed through unchanged.
func unwrapEnvelope(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	if _, ok := m["operation_id"]; !ok {
		return m
	}
	inner, ok := m["args"].(map[string]any)
	if !ok {
		return m
	}
	return inner
}

var _ pipeline.Hook = (*ToolCallAuthz)(nil)
