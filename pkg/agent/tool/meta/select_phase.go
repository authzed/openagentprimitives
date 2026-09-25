// pkg/agent/tool/meta/select_phase.go
package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

// SelectPhaseConfig wires select_phase to the runtime's frozen plan and its
// append-only log.
//
// Both are injected rather than reached for, so the tool stays a pure
// bounds-check plus a record and is testable without a session.
type SelectPhaseConfig struct {
	// ActivePlan returns the frozen approved plan, and whether one exists.
	ActivePlan func(ctx context.Context) (plangate.Plan, bool)

	// Record appends the selection to the audit log. The agent supplies WHICH
	// phase; this is the runtime attesting THAT the switch happened.
	Record func(ctx context.Context, index int) error

	// PhaseCompletion reports, per phase index, whether its work is finished.
	//
	// Optional. Nil means the runtime cannot establish completion, and every
	// transition proceeds — a wiring gap must not wedge every session whose plan
	// has an ordering edge. When present, absence of an index means UNFINISHED:
	// no evidence of completion is not evidence of completion.
	PhaseCompletion func(ctx context.Context) map[int]bool

	// PhaseEntries reports, per phase index, how many times the runtime has
	// seen it ENTERED. It is what makes MaxSpec.Count mean something: the
	// approver's card states maxCount and this tool's own description tells the
	// model a phase "may normally be entered once", and until this existed
	// nothing compared the ledger to the limit.
	//
	// Optional, and nil means the runtime cannot establish the count so every
	// transition proceeds — same stance as PhaseCompletion, and for the same
	// reason: a wiring gap must not wedge every session with a bounded phase.
	// Unlike completion, absence of an index here is UNAMBIGUOUS (zero
	// entries), so there is no fail-closed reading to choose.
	PhaseEntries func(ctx context.Context) map[int]int
}

// NewSelectPhase constructs the select_phase meta tool.
func NewSelectPhase(cfg SelectPhaseConfig) tool.Tool { return &selectPhaseTool{cfg: cfg} }

type selectPhaseTool struct{ cfg SelectPhaseConfig }

func (*selectPhaseTool) Name() string    { return "select_phase" }
func (*selectPhaseTool) Kind() tool.Kind { return tool.KindMeta }

// Permission is Passthrough, like update_plan: select_phase never appears on
// the permission surface and is never itself gated.
//
// It cannot widen anything. Every index it can name leads to a ceiling a human
// already approved, so gating the selector would only add a second approval in
// front of a decision already made. What it CAN do — move the agent between
// approved ceilings — is bounded by the frozen list and recorded, which is the
// property that matters.
func (*selectPhaseTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}

func (*selectPhaseTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*selectPhaseTool) Description() string {
	return "Switch to a different phase of the approved plan. Pass the phase's INDEX in the plan " +
		"(0 is the first phase). You hold exactly one phase's permissions at a time: selecting a phase " +
		"gives you its permissions and gives up the previous phase's, so select the phase whose work you " +
		"are about to do. Every selection is recorded. A phase may normally be entered once — re-entering " +
		"one you have already used spends another of its allowed entries, and running out means asking " +
		"the user."
}

func (*selectPhaseTool) InputSchema() json.RawMessage {
	// An INDEX, deliberately — never a name. Freezing drops the agent's phase
	// ids precisely so authorization cannot key on something the agent chose;
	// an id-based selector here would hand that back.
	return json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"index":{"type":"integer","minimum":0}
		},
		"required":["index"]
	}`)
}

type selectPhaseArgs struct {
	Index int `json:"index"`
}

func (t *selectPhaseTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var a selectPhaseArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"index":1}`); !ok {
		return res, nil
	}

	if t.cfg.ActivePlan == nil {
		return tool.Result{
			Content: "select_phase: no approved plan is in force for this session",
			IsError: true, Trusted: true,
		}, nil
	}
	plan, ok := t.cfg.ActivePlan(ctx)
	if !ok || len(plan.Phases) == 0 {
		// No frozen list to index into. Defaulting to phase 0 would invent a
		// position the log never recorded.
		return tool.Result{
			Content: "select_phase: no approved plan is in force for this session; " +
				"declare phases with update_plan and have them approved first",
			IsError: true, Trusted: true,
		}, nil
	}

	if a.Index < 0 || a.Index >= len(plan.Phases) {
		return tool.Result{
			Content: fmt.Sprintf("select_phase: index %d is out of range; this plan has %d phases (valid indexes 0..%d)",
				a.Index, len(plan.Phases), len(plan.Phases)-1),
			IsError: true, Trusted: true,
		}, nil
	}

	// Completion discipline. Leaving a phase unfinished is fine — it is simply
	// marked — and only a DEPENDENCY blocks: opening a phase whose prerequisite
	// has not finished runs work the approver was told would come second.
	//
	// Checked BEFORE Record, and that ordering is the point: a refused
	// transition must not reach the log, or the fold would move the agent into
	// a phase this call just refused to open.
	if t.cfg.PhaseCompletion != nil {
		if blocked, why := plangate.BlockedByIncompletePrerequisite(
			plan, a.Index, t.cfg.PhaseCompletion(ctx),
		); blocked {
			return tool.Result{
				Content: "select_phase: " + why,
				IsError: true, Trusted: true,
			}, nil
		}
	}

	// The ENTRY budget. Checked here, before Record, for the same reason the
	// prerequisite check is: a refused transition must not reach the log, or
	// the fold would move the agent into a phase this call just refused to
	// open.
	//
	// Count == 0 means the author declared no bound, which is unbounded — not
	// zero entries. Reading it the other way would wedge every plan that never
	// mentioned max.
	if t.cfg.PhaseEntries != nil {
		if limit := plan.Phases[a.Index].Max.Count; limit > 0 {
			if spent := t.cfg.PhaseEntries(ctx)[a.Index]; spent >= limit {
				return tool.Result{
					Content: fmt.Sprintf(
						"select_phase: phase %d (%s) was approved to run %d time(s) and has already been entered %d time(s). "+
							"Re-entering it needs a person's approval, which this tool cannot ask for. "+
							"Finish the work in the phase you are in, or re-plan with update_plan and say why the extra pass is needed.",
						a.Index, plan.Phases[a.Index].Label, limit, spent),
					IsError: true, Trusted: true,
				}, nil
			}
		}
	}

	if t.cfg.Record != nil {
		if err := t.cfg.Record(ctx, a.Index); err != nil {
			// A selection that was not durably recorded did not happen. Saying
			// otherwise would leave the agent believing it holds a ceiling the
			// fold will not agree it has.
			return tool.Result{
				Content: "select_phase: could not record the selection, so it did not take effect: " + err.Error(),
				IsError: true, Trusted: true,
			}, nil
		}
	}

	return tool.Result{
		Content: fmt.Sprintf("Now in phase %d: %s", a.Index, plan.Phases[a.Index].Label),
		Trusted: true,
	}, nil
}
