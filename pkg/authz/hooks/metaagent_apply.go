package hooks

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/authz/coldstart"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// metaagent_apply.go is the FOURTH and final metaagent control-plane stage hook
// (Name "metaagent_apply"). It performs the out-of-band mutation + records the
// outcome + notifies the requester, reading the resolved decision + classified
// delta from the per-request Host scratch.
//
// cold_start (resolved via the 5-way action in scratch):
//   - approve_cleaned/approve_original → ApplyScope(delta) + write
//     StatusApprovedCleaned (with cleaned text) / StatusApprovedOriginal (no text)
//   - run_without_scope → no apply + write StatusRanWithoutScope
//   - deny → no apply + write StatusDenied. Cold-start deny aborts the first turn
//     entirely (a pre-session gate); the Add is NOT converted to a sticky
//     hard-deny (there is no active session to harden — matches the old
//     ColdStartHandler deny path).
//
// mid_session (resolved via the bool in scratch):
//   - approve → ApplyScope(delta) (the live scope mutates; the running agent's
//     next tool dispatch sees it via the data-plane Scope hook)
//   - deny → convert the Add into a sticky Layer-2 HardDeny and ApplyScope it
//     ("no" is sticky — the deny→sticky-disallow rule, preserved). Narrows on a
//     deny are simply not applied (nothing to make sticky).

// MetaagentApplyDeps configures the MetaagentApply hook. All closures are
// supplied by the authzd worker; the hook stays import-clean of cmd/*.
type MetaagentApplyDeps struct {
	// PeekExtract returns the classified delta + shape + cleaned task.
	PeekExtract func() (delta scope.ScopeDelta, shape, cleaned string)
	// PeekApproved returns the resolved bool + cold-start 5-way action.
	PeekApproved func() (approved bool, action string)

	// ApplyScope applies a delta to the session scope (Metaagent.ApplyScopeChange).
	ApplyScope func(ctx context.Context, d scope.ScopeDelta) error
	// WriteTask persists the cold_start_task outcome (cold_start only).
	WriteTask func(ctx context.Context, c coldstarttask.Content) error
	// NotifyRequester delivers a requester-facing outcome notice.
	NotifyRequester func(ctx context.Context, body string)
	// Audit writes one metaagent_audit record for the resolved outcome.
	Audit func(ctx context.Context, action string, appliedDelta *scope.ScopeDelta)

	// EmitScopeMutated records a ScopeMutated lifecycle event after a
	// mid-session scope change is successfully applied. It is an in-flight
	// event (no phase change) — the operator fold records it in the
	// session's signed timeline. No-op when nil or for cold_start kind
	// (cold-start scope changes are covered by the DecisionAsked/Resolved pair).
	EmitScopeMutated func(ctx context.Context)

	Logger *slog.Logger
}

// MetaagentApply is the mutation + record + notify stage.
type MetaagentApply struct{ d MetaagentApplyDeps }

// NewMetaagentApply constructs the MetaagentApply hook.
func NewMetaagentApply(d MetaagentApplyDeps) *MetaagentApply {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &MetaagentApply{d: d}
}

func (h *MetaagentApply) Name() string { return "metaagent_apply" }
func (h *MetaagentApply) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.MetaagentApply}
}

func (h *MetaagentApply) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	kind, inboxIdx := "", 0
	if in.Metaagent != nil {
		kind, inboxIdx = in.Metaagent.Kind, in.Metaagent.InboxIdx
	}

	var delta scope.ScopeDelta
	var cleaned string
	if h.d.PeekExtract != nil {
		delta, _, cleaned = h.d.PeekExtract()
	}
	approved, action := false, ""
	if h.d.PeekApproved != nil {
		approved, action = h.d.PeekApproved()
	}

	if kind == "cold_start" {
		return h.applyColdStart(ctx, delta, cleaned, action, inboxIdx)
	}
	return h.applyMidSession(ctx, delta, approved)
}

