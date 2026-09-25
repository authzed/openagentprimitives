package hooks

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// metaagent_received.go is the FIRST metaagent control-plane stage hook
// (Name "metaagent_received"). It runs pre-LLM and is cheap: it stamps the
// classified Kind into the per-request Host scratch and runs the manage_scope
// owner gate.
//
// Kind classification (spec §4): the Kind is knowable WITHOUT the LLM — it is
// the trigger source (cold_start = session-start trigger; mid_session =
// @mention), already on Input.Metaagent.Kind (the worker stamps it from the
// payload's coldStart bool). Received copies it to scratch so downstream stages
// branch on it; there is no heuristic and no model call here.
//
// manage_scope gate (resolved decision #3):
//   - cold_start SKIPS the gate. The requester IS definitionally started_by
//     (the runner's SessionStart hook published it), so the check is
//     tautological — there is no third party to gate, and it needs no SpiceDB.
//   - mid_session ENFORCES the gate via the deps-injected, fully-consistent
//     manage_scope check. A non-owner is Denied (the intended tightening: a
//     thread participant can #interact but not change scope). A check error or
//     unwired checker fails CLOSED (Deny). In a running authzd the checker is
//     always wired (SpiceDB is required at startup), so the unwired-Deny path
//     is defensive only.

// MetaagentReceivedDeps configures the MetaagentReceived hook. CheckManageScope
// wraps the SpiceDB agentsession#manage_scope check (authz.CheckSessionManageScope
// bound fully-consistent + the canonical subject). SetKind writes the classified
// kind to the Host scratch.
type MetaagentReceivedDeps struct {
	// CheckManageScope answers "may this subject change the session's scope?".
	// The worker binds it to a fully-consistent SpiceDB check with the bare
	// canonical subject (identity.CanonicalUserID — already stripped of any
	// "user:" prefix by the caller). nil ⇒ mid_session fails closed (Deny);
	// cold_start never consults it.
	CheckManageScope func(ctx context.Context, ns, name string, subject identity.CanonicalUserID) (bool, error)

	// SetKind records the classified kind into the per-request Host scratch.
	SetKind func(kind string)

	Logger *slog.Logger
}

// MetaagentReceived is the gate+classify stage.
type MetaagentReceived struct{ d MetaagentReceivedDeps }

// NewMetaagentReceived constructs the MetaagentReceived hook.
func NewMetaagentReceived(d MetaagentReceivedDeps) *MetaagentReceived {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &MetaagentReceived{d: d}
}

func (h *MetaagentReceived) Name() string { return "metaagent_received" }
func (h *MetaagentReceived) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.MetaagentReceived}
}

func (h *MetaagentReceived) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	kind := ""
	if in.Metaagent != nil {
		kind = in.Metaagent.Kind
	}
	// Stamp the classified kind into scratch first — classification is
	// independent of the gate outcome (downstream stages and audit want the
	// kind even on a Deny).
	if h.d.SetKind != nil {
		h.d.SetKind(kind)
	}

	if kind == "cold_start" {
		// Tautological: cold_start's requester is definitionally started_by (the
		// runner's SessionStart hook published it). No third party to gate, no
		// SpiceDB needed. Audited via the executor's approval/audit path downstream.
		return pipeline.Decision{}
	}

	// mid_session: enforce the manage_scope owner gate.
	if h.d.CheckManageScope == nil {
		h.d.Logger.Info("metaagent_received: CheckManageScope not wired; denying (fail-closed)",
			"session", in.Session.String())
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  "metaagent_received: manage_scope checker not wired",
		}
	}

	ok, err := h.d.CheckManageScope(ctx, in.Session.Namespace, in.Session.Name, in.Requester)
	if err != nil {
		h.d.Logger.Info("metaagent_received: manage_scope check errored; denying (fail-closed)",
			"session", in.Session.String(), "requester", in.Requester, "err", err.Error())
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  "metaagent_received: manage_scope check error: " + err.Error(),
		}
	}
	if !ok {
		h.d.Logger.Info("metaagent_received: requester not authorized to change scope; denying",
			"session", in.Session.String(), "requester", in.Requester)
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  "metaagent_received: requester not authorized to change session scope",
		}
	}
	return pipeline.Decision{}
}

var _ pipeline.Hook = (*MetaagentReceived)(nil)
