package hooks

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// session_fork.go is the operator's SessionFork control-plane gate. It runs
// pre-materialization and is cheap: a single positive SpiceDB check of
// agentsession#fork for the forker (bound fully-consistent + the "user:"
// prefix stripped inside the deps closure). No LLM / untrusted text → no
// quarantine concern, so it runs directly in the operator.
//
// v1 policy is owner-only (agentsession#fork = owner). A non-owner, a
// checker error, or an unwired checker all fail CLOSED (Deny) and attach a
// requester-facing Notice the executor delivers via the Host. The operator
// REQUIRES SpiceDB at startup, so the unwired-Deny path is defensive only.

// SessionForkDeps configures the SessionFork hook. CheckFork wraps
// authz.CheckSessionFork bound fully-consistent (the deps closure strips the
// "user:" prefix before the SpiceDB call). nil ⇒ fail closed (Deny).
type SessionForkDeps struct {
	CheckFork func(ctx context.Context, ns, name string, subject identity.CanonicalUserID) (bool, error)
	Logger    *slog.Logger
}

// SessionFork is the fork authz gate hook.
type SessionFork struct{ d SessionForkDeps }

// NewSessionFork constructs the SessionFork hook.
func NewSessionFork(d SessionForkDeps) *SessionFork {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &SessionFork{d: d}
}

func (h *SessionFork) Name() string { return "session_fork" }
func (h *SessionFork) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.SessionFork}
}

// forkDeniedNotice is refused-but-recoverable: the requester cannot fork this
// session, but asking its owner is a real path forward.
func forkDeniedNotice() *notice.Notice {
	return notice.New(categories.ForkDenied, notice.Args{
		Lead:     "Only the session owner can restart this conversation",
		NextStep: "Ask the person who started it to restart from here.",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester},
	})
}

func (h *SessionFork) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	deny := func(reason string) pipeline.Decision {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  reason,
			Notices: []pipeline.Notice{{Notice: forkDeniedNotice(), ToRequester: true}},
		}
	}

	if h.d.CheckFork == nil {
		h.d.Logger.Info("session_fork: CheckFork not wired; denying (fail-closed)",
			"session", in.Session.String())
		return deny("session_fork: fork checker not wired")
	}

	ok, err := h.d.CheckFork(ctx, in.Session.Namespace, in.Session.Name, in.Requester)
	if err != nil {
		h.d.Logger.Info("session_fork: fork check errored; denying (fail-closed)",
			"session", in.Session.String(), "forker", in.Requester, "err", err.Error())
		return deny("session_fork: fork check error: " + err.Error())
	}
	if !ok {
		h.d.Logger.Info("session_fork: forker not authorized; denying",
			"session", in.Session.String(), "forker", in.Requester)
		return deny("session_fork: forker not authorized to fork this session")
	}
	return pipeline.Decision{}
}

var _ pipeline.Hook = (*SessionFork)(nil)
