package toolguard

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// RecordDeps wires the post-hook. RecordAudit, PatchStatus, Logger nil-safe.
type RecordDeps struct {
	Policy      *ResolvedPolicy
	Registry    *Registry
	LookupTool  func(name string) (kind, origin string)
	RecordAudit func(ctx context.Context, ev Event)
	// PatchStatus pushes the open-breaker snapshot to AgentSession status.
	// Called only when a transition happened (trip/close), not per call.
	PatchStatus func(ctx context.Context, snap []OpenBreakerInfo)
	Logger      *slog.Logger
}

// GuardRecord is the PostToolCall hook: it feeds Execute outcomes into the
// breaker registry (which never gates) and the credential-halt layer (which
// ends the session on a streak of unbilled failures — see credential_halt.go),
// and, when an ingress byte budget is configured, gates oversized successful
// results — returning Deny/Halt so the dispatch loop withholds the payload.
type GuardRecord struct{ d RecordDeps }

// NewGuardRecord builds the PostToolCall recording hook; Logger defaults to slog.Default().
func NewGuardRecord(d RecordDeps) *GuardRecord {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &GuardRecord{d: d}
}

func (h *GuardRecord) Name() string             { return "tool_guard_record" }
func (h *GuardRecord) Points() []pipeline.Point { return []pipeline.Point{pipeline.PostToolCall} }

func (h *GuardRecord) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil {
		return pipeline.Decision{}
	}
	kind, origin := h.d.LookupTool(in.Tool.Name)
	if kind == "" {
		return pipeline.Decision{}
	}
	rule := h.d.Policy.RuleFor(kind, in.Tool.Name, origin)
	limit := rule.IngressLimitFor(in.UIDataBinding)
	hasBreaker := rule.FailureThreshold > 0 || rule.OriginFailureThreshold > 0
	if !hasBreaker && limit == 0 && rule.AuthHaltThreshold <= 0 {
		return pipeline.Decision{}
	}

	if hasBreaker {
		trans := h.d.Registry.Record(ToolKey(in.Tool.Name), OriginKey(origin), rule, in.Tool.IsError)
		for _, tr := range trans {
			h.d.Logger.Info("toolguard: breaker transition",
				"session", in.Session.String(), "tool", in.Tool.Name, "origin", origin,
				"event", tr.Event, "key", tr.Key, "trips", tr.Trips,
				"coolOff", tr.CoolOff.String(), "rule", rule.Provenance)
			if h.d.RecordAudit != nil {
				h.d.RecordAudit(ctx, Event{
					Event: tr.Event, Tool: in.Tool.Name, Origin: origin, Key: tr.Key,
					UseID: in.Tool.UseID, Trips: tr.Trips, CoolOff: tr.CoolOff,
					RetryAt: tr.RetryAt, Action: rule.Action.String(), Provenance: rule.Provenance,
				})
			}
		}
		if len(trans) > 0 && h.d.PatchStatus != nil {
			h.d.PatchStatus(ctx, h.d.Registry.OpenBreakers())
		}
	}

	// Credential halt. Recorded on EVERY call, because a success or a billed
	// failure is what clears the streak, and answered with a halt rather than a
	// deny: the breaker's "unavailable, retry in ~30s" is right for an upstream
	// that is down and wrong for a credential that will never work, which is
	// how one dead key came to spend a session's whole turn budget with nothing
	// told to the operator.
	//
	// Both conjuncts are required. IsError is the call's outcome; the carrier
	// is the platform's structured observation ABOUT that outcome, and neither
	// alone is the claim being made here.
	if h.d.Registry.RecordAuth(
		OriginKey(origin), rule.AuthHaltThreshold,
		in.Tool.IsError && in.Tool.UnbilledFailure,
	) {
		return h.credentialHalt(ctx, in, origin, rule)
	}

	// Ingress byte budget: applies to ALL results, error included — a tool
	// could mark an unbounded exfil payload IsError to dodge the cap, and the
	// success-with-IsError MCP path returns full content. Checked AFTER breaker
	// recording so the breaker still observes the true Execute outcome; on deny
	// the dispatch loop withholds the payload. Which ceiling applies is
	// IngressLimitFor's decision, not a branch here.
	if limit > 0 {
		if n := int64(len(in.Tool.Result)); n > limit {
			dim := "ingress"
			if in.UIDataBinding {
				dim = "ui_ingress"
			}
			return byteLimitDecision(ctx, h.d.Logger, h.d.RecordAudit, in, origin, rule, dim, n, limit)
		}
	}
	return pipeline.Decision{}
}
