package hooks

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// metaagent_extract.go is the SECOND metaagent control-plane stage hook
// (Name "metaagent_extract"). It runs the untrusted-text extractor LLM INSIDE
// authzd's process (the prompt-injection quarantine: the Extract closure is the
// authzd-side extractor; the composer/approver-summary LLM never sees user text).
//
// Responsibilities (spec §3 Extract, §4 shape):
//  1. Run the extractor over the raw text → a ScopeDelta (+ cleaned task for
//     cold_start). Fail CLOSED on extractor error: Halt (cold_start writes
//     StatusScopeReviewFailed so the runner halts; mid_session notifies + aborts).
//     Never apply an unparsed change.
//  2. ClassifySkipped(proposed, envelope, requesterPerms) → applied delta. This is
//     where the widen-is-privileged rule lives (an Add is kept only for resources
//     the requester can access). It runs here so Decide consumes the already-bounded
//     delta.
//  3. Derive the SHAPE (widen|narrow|mixed) from the applied delta and write
//     delta/shape/cleaned to the Host scratch for Decide/Apply.
//  4. No-op / cannot-address short-circuit: if the applied delta is empty there is
//     nothing to approve. cold_start writes StatusApprovedCleaned (or
//     StatusApprovedOriginal) and returns Deny to short-circuit the sequence (the
//     task is already written — no Decide/Apply). mid_session notifies the
//     requester nothing was applicable and short-circuits. A Deny here is NOT a
//     security denial — it is "handled; nothing for Decide/Apply to do"; the
//     worker stops on any non-Allow verdict.

// MetaagentExtractDeps configures the MetaagentExtract hook. All closures are
// supplied by the authzd worker; the hook stays import-clean of cmd/*.
type MetaagentExtractDeps struct {
	// Extract runs the untrusted-text extractor LLM (the worker binds requester +
	// envelope; the hook supplies the user text). Returns a delta + cleaned task
	// (cleaned is cold_start-only; mid_session ignores it).
	Extract func(ctx context.Context, text string) (scope.ColdStartExtraction, error)

	// Envelope / RequesterPerms feed ClassifySkipped (the widen-bounding).
	Envelope       scope.AgentClassEnvelope
	RequesterPerms scope.RequesterPerms

	// SetExtract writes the classified delta + derived shape + cleaned task to the
	// Host scratch.
	SetExtract func(delta scope.ScopeDelta, shape, cleaned string)

	// WriteTask persists a cold_start_task (cold_start fail-closed + no-op paths).
	WriteTask func(ctx context.Context, c coldstarttask.Content) error
	// NotifyRequester delivers a requester-facing notice.
	NotifyRequester func(ctx context.Context, body string)
	// Audit writes one metaagent_audit record for an outcome the hook owns.
	Audit func(ctx context.Context, action string, appliedDelta *scope.ScopeDelta)

	Logger *slog.Logger
}

// MetaagentExtract is the LLM-extract + classify + shape stage.
type MetaagentExtract struct{ d MetaagentExtractDeps }

// NewMetaagentExtract constructs the MetaagentExtract hook.
func NewMetaagentExtract(d MetaagentExtractDeps) *MetaagentExtract {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &MetaagentExtract{d: d}
}

func (h *MetaagentExtract) Name() string { return "metaagent_extract" }
func (h *MetaagentExtract) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.MetaagentExtract}
}

