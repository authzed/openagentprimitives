// pkg/agent/runner/toolguard_meta.go
//
// Toolguard for the meta tools the containment pipeline skips.
//
// toolguard's Guard/GuardRecord hooks run inside executeToolContained, which the
// dispatcher skips for every meta tool with a trivial (Stateless / Passthrough)
// permission — all of them except apply_workspace. ToolGuardMatch.Kind
// nonetheless advertises "meta", so `match: {kind: meta, tool:
// read_channel_history}` with a rateLimit or dataLimit passes admission and then
// enforces nothing — no rate cap, no breaker, no ingress byte budget, and no
// rejection, status note or log to say so — on exactly the meta tools that pull
// third-party content into the model's context.
//
// This file closes that gap by running the same two hooks around the ungated
// execute. It deliberately does NOT run the full pipeline.Executor: the other
// PreToolCall hooks are no-ops for these tools by construction (no SpiceDB-keyed
// resource, no MCP spec), and the executor's approval plumbing is not wanted on
// a path whose whole point is that it carries no authorization decision.
package runner

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// metaToolGuardApplies reports whether toolguard should wrap this ungated meta
// tool call.
//
// The question is "did an admin configure a guard covering this tool", and an
// admin has TWO independent surfaces to say so with: the tier rules
// (Defaults.ToolGuard / AgentClass.spec.toolGuard) and the Limits ceiling
// (ToolGuardCeiling), folded separately by pkg/platform/settings. So the
// predicate derives from the RESOLVED rule, not from rule-list membership:
// asking only "did a rule match?" leaves a ceiling-only admin's byte and rate
// bounds silently inert here while they bind on every gated sandbox/mcp tool.
//
// ForUngatedTools is what keeps toolguard.Builtin out: its no-rule-matched
// fallback carries no breaker, so the ceiling can bind without Builtin's
// consecutive-failure breaker — calibrated for external dependencies — wedging
// the session by denying agent_work_complete or respond_to_user after five
// errors. Disabled() is the same emptiness test Guard.Eval applies, so predicate
// and hook cannot drift.
func (l *Loop) metaToolGuardApplies(t tool.Tool, name string) bool {
	pol := l.metaToolGuardPolicy()
	if pol == nil {
		return false
	}
	kind, origin := l.toolGuardLookup()(name)
	if kind == "" {
		// The dispatcher resolved this tool but toolGuardLookup did not, so the
		// two disagree about what this name is. Guard nothing rather than guess,
		// and say so — a silent skip here is a silently unenforced policy.
		slog.Default().Info("toolguard: meta tool not resolvable for policy lookup; not guarded",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "tool", name, "kind", string(t.Kind()))
		return false
	}
	return !pol.RuleFor(kind, name, origin).Disabled()
}

// metaToolGuardPolicy is the policy view every toolguard consult on this path
// must go through — the predicate above AND the hooks below, so what decides to
// guard and what enforces the guard resolve the same rule. nil when the session
// has no policy at all.
func (l *Loop) metaToolGuardPolicy() *toolguard.ResolvedPolicy {
	return l.ToolGuardPolicy.ForUngatedTools()
}

