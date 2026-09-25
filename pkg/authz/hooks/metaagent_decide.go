package hooks

import (
	"context"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// metaagent_decide.go is the THIRD metaagent control-plane stage hook
// (Name "metaagent_decide"). It composes the approver summary, runs the
// approval round-trip (or auto-applies), and records the resolved bool into the
// Host scratch for Apply.
//
// The Decide hook OWNS the approval round-trip itself (via the RequestApproval
// dep, which the worker binds to the Host's PublishApproval + AwaitDecision)
// rather than emitting a pipeline.ApprovalAsk for the executor to resolve. This
// is deliberate: Apply must run on BOTH approve and deny (mid_session deny →
// sticky HardDeny conversion; cold_start deny → StatusDenied task), so Decide
// must NOT let the executor's approval-deny short-circuit stop the sequence. It
// resolves the bool here, records it into scratch, and returns Allow so the
// worker always runs Apply (which branches on PeekApproved).
//
// The metaagent_scope_approval payload stays BYTE-IDENTICAL: the hook hands the
// ApprovalAsk (Kind + Payload) to RequestApproval, which routes through the
// Host's PublishApproval — the SAME publishColdStart / publishMetaagentScope
// that stamps the 5-button (coldStart:true) vs 3-button (no coldStart) payload.
// The hook does NOT rebuild the payload. The composer/approver-summary LLM sees
// only the classified delta (prompt-injection-safe).

// MidSessionApprovalTimeout is the AwaitDecision cap for mid-session @metaagent
// approvals. An explicit intentional exception to authz.approvalTimeout:
// mid-session scope changes wait ~session-lifetime (the human clicks when they
// are ready), NOT the shorter cold-start window.
const MidSessionApprovalTimeout = 24 * time.Hour

// MetaagentDecideDeps configures the MetaagentDecide hook.
type MetaagentDecideDeps struct {
	// Envelope feeds DetectCaveats (the classified delta arrives via PeekExtract).
	Envelope scope.AgentClassEnvelope

	// PeekExtract returns the classified delta + shape + cleaned task Extract wrote.
	PeekExtract func() (delta scope.ScopeDelta, shape, cleaned string)

	// Compose builds the approver summary (+ skipped/caveat explanations) from the
	// schema/delta ONLY — no user text. Optional; a nil/erroring composer leaves
	// the summary empty (the structured Delta is still authoritative).
	Compose func(ctx context.Context, applied scope.ScopeDelta, skipped []scope.SkippedItem, caveats []scope.CaveatItem) (summary, skippedExplain, caveatExplain string)

	// RequestApproval runs the EFFECT-FREE human-gate round-trip (the worker binds
	// it to the Host's ResolveApproval). The Host builds the byte-identical payload
	// from ask.Kind + ask.Payload. Returns the resolved bool + the cold-start 5-way
	// action (coldstart.Action*; "" for metaagent_scope). An await error maps to a
	// sticky no — NOT a Halt. The hook does NOT apply or write — MetaagentApply does.
	RequestApproval func(ctx context.Context, ask pipeline.ApprovalAsk) (approved bool, action string, err error)

	// SetDecide records the composed output; SetApproved records the resolved bool
	// + cold-start action (both → Host scratch for Apply).
	SetDecide   func(out scope.MetaagentOutput)
	SetApproved func(approved bool, action string)

	// ColdStartTimeout is the per-await cap for cold_start approvals (the runner's
	// AgentClass authz.approvalTimeout). mid_session uses MidSessionApprovalTimeout.
	ColdStartTimeout time.Duration

	Logger *slog.Logger
}

// MetaagentDecide is the compose + approval stage.
type MetaagentDecide struct{ d MetaagentDecideDeps }

// NewMetaagentDecide constructs the MetaagentDecide hook.
func NewMetaagentDecide(d MetaagentDecideDeps) *MetaagentDecide {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &MetaagentDecide{d: d}
}

func (h *MetaagentDecide) Name() string { return "metaagent_decide" }
func (h *MetaagentDecide) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.MetaagentDecide}
}

