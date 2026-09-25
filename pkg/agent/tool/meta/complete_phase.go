// pkg/agent/tool/meta/complete_phase.go
package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

// CompletePhaseConfig wires complete_phase to the frozen plan and the
// append-only log.
type CompletePhaseConfig struct {
	// ActivePlan returns the frozen approved plan, and whether one exists.
	ActivePlan func(ctx context.Context) (plangate.Plan, bool)

	// Record appends the completion. The agent supplies WHICH phase and what it
	// achieved; this is the runtime attesting THAT the declaration was made, and
	// when.
	Record func(ctx context.Context, index int, outcome string) error

	// Entered reports which phases the runtime has seen entered. A phase that
	// was never entered cannot be completed — see Execute.
	Entered func(ctx context.Context) map[int]bool
}

// NewCompletePhase constructs the complete_phase meta tool.
func NewCompletePhase(cfg CompletePhaseConfig) tool.Tool {
	return &completePhaseTool{cfg: cfg}
}

type completePhaseTool struct{ cfg CompletePhaseConfig }

func (*completePhaseTool) Name() string    { return "complete_phase" }
func (*completePhaseTool) Kind() tool.Kind { return tool.KindMeta }

// Permission is Passthrough, like select_phase and update_plan: completing a
// phase never appears on the permission surface and is never itself gated.
//
// It cannot widen anything — it only ever WITHHOLDS. A phase left incomplete
// blocks the phases that declared a dependency on it; completing one releases
// them, and every phase it releases was already approved with that ordering
// visible. There is no reach on the other side of this call.
func (*completePhaseTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}

func (*completePhaseTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*completePhaseTool) Description() string {
	return "Declare a phase of the approved plan finished, stating what it achieved. " +
		"Pass the phase's INDEX (0 is the first phase) and a short `outcome`. " +
		"You do NOT have to complete a phase before moving on — leaving one open is fine, " +
		"and it is simply recorded as unfinished. The one exception is a phase another " +
		"phase depends on: that one must be completed before the dependent phase can be " +
		"selected. Completing a phase you have not entered is refused."
}

func (*completePhaseTool) InputSchema() json.RawMessage {
	// An INDEX, for the same reason select_phase takes one: freezing drops the
	// agent's phase ids so authorization cannot key on a name the agent chose.
	//
	// `outcome` is REQUIRED. A completion nobody can read is a worse audit trail
	// than no completion at all, and stating what a phase achieved is the thing
	// that makes the trail self-documenting.
	return json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"index":{"type":"integer","minimum":0},
			"outcome":{"type":"string","minLength":1,"maxLength":500}
		},
		"required":["index","outcome"]
	}`)
}

type completePhaseArgs struct {
	Index   int    `json:"index"`
	Outcome string `json:"outcome"`
}

func (t *completePhaseTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var a completePhaseArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"index":0,"outcome":"listed the open issues"}`); !ok {
		return res, nil
	}

	if t.cfg.ActivePlan == nil {
		return tool.Result{
			Content: "complete_phase: no approved plan is in force for this session",
			IsError: true, Trusted: true,
		}, nil
	}
	plan, ok := t.cfg.ActivePlan(ctx)
	if !ok || len(plan.Phases) == 0 {
		return tool.Result{
			Content: "complete_phase: no approved plan is in force for this session; " +
				"declare phases with update_plan and have them approved first",
			IsError: true, Trusted: true,
		}, nil
	}

	if a.Index < 0 || a.Index >= len(plan.Phases) {
		return tool.Result{
			Content: fmt.Sprintf("complete_phase: index %d is out of range; this plan has %d phases (valid indexes 0..%d)",
				a.Index, len(plan.Phases), len(plan.Phases)-1),
			IsError: true, Trusted: true,
		}, nil
	}

	// A phase never ENTERED cannot be completed, and this is the guard that
	// keeps completion a narrowing rather than a bypass.
	//
	// A requires edge was already satisfied by prior entry, which the runtime
	// records and the agent cannot forge. If declaring completion alone
	// released a dependent phase, an agent could skip the prerequisite
	// entirely — weaker than the rule completion was meant to strengthen. The
	// fold enforces the same thing independently (State.CompletedPhases); this
	// is here so the agent gets a message rather than a silent no-op.
	if t.cfg.Entered != nil && !t.cfg.Entered(ctx)[a.Index] {
		return tool.Result{
			Content: fmt.Sprintf("complete_phase: phase %d has not been entered, so there is nothing "+
				"to complete. Call select_phase %d first if you intend to do its work.", a.Index, a.Index),
			IsError: true, Trusted: true,
		}, nil
	}

	if t.cfg.Record != nil {
		if err := t.cfg.Record(ctx, a.Index, a.Outcome); err != nil {
			// A completion that was not durably recorded did not happen. Saying
			// otherwise would leave the agent believing a dependent phase is
			// unblocked when the fold will not agree.
			return tool.Result{
				Content: "complete_phase: could not record the completion, so it did not take effect: " + err.Error(),
				IsError: true, Trusted: true,
			}, nil
		}
	}

	return tool.Result{
		Content: fmt.Sprintf("Phase %d (%s) marked complete.", a.Index, plan.Phases[a.Index].Label),
		Trusted: true,
	}, nil
}
