package hooks

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The dispatch-time half of the trifecta. The other half is a containment
// judgement in the OPERATOR (hold.Tripper), and the split is not stylistic:
// hold's own doc says "the evidence it reads is produced by the runner, and
// the runner is the party under suspicion — a compromised one would simply
// decline to trip itself." A trifecta control that only ever ran here would be
// exactly that.
//
// What THIS half asks is narrower: may this CALL proceed? Legs A and B are
// standing properties of the session's bound data; leg C is the call itself.
// The trifecta completes at the moment a session already holding untrusted
// input and sensitive access makes a consequential call — which is the moment
// to refuse, and the last moment it is still cheap.

// TrifectaDeps wires the dispatch-time check.
type TrifectaDeps struct {
	// Mode is disabled | logging | enforcing, matching planGate.mode and the
	// info-leakage policy so an operator has one mental model.
	//
	// Its OWN mode. Never derive it from toolCalls.mode: that gate returns
	// before ForceApproval when disabled, so a trifecta hung off it would
	// disappear whenever an operator turned off an unrelated check.
	Mode string

	// StandingLegs returns the session's legs A and B — derived from the data
	// bound into it. Leg C on the returned value is IGNORED: this hook decides
	// C from the call in front of it, not from the session's whole surface,
	// because the question here is whether THIS call completes the trifecta.
	StandingLegs func(ctx context.Context) (trifecta.Legs, error)

	// CallImpact returns the StateImpact the call in front of the hook runs
	// under. Leg C is readwrite or external; readonly is leg B's territory.
	CallImpact func(toolName string, args map[string]any) (authz.StateImpact, error)

	// InClosureDenialSet reports whether this session sits in a closure that
	// has already been denied (§2.8). Refused in EVERY mode — a structural
	// precondition rather than a policy judgement, following requirePlan's
	// precedent in the plan gate.
	InClosureDenialSet func(ctx context.Context) (bool, error)

	Logger *slog.Logger
}

// Trifecta is the PreToolCall hook.
type Trifecta struct{ d TrifectaDeps }

// NewTrifecta builds the hook.
func NewTrifecta(d TrifectaDeps) *Trifecta {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Trifecta{d: d}
}

func (*Trifecta) Name() string { return "trifecta" }

func (*Trifecta) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.PreToolCall}
}

func (h *Trifecta) enforcing() bool { return h.d.Mode == "enforcing" }

func (h *Trifecta) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil {
		return pipeline.Decision{}
	}

	// The closure denial set is checked FIRST and in every mode, including
	// disabled. A denied closure is a structural fact — this delegation has
	// already been judged to have gone wrong — not a policy judgement about
	// this call, and the plan gate sets the same precedent with requirePlan.
	if h.d.InClosureDenialSet != nil {
		denied, err := h.d.InClosureDenialSet(ctx)
		if err != nil {
			// Unanswerable: refuse rather than assume clean. A closure we
			// cannot establish the standing of is not one to keep running.
			return pipeline.Decision{
				Verdict: pipeline.Deny,
				Reason:  fmt.Sprintf("trifecta: could not establish whether this closure is denied: %v", err),
			}
		}
		if denied {
			return pipeline.Decision{
				Verdict: pipeline.Deny,
				Reason: "trifecta: this delegation closure has been denied, so no call from it proceeds. " +
					"That refusal is structural and applies in every mode.",
				Audit: []pipeline.AuditRecord{{Kind: "trifecta_closure_denied"}},
			}
		}
	}

	if h.d.Mode == "" || h.d.Mode == "disabled" {
		return pipeline.Decision{}
	}
	if h.d.StandingLegs == nil || h.d.CallImpact == nil {
		return pipeline.Decision{}
	}

	legs, err := h.d.StandingLegs(ctx)
	if err != nil {
		return h.unresolvable(in, "standing legs", err)
	}
	impact, err := h.d.CallImpact(in.Tool.Name, argsMapOf(in))
	if err != nil {
		return h.unresolvable(in, "call impact", err)
	}

	// C comes from the CALL. A session whose surface contains a writing tool
	// has not completed the trifecta by making a read.
	legs.Consequential = impact == authz.Readwrite || impact == authz.External

	v := trifecta.Evaluate(legs)
	rec := pipeline.AuditRecord{
		Kind: "trifecta",
		Fields: map[string]any{
			"tool": in.Tool.Name, "legs": v.LegCount, "refused": v.Refused, "mode": h.d.Mode,
		},
	}
	if !v.Refused {
		// Recorded even when allowed: two-leg near-misses are the dataset
		// logging mode exists to collect, and they are invisible if only
		// refusals are written.
		return pipeline.Decision{Audit: []pipeline.AuditRecord{rec}}
	}
	if !h.enforcing() {
		h.d.Logger.Info("trifecta: would deny",
			"tool", in.Tool.Name, "session", in.Session.String(), "reason", v.Reason)
		return pipeline.Decision{Audit: []pipeline.AuditRecord{rec}}
	}
	return pipeline.Decision{Verdict: pipeline.Deny, Reason: "trifecta: " + v.Reason, Audit: []pipeline.AuditRecord{rec}}
}

// unresolvable refuses when enforcing and records otherwise.
//
// A leg that could not be established is UNKNOWN, and under enforcement the
// alternative is running a call whose closure nobody can characterise. Under
// logging it is recorded loudly instead — the mode exists to gather evidence,
// and failing the session would stop it gathering any.
func (h *Trifecta) unresolvable(in pipeline.Input, what string, err error) pipeline.Decision {
	h.d.Logger.Info("trifecta: could not establish "+what,
		"tool", in.Tool.Name, "session", in.Session.String(), "mode", h.d.Mode, "err", err.Error())
	rec := pipeline.AuditRecord{
		Kind:   "trifecta_unresolvable",
		Fields: map[string]any{"tool": in.Tool.Name, "what": what, "mode": h.d.Mode},
	}
	if !h.enforcing() {
		return pipeline.Decision{Audit: []pipeline.AuditRecord{rec}}
	}
	return pipeline.Decision{
		Verdict: pipeline.Deny,
		Reason:  fmt.Sprintf("trifecta: could not establish %s for this call, so its closure cannot be characterised: %v", what, err),
		Audit:   []pipeline.AuditRecord{rec},
	}
}
