package toolguard

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// Event is one enforcement-relevant occurrence, fed to the nil-safe
// RecordAudit dep (→ toolguard_audit memory kind) and to structured logs.
type Event struct {
	// Event: breaker_opened | breaker_half_open | breaker_closed |
	// rate_limit_hit | egress_limit_hit | ingress_limit_hit |
	// guard_deny | guard_warn | guard_halt | credential_halt
	//
	// credential_halt has its own name rather than reusing guard_halt so an
	// operator can tell "a policy stopped this run because a credential is
	// dead" from "a policy stopped it because a budget was blown" without
	// re-deriving it from Key.
	Event  string
	Tool   string
	Origin string
	// Key is the breaker key: "tool/<name>" for per-tool breakers, "origin/<ref>" for origin-level breakers.
	Key   string
	UseID string

	Trips   int32
	CoolOff time.Duration
	RetryAt time.Time

	// Limit: "per_turn" | "window" for rate events; "egress" | "ingress" for
	// byte-limit events.
	Limit string
	// ObservedBytes is the measured size for byte-limit events (0 otherwise).
	ObservedBytes int64

	Action string
	// Provenance is the rule source tier:
	// "class[i]|namespace[i]|cluster[i]|builtin|ceiling".
	Provenance string
}

// byteLimitDecision logs + audits a byte-limit hit and maps ByteAction to a
// pipeline verdict. dim is "egress" or "ingress". audit may be nil. The
// warn/halt event names reuse the generic guard_warn/guard_halt convention
// (matching the rate path); only the deny case carries the dimension-specific
// <dim>_limit_hit name. observed/limit are surfaced for logs + audit.
func byteLimitDecision(
	ctx context.Context,
	logger *slog.Logger,
	audit func(context.Context, Event),
	in pipeline.Input,
	origin string,
	rule ResolvedRule,
	dim string,
	observed, limit int64,
) pipeline.Decision {
	ev := Event{
		Tool: in.Tool.Name, Origin: origin, Key: ToolKey(in.Tool.Name),
		UseID: in.Tool.UseID, Limit: dim, ObservedBytes: observed,
		Action: rule.ByteAction.String(), Provenance: rule.Provenance,
	}
	logger.Info("toolguard: data-volume limit hit",
		"session", in.Session.String(), "tool", in.Tool.Name, "origin", origin,
		"dimension", dim, "observedBytes", observed, "limitBytes", limit,
		"action", rule.ByteAction.String(), "rule", rule.Provenance)
	emit := func(name string) {
		if audit != nil {
			ev.Event = name
			audit(ctx, ev)
		}
	}
	msg := byteDenyMessage(in.Tool.Name, dim, limit)
	switch rule.ByteAction {
	case ActionWarn:
		emit("guard_warn")
		return pipeline.Decision{}
	case ActionHalt:
		emit("guard_halt")
		// "tool_guard:" prefix routes runnerHost.Halt to the ToolGuardHalt reason.
		return pipeline.Decision{Verdict: pipeline.Halt, Reason: "tool_guard: " + msg}
	default: // deny
		emit(dim + "_limit_hit")
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: msg}
	}
}

// byteDenyMessage is the deny-reason text for a byte-limit denial. For
// "egress" and "ingress" it is LLM-facing IsError text, explicit that the
// limit is hard (retrying the same call will not help); those two are
// unchanged here. For "ui_ingress" it instead reaches a BROWSER — the result
// of a UI data binding, never the model — so it is plain human copy with no
// tool name or internal vocabulary.
func byteDenyMessage(toolName, dim string, limit int64) string {
	switch dim {
	case "egress":
		return fmt.Sprintf(
			"tool %q: the call was blocked because its arguments exceeded the %d-byte outbound limit. Send less data (shorter input, fewer items, or a reference instead of inline content), or report to the user; retrying the same call will not help.",
			toolName, limit)
	case "ui_ingress":
		return "This view's data is too large to display. Narrow the time span, add a filter, or ask for fewer rows."
	default: // ingress
		return fmt.Sprintf(
			"tool %q: the result exceeded the %d-byte limit and was withheld. Request a smaller slice (a narrower query, pagination, or a filter), or report what you have to the user; retrying the same call will not help.",
			toolName, limit)
	}
}
