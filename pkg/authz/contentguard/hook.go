package contentguard

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// Event is the neutral audit record the adapter emits per Inspect (the runner
// wires record→contentguardaudit.Record). Always emitted, including on Pass.
type Event struct {
	Inspector string
	Action    string // pass|block|approve
	Tool      string
	Point     string
	Reason    string
	Details   map[string]any
}

// adapter wraps one configured Instance as a pipeline.Hook.
type adapter struct {
	id        string
	inst      Instance
	record    func(context.Context, Event)
	logger    *slog.Logger
	onTimeout pipeline.TimeoutPolicy
}

// NewAdapter wraps inst as a pipeline.Hook firing at inst.Points(). record is
// called once per Inspect (the runner writes it to contentguard_audit).
// onTimeout is stamped onto every content_inspection ApprovalAsk this adapter
// builds; the caller sources it from the lifecycle decisionParams table
// (contentguard must not import lifecycle, so the policy is injected here
// rather than computed) — see runner.timeoutPolicyFor.
//
// inst is wrapped in Capped here rather than at the call site so that every
// pipeline adapter — present or future, whoever builds it — inspects a bounded
// subject. See cap.go for why an unbounded one is a security hole and not just
// a latency one.
func NewAdapter(id string, inst Instance, record func(context.Context, Event), logger *slog.Logger, onTimeout pipeline.TimeoutPolicy) pipeline.Hook {
	return &adapter{id: id, inst: Capped(inst), record: record, logger: logger, onTimeout: onTimeout}
}

func (a *adapter) Name() string             { return "content_guard:" + a.id }
func (a *adapter) Points() []pipeline.Point { return a.inst.Points() }

func (a *adapter) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil {
		return pipeline.Decision{} // not a tool point; nothing to inspect
	}
	// SubjectFor is the shared Point→content mapping (contentguard.go): both this
	// adapter and the runner's ungated meta path build their Subject through it,
	// so neither can inspect a point the other silently skips. An unknown point
	// yields an error, not an empty Subject an inspector would Pass.
	s, err := SubjectFor(in.Point, in.Tool.Name, in.Tool.Args, in.Tool.Result, in.Tool.IsError)
	if err != nil {
		a.logger.Info("content_guard: no subject for this point (fail-closed Block)",
			"inspector", a.id, "tool", in.Tool.Name, "point", string(in.Point), "err", err.Error())
		a.emit(ctx, "block", in, err.Error(), map[string]any{"error": err.Error()})
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: "content guard " + a.id + ": " + err.Error()}
	}

	f, err := a.inst.Inspect(ctx, s)
	if err != nil {
		// Fail-closed: a guard that errors blocks the content.
		a.logger.Info("content_guard: inspect error (fail-closed Block)",
			"inspector", a.id, "tool", in.Tool.Name, "point", string(in.Point), "err", err.Error())
		a.emit(ctx, "block", in, "inspector error", map[string]any{"error": err.Error()})
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: "content guard " + a.id + ": inspection failed"}
	}

	switch f.Action {
	case Block:
		a.emit(ctx, "block", in, f.Reason, f.Details)
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: f.Reason}
	case Approve:
		a.emit(ctx, "approve", in, f.Reason, f.Details)
		return pipeline.Decision{Approval: &pipeline.ApprovalAsk{
			Kind:      "content_inspection",
			Summary:   f.Reason,
			Payload:   map[string]any{"inspector": a.id, "tool": in.Tool.Name, "details": f.Details},
			OnTimeout: a.onTimeout,
		}}
	default: // Pass
		a.emit(ctx, "pass", in, f.Reason, f.Details)
		return pipeline.Decision{}
	}
}

func (a *adapter) emit(ctx context.Context, action string, in pipeline.Input, reason string, details map[string]any) {
	if a.record == nil {
		return
	}
	a.record(ctx, Event{
		Inspector: a.id, Action: action, Tool: in.Tool.Name,
		Point: string(in.Point), Reason: reason, Details: details,
	})
}
