package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The two terminal meta tools. A session is offered exactly one of them; which
// one is terminalToolNameFor's answer.
const (
	agentWorkCompleteToolName = "agent_work_complete"
	returnResultToolName      = "return_result"
)

func init() { Register(&coreCapability{}) }

// coreCapability is the infrastructural, always-on capability contributing the
// terminal meta tools every session needs regardless of AgentClass grants: this
// session's terminal tool (agent_work_complete or return_result) and
// new_operation.
type coreCapability struct{}

func (coreCapability) Name() string                                { return "core" }
func (coreCapability) DefaultOn() bool                             { return true }
func (coreCapability) Infrastructural() bool                       { return true }
func (coreCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (coreCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	// meta.Load() registers agent_work_complete + new_operation; the terminal
	// tool is swapped for this session's own below.
	tools := append([]tool.Tool{}, meta.Load()...)

	// This class's completion gate, carried by whichever terminal tool this
	// session ends up with. Built unconditionally, unlike the
	// channel-attached-only swap it replaces: a completion requirement is the
	// operator's guarantee about what a session produces, and a kubectl-driven
	// session of the same class makes the same promise. With no requirements
	// declared the constructed tool is byte-identical in behaviour to the
	// registered default.
	cfg := meta.CompletionConfig{
		Requirements: o.Env.CompletionRequirements,
		RecordBypass: o.Env.RecordCompletionBypass,
	}

	// Replaced, not supplemented, so there is exactly one way to finish and no
	// chance of picking the one written for somebody else's session.
	//
	// Both are constructed from the SAME cfg, so replacing the door does not
	// replace the gate: a child of a class that promised something still has to
	// deliver it or say why.
	terminal := terminalToolFor(o.Session, cfg)
	for i, t := range tools {
		if t.Name() == agentWorkCompleteToolName {
			tools[i] = terminal
			break
		}
	}

	// request_input goes to any DELEGATED child, conversational or not.
	//
	// Deliberately not gated on a reachable parent channel the way ask_parent
	// is: this records a plan amendment rather than parking on an answer, so a
	// single_turn child — which has no channel at all — can still ask for data
	// it was not given. It simply continues without waiting, which is what the
	// tool tells it.
	if o.Session != nil && o.Session.Spec.Parent != nil && o.Env.RequestInput != nil {
		// The SAME await wiring ask_parent and await_user_message get: this
		// parks the session exactly as they do, and the yield/resume hooks
		// behind these are what stop the run clock, disarm the silence
		// watchdog and flush accrued run-time before a pod that may be reaped
		// mid-wait goes quiet.
		tools = append(tools, meta.NewRequestInput(meta.RequestInputConfig{
			Record: o.Env.RequestInput,
			Await: meta.AwaitConfig{
				IdleTTL:    o.Env.IdleTTL,
				InboundCh:  o.Env.InboundCh,
				PresenceCh: o.Env.PresenceCh,
				Clock:      o.Env.Clock,
				OnYield:    o.Env.OnAwaitYield,
				OnResume:   o.Env.OnAwaitResume,
			},
		}))
	}
	return tools, nil
}

// terminalToolNameFor names the tool sess finishes through. It is the ONE home
// of the swap rule: coreCapability.Offer builds the tool from it, and
// channelinteraction's respond_to_user skip names it in text an operator reads,
// so a message telling the model what it still has can never name a door this
// session was not given.
//
// A DELEGATED CHILD gets return_result INSTEAD of agent_work_complete, because
// they are different acts. agent_work_complete ends a round for a session with
// a human reader: that person already saw what respond_to_user posted, and its
// `summary` is an audit note nobody reads. A child has no human and no
// respond_to_user (withheld — it would reach the parent's transcript
// uninspected), and the one string it returns IS the deliverable, copied
// verbatim by reconcileChild into SubagentRequest.Status.Result.
//
// Keyed on spec.parent ALONE, not on a conversational binding: a single_turn
// child returns through the same field and predates conversational modes
// entirely.
//
// A nil session answers agent_work_complete, which is what Offer wires for one
// — the two must agree, and agreement is the property this function exists to
// give.
func terminalToolNameFor(sess *spiceboxv1alpha1.AgentSession) string {
	if sess != nil && sess.Spec.Parent != nil {
		return returnResultToolName
	}
	return agentWorkCompleteToolName
}

// terminalToolFor builds the tool terminalToolNameFor names, carrying cfg —
// this class's completion gate — whichever one it is.
func terminalToolFor(sess *spiceboxv1alpha1.AgentSession, cfg meta.CompletionConfig) tool.Tool {
	if terminalToolNameFor(sess) == returnResultToolName {
		return meta.NewReturnResult(cfg)
	}
	return meta.NewAgentWorkComplete(cfg)
}