func (h *MetaagentExtract) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	kind, text, inboxIdx := "", "", 0
	if in.Metaagent != nil {
		kind, text, inboxIdx = in.Metaagent.Kind, in.Metaagent.Text, in.Metaagent.InboxIdx
	}
	coldStart := kind == "cold_start"

	if h.d.Extract == nil {
		h.d.Logger.Info("metaagent_extract: Extract not wired; failing closed",
			"session", in.Session.String())
		if coldStart {
			h.writeTask(ctx, coldstarttask.Content{Status: coldstarttask.StatusScopeReviewFailed, InboxIdx: inboxIdx})
		}
		h.notify(ctx, "Scope review for your request could not be completed. No action was taken.")
		return failClosed("metaagent_extract: extractor not wired")
	}

	ext, err := h.d.Extract(ctx, text)
	if err != nil {
		// Fail CLOSED: never apply an unparsed change. cold_start writes
		// StatusScopeReviewFailed so the runner halts; mid_session aborts + notifies.
		h.d.Logger.Info("metaagent_extract: extractor failed; failing closed",
			"session", in.Session.String(), "kind", kind, "err", err.Error())
		h.notify(ctx, "I couldn't analyze your request for permissions, so no change was made. Please try again.")
		h.audit(ctx, "scope_review_failed(extractor_error)", nil)
		if coldStart {
			h.writeTask(ctx, coldstarttask.Content{Status: coldstarttask.StatusScopeReviewFailed, InboxIdx: inboxIdx})
		}
		return failClosed("metaagent_extract: extractor failed: " + err.Error())
	}

	applied, _ := scope.ClassifySkipped(ext.ScopeDelta, h.d.Envelope, h.d.RequesterPerms)

	// No-op / cannot-address: nothing applicable to approve.
	if applied.IsEmpty() {
		h.audit(ctx, "no_scope_change", nil)
		if coldStart {
			// Run the (cleaned) task immediately — no approval needed.
			status, cleaned := coldstarttask.StatusApprovedCleaned, ext.CleanedTask
			if cleaned == "" {
				status = coldstarttask.StatusApprovedOriginal
			}
			h.writeTask(ctx, coldstarttask.Content{Status: status, CleanedText: cleaned, InboxIdx: inboxIdx})
		} else {
			h.notify(ctx, "I couldn't apply any part of that request — see the audit log for details.")
		}
		// Short-circuit the staged sequence: the outcome is fully handled here, so
		// there is nothing for Decide/Apply to do. NOT a security denial.
		return pipeline.Decision{Verdict: pipeline.Deny, Reason: "metaagent_extract: no applicable scope change"}
	}

	// Carry the classified delta + derived shape + cleaned task to Decide/Apply.
	if h.d.SetExtract != nil {
		h.d.SetExtract(applied, applied.Shape(), ext.CleanedTask)
	}
	return pipeline.Decision{}
}

func (h *MetaagentExtract) writeTask(ctx context.Context, c coldstarttask.Content) {
	if h.d.WriteTask == nil {
		return
	}
	if err := h.d.WriteTask(ctx, c); err != nil {
		h.d.Logger.Info("metaagent_extract: write cold_start_task failed",
			"status", c.Status, "err", err.Error())
	}
}

func (h *MetaagentExtract) notify(ctx context.Context, body string) {
	if h.d.NotifyRequester != nil {
		h.d.NotifyRequester(ctx, body)
	}
}

func (h *MetaagentExtract) audit(ctx context.Context, action string, applied *scope.ScopeDelta) {
	if h.d.Audit != nil {
		h.d.Audit(ctx, action, applied)
	}
}

// failClosed builds a Halt decision carrying a fail-closed reason + the
// requester notice. For cold_start the runner maps the Halt →
// ReasonAgentSessionScopeReviewFailed; for mid_session the change is simply
// aborted (no session to halt).
func failClosed(reason string) pipeline.Decision {
	return pipeline.Decision{
		Verdict: pipeline.Halt,
		Reason:  reason,
		Notices: []pipeline.Notice{{
			Notice: notice.New(categories.SessionHalted, notice.Args{
				Lead: "Scope review couldn't be completed",
				// "No action was taken" is the load-bearing sentence: a
				// fail-closed halt means nothing ran, and a reader who assumes
				// otherwise may go looking for changes to undo.
				Body:     "The session was stopped before running. No action was taken.",
				NextStep: "Try again; if it keeps happening, ask an operator to check the scope reviewer.",
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester},
			}),
			ToRequester: true,
		}},
	}
}

var _ pipeline.Hook = (*MetaagentExtract)(nil)
