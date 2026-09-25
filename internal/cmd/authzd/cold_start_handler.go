package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/coldstart"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
)

// The five cold-start actions now live in pkg/authz/coldstart (importable so the
// authzd pipeline Host can reference them without importing this package main).
// These aliases keep the existing internal/cmd/authzd call sites + tests unchanged.
const (
	ColdStartApproveCleaned  = coldstart.ActionApproveCleaned
	ColdStartApproveOriginal = coldstart.ActionApproveOriginal
	ColdStartRunWithoutScope = coldstart.ActionRunWithoutScope
	ColdStartDeny            = coldstart.ActionDeny
)

// coldStartExtractorIface is the extractor surface ColdStartHandler needs.
// *ColdStartExtractor satisfies it; tests use a fake.
type coldStartExtractorIface interface {
	ExtractColdStart(ctx context.Context, in ExtractorInput) (scope.ColdStartExtraction, error)
}

// ColdStartHandler runs the new-session first-turn flow: extract scope +
// cleaned task, classify, (optionally) gate on the approver, apply scope per
// the chosen action, and record the runner's instruction in cold_start_task.
type ColdStartHandler struct {
	Metaagent *Metaagent
	Extractor coldStartExtractorIface
}

// ColdStartRequest carries the caller-supplied context for one new-session
// first turn.
type ColdStartRequest struct {
	Requester      string
	RequestText    string
	Envelope       scope.AgentClassEnvelope
	RequesterPerms scope.RequesterPerms
	ToolReads      map[string]scope.ToolReads
	InboxIdx       int
	AutoApply      bool // coldStart == extractAndAutoApply
}

// ColdStartDecide receives the composed output + cleaned task; returns one of
// the ColdStart* actions. Production wires it through the approval
// orchestrator + Slack buttons; tests pass a stub.
type ColdStartDecide func(ctx context.Context, out scope.MetaagentOutput, cleanedTask string) (string, error)

// coldStartAutoApprover is the Approver value used when scope is applied
// automatically (AutoApply=true). No human clicked approve; the system
// decided based on the AgentClass policy.
const coldStartAutoApprover = "system:auto"

