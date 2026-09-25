package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func init() { Register(&planningCapability{}) }

// planningCapability is default-on (active for every AgentClass unless
// explicitly disabled) and contributes update_plan, plus select_phase when the
// plan gate is on.
type planningCapability struct{}

func (planningCapability) Name() string                                { return "planning" }
func (planningCapability) DefaultOn() bool                             { return true }
func (planningCapability) Infrastructural() bool                       { return false }
func (planningCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (planningCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	// A delegated child requests authority; it never authors a plan. Withholding
	// update_plan (and select_phase / complete_phase with it) is what makes "a
	// child cannot widen its own authority" structural rather than policy — the
	// same move already made for respond_to_user (withheld) and agent_work_complete
	// (replaced by return_result). A child that needs authority its inherited phase
	// does not cover takes the request path (request_approval / request_plan_amendment),
	// which routes to the parent and stays attributable to the child.
	//
	// Keyed on Spec.Parent, the one discriminator the SubagentRequest controller
	// sets and core.go already uses for request_input and the terminal-tool swap.
	if o.Session != nil && o.Session.Spec.Parent != nil {
		return nil, &SkipReason{
			Capability: "planning",
			Reason:     "delegated child: plan-authoring tools are withheld; a child requests authority from its parent, it does not author a plan",
		}
	}

	// NATSPublish is nil for kubectl-driven sessions; NewUpdatePlan tolerates nil.
	out := []tool.Tool{meta.NewUpdatePlan(meta.UpdatePlanConfig{
		NATSPublish:      o.Env.NATSPublish,
		EnvelopeSigner:   o.Env.EnvelopeSigner,
		OnPhasesDeclared: o.Env.FreezePhases,
	})}

	// select_phase is offered ONLY when the plan gate is on. With the gate off
	// there is no frozen plan to index into, so the tool could do nothing but
	// return an error — and an always-failing tool in the catalog is worse than
	// an absent one: it spends the model's attention and invites retries.
	if o.Env.PlanGateActive {
		out = append(out, meta.NewSelectPhase(meta.SelectPhaseConfig{
			ActivePlan:      o.Env.ActivePlan,
			Record:          o.Env.RecordPhaseSelection,
			PhaseCompletion: o.Env.PhaseCompletion,
			PhaseEntries:    o.Env.PhaseEntries,
		}))
		// complete_phase rides the same gate-on condition as select_phase: with
		// no frozen plan there is no phase to complete.
		out = append(out, meta.NewCompletePhase(meta.CompletePhaseConfig{
			ActivePlan: o.Env.ActivePlan,
			Record:     o.Env.RecordPhaseCompletion,
			Entered:    o.Env.PhaseEntered,
		}))
	}
	return out, nil
}
