package hooks

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// SessionCleanupDeps is the dependency struct for the SessionCleanup hook.
//
// RemoveExternalState is the wired-if-present hook for removing agent-added
// external config the runner may have written for this session (the "external
// state: remove, don't restore" policy). It is nil today — there is no
// runner-owned external state at SessionEnd — and a nil dep is a no-op.
type SessionCleanupDeps struct {
	RemoveExternalState func(ctx context.Context) error // nil ⇒ no-op
	Logger              *slog.Logger
}

// SessionCleanup is the SessionEnd hook for session-teardown bookkeeping: a
// finalize-audit record carrying the end reason, plus external-state removal
// when that dep is wired.
//
// THIN BY DESIGN, and it must stay that way. The heavy, security-relevant
// teardown — revoking every per-session SpiceDB grant tuple (tool AND leakage),
// deleting the memory scope, reaping the toolspec-reader CRB and passthrough
// Role/RoleBinding, revoking the session token — belongs to the operator's
// AgentSession finalizer, which runs on deletion regardless of runner liveness.
// That finalizer is the DURABLE BACKSTOP: an OOM-killed or SIGKILLed runner
// never returns from Run() and never fires this hook, so anything duplicated
// here would simply be lost. As it stands, losing this hook costs an audit
// breadcrumb, never a grant.
type SessionCleanup struct{ d SessionCleanupDeps }

// NewSessionCleanup creates a SessionCleanup hook.
func NewSessionCleanup(d SessionCleanupDeps) *SessionCleanup {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &SessionCleanup{d: d}
}

func (h *SessionCleanup) Name() string             { return "session_cleanup" }
func (h *SessionCleanup) Points() []pipeline.Point { return []pipeline.Point{pipeline.SessionEnd} }

func (h *SessionCleanup) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	reason := ""
	if in.End != nil {
		reason = in.End.Reason
	}

	// Wired-if-present external-state removal. A removal error must NEVER abort
	// the terminal write the runner is about to perform — log + swallow.
	if h.d.RemoveExternalState != nil {
		if err := h.d.RemoveExternalState(ctx); err != nil {
			h.d.Logger.Info("session_cleanup: remove external state failed",
				"session", in.Session.String(), "reason", reason, "err", err.Error())
		}
	}

	return pipeline.Decision{
		Verdict: pipeline.Allow,
		Audit: []pipeline.AuditRecord{{
			Kind: "session_end",
			Fields: map[string]any{
				"reason":  reason,
				"session": in.Session.String(),
			},
		}},
	}
}

var _ pipeline.Hook = (*SessionCleanup)(nil)