// Handle runs the cold-start pipeline for one new session's first turn:
// extractor LLM (untrusted input) → deterministic classification → composer LLM
// (no user text) → approver decision → apply per action. On extractor failure it
// fails OPEN, recording StatusRanWithoutScope so the runner still runs the raw
// turn.
func (h *ColdStartHandler) Handle(ctx context.Context, scopeRef memory.Scope, sessRef SessionRef, req ColdStartRequest, decide ColdStartDecide) error {
	ext, err := h.Extractor.ExtractColdStart(ctx, ExtractorInput{
		UserRequest:        req.RequestText,
		Requester:          req.Requester,
		AgentClassEnvelope: req.Envelope,
	})
	if err != nil {
		// Fail CLOSED: scope review is a security gate. If the extractor cannot
		// analyze the request (LLM error, timeout, nil provider) we must NOT run
		// the agent unscoped — the runner halts the session on StatusScopeReviewFailed.
		// Log the underlying error (no silent errors) so an operator can diagnose
		// the extractor failure, and tell the requester the session was stopped.
		slog.Error("cold-start: extractor failed; failing session closed (agent will NOT run unscoped)",
			"session", scopeRef.ID, "requester", req.Requester, "err", err.Error())
		h.Metaagent.notify(ctx, scopeRef, req.Requester,
			"I couldn't analyze your request for permissions, so I stopped the session for safety. Please try again.")
		h.auditColdStart(ctx, scopeRef, req, scope.ScopeDelta{}, scope.MetaagentOutput{},
			"scope_review_failed(extractor_error)", nil)
		if werr := h.write(ctx, scopeRef, coldstarttask.Content{Status: coldstarttask.StatusScopeReviewFailed, InboxIdx: req.InboxIdx}); werr != nil {
			return werr
		}
		return fmt.Errorf("cold-start: extractor failed: %w", err)
	}

	// No-op fast path: the extractor found no permission change (no expansion or
	// contraction). Don't bother the approver — let the agent start immediately on
	// the (cleaned) task. No scope is applied. If nothing was stripped, run the
	// original request verbatim.
	if ext.ScopeDelta.IsEmpty() {
		h.auditColdStart(ctx, scopeRef, req, ext.ScopeDelta, scope.MetaagentOutput{}, "no_scope_change", nil)
		status, text := coldstarttask.StatusApprovedCleaned, ext.CleanedTask
		if text == "" {
			status, text = coldstarttask.StatusApprovedOriginal, ""
		}
		return h.write(ctx, scopeRef, coldstarttask.Content{Status: status, CleanedText: text, InboxIdx: req.InboxIdx})
	}

	applied, skipped := scope.ClassifySkipped(ext.ScopeDelta, req.Envelope, req.RequesterPerms)
	caveats := scope.DetectCaveats(applied, req.Envelope, req.ToolReads)
	out := scope.MetaagentOutput{Delta: applied, Skipped: skipped, Caveats: caveats}

	action := ColdStartApproveCleaned
	approver := coldStartAutoApprover
	if !req.AutoApply {
		if h.Metaagent.Composer != nil {
			// Best-effort prose for the approval block; composer failure is
			// non-fatal — the structured Delta is still authoritative.
			if comp, cerr := h.Metaagent.Composer.Compose(ctx, ComposerInput{
				Applied: applied, Skipped: skipped, Caveats: caveats,
			}); cerr == nil {
				out.ApproverSummary = comp.ApproverSummary
			}
		}
		a, derr := decide(ctx, out, ext.CleanedTask)
		if derr != nil {
			return fmt.Errorf("cold-start decide: %w", derr)
		}
		action = a
		// TODO(approver-identity): req.Requester is the session initiator, not
		// necessarily the person who clicked approve. The real approver identity
		// is recorded by the approval layer (Phase D2/F). Set "" here to avoid
		// implying the requester approved their own request.
		approver = ""
	}

	switch action {
	case ColdStartApproveCleaned, ColdStartApproveOriginal:
		if err := h.Metaagent.ApplyScopeChange(ctx, scopeRef, sessRef, applied); err != nil {
			h.auditColdStart(ctx, scopeRef, req, ext.ScopeDelta, out, action, nil)
			return fmt.Errorf("cold-start apply scope: %w", err)
		}
		h.auditColdStart(ctx, scopeRef, req, ext.ScopeDelta, out, action, &applied)
		// Confirm to the requester that scope is set and the agent is starting.
		// Generic by design: the applied delta is available but a rich summary is
		// a follow-on; the requester just needs to know the gate cleared.
		h.Metaagent.notify(ctx, scopeRef, req.Requester,
			"Scope set. The agent is now working on your request.")
		status, text := coldstarttask.StatusApprovedCleaned, ext.CleanedTask
		if action == ColdStartApproveOriginal {
			status, text = coldstarttask.StatusApprovedOriginal, ""
		}
		// An empty CleanedTask (approve_cleaned with nothing to clean) is handled
		// by the runner (Phase E): it places no user turn when CleanedText is empty.
		return h.write(ctx, scopeRef, coldstarttask.Content{
			Status: status, CleanedText: text, InboxIdx: req.InboxIdx, Approver: approver,
		})
	case ColdStartRunWithoutScope:
		h.auditColdStart(ctx, scopeRef, req, ext.ScopeDelta, out, action, nil)
		h.Metaagent.notify(ctx, scopeRef, req.Requester,
			"Running your request without scope changes.")
		return h.write(ctx, scopeRef, coldstarttask.Content{Status: coldstarttask.StatusRanWithoutScope, InboxIdx: req.InboxIdx})
	case ColdStartDeny:
		// Cold-start deny aborts the first turn entirely ("abort + notify" per spec).
		// This is a pre-session gate, NOT a within-session re-request — there is no
		// existing scope to harden. Widening requests are NOT converted to sticky
		// SpiceDB hard-deny tuples here, unlike the mid-session deny path: the session
		// simply does not start, the requester is notified, and the runner sees
		// StatusDenied.
		h.Metaagent.notify(ctx, scopeRef, req.Requester, "Your request was declined; the agent will not run it.")
		h.auditColdStart(ctx, scopeRef, req, ext.ScopeDelta, out, action, nil)
		return h.write(ctx, scopeRef, coldstarttask.Content{Status: coldstarttask.StatusDenied, InboxIdx: req.InboxIdx})
	default:
		return fmt.Errorf("cold-start: unknown action %q", action)
	}
}

// auditColdStart writes a best-effort metaagent_audit record for one
// cold-start outcome. Mirrors Metaagent.audit but operates on ColdStartRequest
// and the cold-start-specific proposed/applied deltas. Best-effort: a write
// failure must not block the cold-start flow, but it IS logged (no silent
// errors).
func (h *ColdStartHandler) auditColdStart(
	ctx context.Context, scopeRef memory.Scope, req ColdStartRequest,
	proposed scope.ScopeDelta, out scope.MetaagentOutput,
	decision string, appliedDelta *scope.ScopeDelta,
) {
	if err := metaagentaudit.Record(ctx, h.Metaagent.Memory, scopeRef, metaagentaudit.Content{
		Ts:            time.Now().UTC(),
		Requester:     req.Requester,
		RequestText:   req.RequestText,
		ProposedDelta: proposed,
		Classification: metaagentaudit.Classification{
			Applied: out.Delta, Skipped: out.Skipped, Caveats: out.Caveats,
		},
		ApproverDecision: decision,
		AppliedDelta:     appliedDelta,
	}); err != nil {
		slog.Info("cold-start: metaagentaudit.Record failed",
			"session", scopeRef.ID, "requester", req.Requester, "decision", decision, "err", err.Error())
	}
}

func (h *ColdStartHandler) write(ctx context.Context, scopeRef memory.Scope, c coldstarttask.Content) error {
	c.DecidedAt = time.Now().UTC()
	return coldstarttask.Put(ctx, h.Metaagent.Memory, scopeRef, c)
}