// executeMetaToolGuarded runs exec under toolguard's PreToolCall guard and
// PostToolCall recorder, returning the same containedOutcome shape
// executeToolContained returns so the dispatcher's deny/halt bookkeeping is
// identical on both branches. When no authored rule covers this tool it is a
// straight pass-through — no added cost or behavior on the unguarded meta path.
func (l *Loop) executeMetaToolGuarded(
	ctx context.Context,
	sess *tool.SessionContext,
	t tool.Tool,
	name string,
	args json.RawMessage,
	useID string,
	exec func(context.Context) tool.Result,
) (tool.Result, containedOutcome) {
	if !l.metaToolGuardApplies(t, name) {
		return exec(ctx), containedOutcome{Phase: containRanOK}
	}

	// Building the executor is what constructs l.toolGuardReg (the tool_guard
	// HookFactory owns it). Forcing it here — it is sync.Once-guarded — both
	// avoids a nil registry on a turn that dispatches only meta tools, and keeps
	// gated and ungated calls sharing ONE set of breaker/rate counters, so a
	// rule spanning both kinds cannot be evaded by which branch a call took.
	_ = l.executor()

	// A half-open breaker probe is claimed at Pre and handed back at Post; a Pre
	// deny or halt returns without a Post, and half-open has no cool-off timer to
	// undo a stranded claim. One ledger per call, released on every exit.
	ctx, releaseProbes := toolguard.WithProbeLedger(ctx)
	defer releaseProbes()

	sessRef := pipeline.SessionRef{Namespace: sess.Namespace, Name: sess.Name, Class: l.AgentName}

	// Both hooks re-resolve the rule themselves, so both must be handed the same
	// ungated view metaToolGuardApplies consulted. Handing them l.ToolGuardPolicy
	// would enforce something the predicate never agreed to: its no-match
	// fallback IS clamp(Builtin), so any ceiling at all would drag Builtin's
	// breaker onto these tools.
	guardDeps := l.toolGuardDeps()
	guardDeps.Policy = l.metaToolGuardPolicy()
	recordDeps := l.toolGuardRecordDeps()
	recordDeps.Policy = guardDeps.Policy

	guard := toolguard.NewGuard(guardDeps)
	preDec := guard.Eval(ctx, pipeline.Input{
		Session: sessRef,
		Tool:    &pipeline.ToolCallInfo{Name: name, Args: args, UseID: useID},
	})
	switch preDec.Verdict {
	case pipeline.Deny:
		return tool.Result{Content: preDec.Reason, IsError: true},
			containedOutcome{Phase: containPreDeny, Reason: preDec.Reason}
	case pipeline.Halt:
		return l.haltMetaToolCall(ctx, sess, name, preDec.Reason, containPreHalt)
	}

	res := exec(ctx)

	rec := toolguard.NewGuardRecord(recordDeps)
	postDec := rec.Eval(ctx, pipeline.Input{
		Session: sessRef,
		Tool: &pipeline.ToolCallInfo{
			Name: name, Args: args, UseID: useID,
			Result: res.Content, IsError: res.IsError,
		},
	})
	switch postDec.Verdict {
	case pipeline.Deny:
		// An ingress-budget deny withholds the payload: the model must see that
		// the result was withheld and why, never the oversized body.
		return tool.Result{Content: postDec.Reason, IsError: true},
			containedOutcome{Phase: containPostDeny, Reason: postDec.Reason}
	case pipeline.Halt:
		return l.haltMetaToolCall(ctx, sess, name, postDec.Reason, containPostHalt)
	}
	return res, containedOutcome{Phase: containRanOK}
}

// haltMetaToolCall writes the terminal Failed status for a toolguard halt, the
// same side effect pipeline.Executor performs via host.Halt on the gated path —
// without it the dispatcher would record a halt the session's status never
// reflects. The Halt error is logged rather than returned: the halt verdict
// stands regardless, and swallowing the failure to record it would leave an
// operator with a stopped session and no explanation.
func (l *Loop) haltMetaToolCall(
	ctx context.Context, sess *tool.SessionContext, name, reason string, phase containPhase,
) (tool.Result, containedOutcome) {
	host := newRunnerHost(l, hostSession{Namespace: sess.Namespace, Name: sess.Name, Class: l.AgentName})
	if err := host.Halt(ctx, reason); err != nil {
		slog.Default().Info("toolguard: writing the halt status for a meta tool failed",
			"session", sess.Namespace+"/"+sess.Name, "tool", name, "reason", reason, "err", err.Error())
	}
	return tool.Result{Content: reason, IsError: true}, containedOutcome{Phase: phase, Reason: reason}
}
