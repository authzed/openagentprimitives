package capability

import (
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func init() { Register(&subagentConversationCapability{}) }

// subagentConversationCapability contributes ask_parent: the delegated
// child's half of a conversational delegation.
//
// Default-on, and active only for a session that IS one — a delegated child
// (spec.parent) bound to a channel whose counterparty is another session. It
// is not the `subagents` grant: that one says an agent may delegate, and a
// child is under no obligation to hold it. Nor is it something a child class
// opts into, because the decision was already made one level up: the PARENT's
// roster is what grants a task/chat mode, and the SubagentRequest controller
// is what refuses a wider one. Making the child ask for it again would mean a
// roster could grant a conversation the child then had no way to hold.
//
// Not channel-attached, or not delegated, is a normal inactive state (an
// ordinary session, or a single_turn child, which is headless by
// construction) — so Offer returns (nil, nil) rather than a SkipReason,
// mirroring channel_interaction's own treatment of a session with no channel.
type subagentConversationCapability struct{}

func (subagentConversationCapability) Name() string          { return "subagent_conversation" }
func (subagentConversationCapability) DefaultOn() bool       { return true }
func (subagentConversationCapability) Infrastructural() bool { return false }

// ParseConfig has no capability-specific sub-config, only the common
// {enabled} flag Assemble already parses out of the same raw blob.
func (subagentConversationCapability) ParseConfig(json.RawMessage) (Config, error) {
	return nil, nil
}

func (subagentConversationCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	// Channel-attachment is asked separately from delegation, and of a
	// different thing. IsDelegatedChild below reads spec.InputChannel — the
	// SPEC's answer to "is a pair Channel bound to this session" — while
	// o.Binding is whether this RUNNER was started with the bus wiring behind
	// it (internal/cmd/runner's channelAttachedFromEnv). A session that has the
	// spec but not the wiring has no InboundCh, so ask_parent would park it on
	// a reply nothing can deliver.
	if o.Binding == nil {
		return nil, nil // headless (a single_turn child), or a runner with no bus
	}
	// The delegated-child predicate itself is v1alpha1.IsDelegatedChild — the
	// SAME one the runner uses to decide that agent_work_complete completes
	// such a session instead of parking it Idle. Restating it here is how the
	// two drift: it reads spec.InputChannel, which is the field the pair
	// Channel is bound to, and an inline copy asking o.Binding.Kind agrees with
	// it only for as long as those two are the same channel.
	isChild, err := spiceboxv1alpha1.IsDelegatedChild(o.Session, chregistry.AllowsSessionCounterparty)
	if err != nil {
		// The kind is not registered in this binary, so whether a parent is
		// reachable through the binding cannot be established. Offering
		// ask_parent on a guess would park the child on an answer nobody can
		// send, so it is withheld — and, unlike the ordinary "bound to a human
		// surface" case below, this is a wiring bug and must not be silent.
		//
		// The kind is named by the wrapped registry error itself
		// (`unknown kind: "…"`), so it is not re-derived here — reaching back
		// into the session for a field the predicate already reported on is
		// how the two restate each other again.
		return nil, &SkipReason{
			Capability: "subagent_conversation",
			Reason: fmt.Sprintf("this session's channel kind is not registered in this binary, so whether the agent that delegated here is reachable through it could not be established: %v",
				err),
		}
	}
	if !isChild {
		// Either nobody delegated here, or the session is delegated and bound
		// to a human surface rather than to the agent that delegated. A child
		// whose binding is a Slack thread or a browser tab has no parent on the
		// other end of it, and ask_parent would park it waiting for one forever.
		return nil, nil
	}
	if isAttendedChild(o.Session) {
		// An attended child's counterparty is the HUMAN on the root session's
		// channel, not the parent that delegated it -- offering ask_parent here
		// would hand it a second, uninspected line to the parent's transcript
		// alongside respond_to_user, which is exactly the laundering path
		// respondToUserSkip's SECURITY note (channelinteraction.go) exists to
		// close. Treated the same as the "bound to a human surface" case just
		// above: a normal inactive state for THIS capability, not a
		// granted-but-unavailable skip -- the child still has respond_to_user.
		return nil, nil
	}
	if o.Env.AskParent == nil {
		return nil, &SkipReason{
			Capability: "subagent_conversation",
			Reason:     "no way to record a question on this session's status; the agent that delegated here cannot be asked anything",
		}
	}
	e := o.Env
	return []tool.Tool{meta.NewAskParent(meta.AskParentConfig{
		Record: e.AskParent,
		// The SAME await wiring await_user_message gets: ask_parent parks the
		// session exactly as it does, and the yield/resume hooks behind these
		// are what stop the run clock, disarm the silence watchdog and flush
		// accrued run-time before a pod that may be reaped mid-wait goes quiet.
		Await: meta.AwaitConfig{
			IdleTTL:    e.IdleTTL,
			InboundCh:  e.InboundCh,
			PresenceCh: e.PresenceCh,
			Clock:      e.Clock,
			OnYield:    e.OnAwaitYield,
			OnResume:   e.OnAwaitResume,
		},
	})}, nil
}

// isAttendedChild reports whether sess is a delegated child whose replies are
// routed to a HUMAN on the root session's channel rather than to the parent
// that delegated it — the shape attended mode gives a child once the
// SubagentRequest controller has provisioned it (a later change; see
// buildChild's Mode handling in pkg/controllers/subagentrequest/controller.go).
//
// It answers from spec.OutputChannel's mere PRESENCE rather than from the
// SubagentRequest's own Spec.Mode / EffectiveMode(). Reading the mode here
// would mean a second live lookup (OwningSubagentRequest, an API read) at
// every tool-assembly pass for every delegated child, on top of the one
// askParentBudgetGuard already does per ask_parent call — and, per that
// helper's own doc, mode is meant to be INTERPRETED once, by the controller
// that owns it, not re-derived at each of its consumers. spec.OutputChannel
// is that interpretation already landed on the child: channelinteraction.go's
// respondToUserSkip documents that "no code path gives a delegated child an
// OutputChannel" for task or chat — both share ONE Channel for both
// directions — so a delegated child that HAS one is, today, exactly the
// attended shape and nothing else. If a future mode ever gives a delegated
// child a distinct OutputChannel for an unrelated reason, this predicate (and
// the SkipReason-free withholding it drives) needs a second look.
func isAttendedChild(sess *spiceboxv1alpha1.AgentSession) bool {
	return sess != nil && sess.Spec.OutputChannel != nil
}
