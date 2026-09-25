package capability

import (
	"encoding/json"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func init() { Register(&subagentsCapability{}) }

// subagentsCapability contributes the delegating side of a delegation:
// delegate, which opens one, and reply_to_subagent, which answers a child
// that came back with a question. The child's own side (ask_parent) is a
// different capability, because a delegated child is not required to hold
// this grant — nothing about being delegated TO implies being allowed to
// delegate.
//
// Opt-in (DefaultOn == false) on purpose, the same posture as
// credential_update: delegation creates platform objects (a SubagentRequest,
// and if approved a whole child AgentSession) and spends the session tree's
// own budget, so an AgentClass has to ask for it rather than inherit it. The
// determination that actually guards a delegation -- roster membership,
// graph validity, identity monotonicity, budget -- lives operator-side in
// pkg/controllers/subagentrequest; this gate is the coarser "should this
// agent be able to ask at all".
type subagentsCapability struct{}

func (subagentsCapability) Name() string          { return "subagents" }
func (subagentsCapability) DefaultOn() bool       { return false }
func (subagentsCapability) Infrastructural() bool { return false }

// ParseConfig has no capability-specific sub-config, only the common
// {enabled} flag the caller (Assemble, via agentcaps.GrantOf) already parses
// out of the same raw blob before this ever runs -- see knowledge.go /
// planning.go for the identical no-op shape.
func (subagentsCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (subagentsCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Env.SubagentCreate == nil || o.Env.SubagentPoll == nil {
		return nil, &SkipReason{Capability: "subagents", Reason: "no subagent request client wired into the runner"}
	}
	// A roster of zero means this class was never given anyone to delegate
	// to -- offering delegate here would be a tool that can only ever be
	// denied. Refusing it up front matches request_credential_update's own
	// "would have nothing to do" skip reasoning.
	if len(o.Class.Spec.RosterNames()) == 0 {
		return nil, &SkipReason{Capability: "subagents", Reason: "spec.subagents declares no agents to delegate to"}
	}
	cfg := meta.DelegateConfig{
		Namespace:      o.Session.Namespace,
		SessionName:    o.Session.Name,
		Create:         o.Env.SubagentCreate,
		Poll:           o.Env.SubagentPoll,
		Send:           o.Env.SubagentSend,
		ResolveDataTag: o.Env.SubagentResolveDataTag,
		PollInterval:   2 * time.Second,
		Timeout:        o.Env.SubagentTimeout,
	}
	// Both tools, from the one config. reply_to_subagent additionally needs
	// Env.SubagentSend, which is absent on a session with no bus (a
	// kubectl-driven parent); it is offered anyway and refuses that call
	// explicitly, the same posture delegate itself takes toward a nil Create.
	//
	// Offered rather than withheld-with-a-SkipReason on purpose. Assemble does
	// append the tools that come back alongside a skip, so withholding one of
	// several IS expressible now (channel_interaction does exactly that with
	// respond_to_user) — but a SkipReason only reaches the runner's log, while
	// a refusal at call time reaches the MODEL, in the turn it asked, with the
	// reason attached. For a parent that will never have a bus that is the
	// better of the two.
	return []tool.Tool{
		meta.NewDelegateTool(cfg),
		meta.NewSubagentReplyTool(cfg),
		// send_input is delegate's mid-flight counterpart: `inputs` hands data
		// over AT the handoff, this one answers a child that asked for more
		// afterwards. Offered on the same terms as reply_to_subagent — a
		// parent with no way to bind refuses the call by name rather than
		// leaving the model to infer a missing tool.
		meta.NewSendInputTool(meta.SendInputConfig{
			Bind:           o.Env.SubagentBindDataSlot,
			ResolveDataTag: o.Env.SubagentResolveDataTag,
		}),
	}, nil
}