func (h *MetaagentApply) applyColdStart(ctx context.Context, delta scope.ScopeDelta, cleaned, action string, inboxIdx int) pipeline.Decision {
	switch action {
	case coldstart.ActionApproveCleaned, coldstart.ActionApproveOriginal:
		if err := h.applyScope(ctx, delta); err != nil {
			h.audit(ctx, action, nil)
			h.d.Logger.Info("metaagent_apply: cold-start apply failed; writing review-failed",
				"err", err.Error())
			h.writeTask(ctx, coldstarttask.Content{Status: coldstarttask.StatusScopeReviewFailed, InboxIdx: inboxIdx})
			return pipeline.Decision{}
		}
		appliedCopy := delta
		h.audit(ctx, action, &appliedCopy)
		h.notify(ctx, "Scope set. The agent is now working on your request.")
		status, text := coldstarttask.StatusApprovedCleaned, cleaned
		if action == coldstart.ActionApproveOriginal {
			status, text = coldstarttask.StatusApprovedOriginal, ""
		}
		h.writeTask(ctx, coldstarttask.Content{Status: status, CleanedText: text, InboxIdx: inboxIdx})

	case coldstart.ActionRunWithoutScope:
		h.audit(ctx, action, nil)
		h.notify(ctx, "Running your request without scope changes.")
		h.writeTask(ctx, coldstarttask.Content{Status: coldstarttask.StatusRanWithoutScope, InboxIdx: inboxIdx})

	default: // coldstart.ActionDeny (and any unexpected action) → deny, no apply.
		h.audit(ctx, coldstart.ActionDeny, nil)
		h.notify(ctx, "Your request was declined; the agent will not run it.")
		h.writeTask(ctx, coldstarttask.Content{Status: coldstarttask.StatusDenied, InboxIdx: inboxIdx})
	}
	return pipeline.Decision{}
}

func (h *MetaagentApply) applyMidSession(ctx context.Context, delta scope.ScopeDelta, approved bool) pipeline.Decision {
	if approved {
		if err := h.applyScope(ctx, delta); err != nil {
			h.audit(ctx, "approve", nil)
			h.d.Logger.Info("metaagent_apply: mid-session apply failed", "err", err.Error())
			h.notify(ctx, "Your scope change could not be applied due to an internal error.")
			return pipeline.Decision{}
		}
		appliedCopy := delta
		h.audit(ctx, "approve", &appliedCopy)
		// Record the live scope mutation in the signed lifecycle log. This is an
		// in-flight event (no phase change) so the operator timeline shows scope
		// changes that occurred while the agent was running.
		if h.d.EmitScopeMutated != nil {
			h.d.EmitScopeMutated(ctx)
		}
		h.notify(ctx, "Scope change applied.")
		return pipeline.Decision{}
	}

	// Deny: convert the widenings (Add) into sticky Layer-2 HardDeny tuples —
	// "no" is sticky. Narrows on a deny are simply not applied.
	conv := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{
			Resources:        append([]scope.ResourceRef(nil), delta.Add.Resources...),
			ResourcePatterns: append([]scope.ResourcePattern(nil), delta.Add.ResourcePatterns...),
			Tools:            append([]string(nil), delta.Add.Tools...),
			ArgConstraints:   append([]scope.ArgConstraint(nil), delta.Add.ArgConstraints...),
		},
	}
	if !conv.IsEmpty() {
		if err := h.applyScope(ctx, conv); err != nil {
			h.audit(ctx, "deny", nil)
			h.d.Logger.Info("metaagent_apply: mid-session deny-conversion apply failed", "err", err.Error())
			h.notify(ctx, "Your scope change was denied (an internal error occurred recording it).")
			return pipeline.Decision{}
		}
	}
	h.audit(ctx, "deny", nil)
	h.notify(ctx, "Scope change denied.")
	return pipeline.Decision{}
}

func (h *MetaagentApply) applyScope(ctx context.Context, d scope.ScopeDelta) error {
	if h.d.ApplyScope == nil {
		return nil
	}
	return h.d.ApplyScope(ctx, d)
}

func (h *MetaagentApply) writeTask(ctx context.Context, c coldstarttask.Content) {
	if h.d.WriteTask == nil {
		return
	}
	if err := h.d.WriteTask(ctx, c); err != nil {
		h.d.Logger.Info("metaagent_apply: write cold_start_task failed",
			"status", c.Status, "err", err.Error())
	}
}

func (h *MetaagentApply) notify(ctx context.Context, body string) {
	if h.d.NotifyRequester != nil {
		h.d.NotifyRequester(ctx, body)
	}
}

func (h *MetaagentApply) audit(ctx context.Context, action string, applied *scope.ScopeDelta) {
	if h.d.Audit != nil {
		h.d.Audit(ctx, action, applied)
	}
}

var _ pipeline.Hook = (*MetaagentApply)(nil)
