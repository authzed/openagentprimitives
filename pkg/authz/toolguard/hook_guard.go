package toolguard

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// GuardDeps wires the pre-hook. RecordAudit and Logger are nil-safe.
type GuardDeps struct {
	// Policy is the resolved (folded) set of rules applied each call.
	Policy *ResolvedPolicy
	// Registry holds per-breaker and per-rate-window counters for this session.
	Registry *Registry
	// LookupTool maps an LLM tool name to (kind, origin). origin "" for
	// origin-less tools; ("", "") for unknown names (guard no-ops).
	LookupTool func(name string) (kind, origin string)
	// TurnIndex extracts the current turn from the dispatch ctx (the runner
	// wires sandbox.IDsFromCtx). Used for per-turn rate counters.
	TurnIndex func(ctx context.Context) int
	// RecordAudit, when non-nil, receives every enforcement-relevant Event (audit memory).
	RecordAudit func(ctx context.Context, ev Event)
	Logger      *slog.Logger
}

// Guard is the PreToolCall hook: admits or denies per the resolved rule.
type Guard struct{ d GuardDeps }

// NewGuard builds the PreToolCall guard hook; Logger defaults to slog.Default().
func NewGuard(d GuardDeps) *Guard {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Guard{d: d}
}

func (h *Guard) Name() string             { return "tool_guard" }
func (h *Guard) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreToolCall} }

func (h *Guard) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil {
		return pipeline.Decision{}
	}
	kind, origin := h.d.LookupTool(in.Tool.Name)
	if kind == "" {
		return pipeline.Decision{}
	}
	rule := h.d.Policy.RuleFor(kind, in.Tool.Name, origin)
	if rule.Disabled() {
		return pipeline.Decision{}
	}

	// Egress byte budget: a static property of the args, checked before Admit
	// so an over-limit call consumes neither a rate slot nor a breaker probe.
	if rule.MaxEgressBytes > 0 {
		if n := int64(len(in.Tool.Args)); n > rule.MaxEgressBytes {
			return byteLimitDecision(ctx, h.d.Logger, h.d.RecordAudit, in, origin, rule, "egress", n, rule.MaxEgressBytes)
		}
	}

	adm, trans := h.d.Registry.Admit(ctx, ToolKey(in.Tool.Name), OriginKey(origin), rule, h.d.TurnIndex(ctx))
	h.emitTransitions(ctx, in, origin, rule, trans)
	if adm.Probe && probeLedgerFrom(ctx) == nil {
		// Wiring regression at the call site: without a ledger, only a
		// PostToolCall record can hand this claim back, so any exit that skips
		// PostToolCall (deny, halt, interrupt) wedges the key for the session.
		h.d.Logger.Info("toolguard: half-open probe claimed with no probe ledger in ctx; an outcome-less exit would strand it",
			"session", in.Session.String(), "tool", in.Tool.Name, "origin", origin,
			"rule", rule.Provenance)
	}
	if adm.Allowed {
		return pipeline.Decision{}
	}

	action := rule.Action
	limit := ""
	switch adm.DeniedBy {
	case "rate_turn":
		action, limit = rule.RateAction, "per_turn"
	case "rate_window":
		action, limit = rule.RateAction, "window"
	}
	msg := denyMessage(in.Tool.Name, rule, adm)

	ev := Event{
		Tool: in.Tool.Name, Origin: origin, Key: adm.Key, UseID: in.Tool.UseID,
		RetryAt: time.Time{}, Limit: limit,
		Action: action.String(), Provenance: rule.Provenance,
	}
	h.d.Logger.Info("toolguard: call gated",
		"session", in.Session.String(), "tool", in.Tool.Name, "origin", origin,
		"deniedBy", adm.DeniedBy, "key", adm.Key, "action", action.String(),
		"retryAfter", adm.RetryAfter.String(), "rule", rule.Provenance)

	switch action {
	case ActionWarn:
		ev.Event = "guard_warn"
		h.audit(ctx, ev)
		return pipeline.Decision{}
	case ActionHalt:
		ev.Event = "guard_halt"
		h.audit(ctx, ev)
		// The "tool_guard:" prefix routes runnerHost.Halt to
		// ReasonAgentSessionToolGuardHalt.
		return pipeline.Decision{Verdict: pipeline.Halt, Reason: "tool_guard: " + msg}
	default:
		if limit != "" {
			ev.Event = "rate_limit_hit"
		} else {
			ev.Event = "guard_deny"
		}
		h.audit(ctx, ev)
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: msg}
	}
}

func (h *Guard) emitTransitions(ctx context.Context, in pipeline.Input, origin string, rule ResolvedRule, trans []Transition) {
	for _, tr := range trans {
		h.d.Logger.Info("toolguard: breaker transition",
			"session", in.Session.String(), "tool", in.Tool.Name, "origin", origin,
			"event", tr.Event, "key", tr.Key, "trips", tr.Trips,
			"coolOff", tr.CoolOff.String(), "rule", rule.Provenance)
		h.audit(ctx, Event{
			Event: tr.Event, Tool: in.Tool.Name, Origin: origin, Key: tr.Key,
			UseID: in.Tool.UseID, Trips: tr.Trips, CoolOff: tr.CoolOff,
			RetryAt: tr.RetryAt, Action: rule.Action.String(), Provenance: rule.Provenance,
		})
	}
}

func (h *Guard) audit(ctx context.Context, ev Event) {
	if h.d.RecordAudit != nil {
		h.d.RecordAudit(ctx, ev)
	}
}

// denyMessage is the LLM-facing IsError text — explicit about what happened
// and what to do instead, phrased so the model doesn't treat rate exhaustion
// as transient.
func denyMessage(toolName string, rule ResolvedRule, adm Admission) string {
	switch adm.DeniedBy {
	case "rate_turn":
		return fmt.Sprintf(
			"tool %q: call budget for this tool is exhausted this turn (max %d calls/turn). Do not call it again this turn; use a different tool or report progress to the user.",
			toolName, rule.RateMaxPerTurn)
	case "rate_window":
		// The bound that actually fired, which a rule carrying both an authored
		// window and an admin ceiling's is not free to guess at.
		return fmt.Sprintf(
			"tool %q: call budget exhausted (max %d calls per %s). Do not retry it now; use a different tool or report progress to the user.",
			toolName, adm.RateLimit.MaxCalls, adm.RateLimit.Window)
	case "probe":
		return fmt.Sprintf(
			"tool %q is temporarily unavailable: its circuit breaker is verifying recovery (a probe call is in flight). Do not retry it now; use a different tool or report the failure to the user.",
			toolName)
	default: // "breaker"
		where := ""
		if rest, ok := strings.CutPrefix(adm.Key, "origin/"); ok {
			where = fmt.Sprintf(" (origin %s)", rest)
		}
		return fmt.Sprintf(
			"tool %q is temporarily unavailable: circuit breaker open after repeated failures%s. Do not retry it for ~%s; use a different tool or report the failure to the user.",
			toolName, where, adm.RetryAfter.Round(time.Second))
	}
}