func (h *MetaagentDecide) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	kind, requester, text, autoApply, inboxIdx := "", "", "", false, 0
	if in.Metaagent != nil {
		kind = in.Metaagent.Kind
		requester = in.Metaagent.Requester
		text = in.Metaagent.Text
		autoApply = in.Metaagent.AutoApply
		inboxIdx = in.Metaagent.InboxIdx
	}
	coldStart := kind == "cold_start"

	var applied scope.ScopeDelta
	var cleaned string
	if h.d.PeekExtract != nil {
		applied, _, cleaned = h.d.PeekExtract()
	}

	// DetectCaveats over the applied delta (classified at Extract). The
	// ClassifySkipped widen-bounding already happened at Extract; Decide consumes it.
	caveats := scope.DetectCaveats(applied, h.d.Envelope, nil)
	var skipped []scope.SkippedItem

	summary, skippedExplain, caveatExplain := "", "", ""
	if h.d.Compose != nil {
		summary, skippedExplain, caveatExplain = h.d.Compose(ctx, applied, skipped, caveats)
	}
	if summary == "" {
		summary = "Approval requested for a scope change. Click Show Details for the technical view."
	}

	out := scope.MetaagentOutput{
		Delta:           applied,
		Skipped:         skipped,
		Caveats:         caveats,
		ApproverSummary: summary,
		SkippedExplain:  skippedExplain,
		CaveatExplain:   caveatExplain,
	}
	if h.d.SetDecide != nil {
		h.d.SetDecide(out)
	}

	// autoApply (cold_start extractAndAutoApply): no human gate. Resolve to an
	// approve. cold_start carries the 5-way action (approve_cleaned) so Apply
	// writes the cleaned task; mid_session carries "" (a plain approve).
	if autoApply {
		if h.d.SetApproved != nil {
			act := ""
			if coldStart {
				act = "approve_cleaned"
			}
			h.d.SetApproved(true, act)
		}
		return pipeline.Decision{}
	}

	// Build the ApprovalAsk. The Host's PublishApproval (routed via RequestApproval)
	// stamps the byte-identical payload — cold_start adds coldStart:true +
	// cleanedTask (5-button); metaagent_scope omits them (3-button).
	pl := scope.MetaagentApprovalPayload{
		Requester:       requester,
		Verbatim:        text,
		ApproverSummary: summary,
		SkippedExplain:  skippedExplain,
		CaveatExplain:   caveatExplain,
		Applied:         applied,
		Skipped:         skipped,
		Caveats:         caveats,
	}
	ask := pipeline.ApprovalAsk{Summary: summary}
	if coldStart {
		ask.Kind = "cold_start"
		ask.Timeout = h.d.ColdStartTimeout
		pl.ColdStart = true
		pl.CleanedTask = cleaned
		pl.InboxIdx = inboxIdx
	} else {
		ask.Kind = "metaagent_scope"
		ask.Timeout = MidSessionApprovalTimeout
	}
	ask.Payload = pl.ToAskPayload()

	approved, action := false, ""
	if h.d.RequestApproval != nil {
		ok, act, err := h.d.RequestApproval(ctx, ask)
		if err != nil {
			// An await/publish error maps to a sticky no (preserving the
			// timeout→deny semantics), NOT a Halt: Apply still runs the deny path.
			h.d.Logger.Info("metaagent_decide: approval round-trip errored; treating as sticky no",
				"session", in.Session.String(), "kind", kind, "err", err.Error())
			approved, action = false, ""
		} else {
			approved, action = ok, act
		}
	} else {
		h.d.Logger.Info("metaagent_decide: RequestApproval not wired; treating as sticky no",
			"session", in.Session.String())
	}
	if h.d.SetApproved != nil {
		h.d.SetApproved(approved, action)
	}
	return pipeline.Decision{}
}

var _ pipeline.Hook = (*MetaagentDecide)(nil)
