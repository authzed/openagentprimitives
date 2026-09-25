package pipeline

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// refuseUninvitedInitiative is the INITIATIVE half of what
// SubagentRequestSpec.Mode declares: a delegated child may be allowed to ASK
// its parent something without being allowed to be DRIVEN by anyone.
//
// A `task` child's whole declaration is that untrusted content moves it exactly
// one bounded step: it asks a question and continues. Admitting a turn a PERSON
// opened would hand it a conversation the delegation never granted — the same
// widening reconcileParentExchange refuses on the exchange side. A `chat` child
// declares that conversation up front and may be addressed freely.
//
// It returns (decision, refused, err). refused=true means Deliver must return
// the decision as-is and go no further; err is a read failure the caller
// surfaces as an internal error so the message is retried rather than
// half-delivered.
//
// Three things it deliberately does NOT do:
//
//   - It does not introduce a second notion of "is this a human". nonHumanSubject
//     is Deliver's own, computed once from whether the inbound carried a per-user
//     external identity, and a non-human acting subject returns here before any
//     lookup happens. That is what admits the PARENT's reply: it arrives as an
//     "agentsession:<ns>/<name>" acting subject with no external identity, so it
//     is not a person opening a turn and is never measured against this gate.
//     Without that property the resumable loop would break on the first reply.
//   - It does not ask whether the sender MAY interact. That is the SpiceDB check
//     immediately after, and it answers a different question: this one says
//     nobody may drive this child, however much standing they hold.
//   - It does not branch on a channel kind. The mode is the declaration, and the
//     mode lives on the delegation.
//
// Fail-closed. A session that says it was delegated but whose delegation cannot
// be read is refused, loudly: the terms that would permit the turn are exactly
// what could not be established.
func (p *Pipeline) refuseUninvitedInitiative(
	ctx context.Context,
	active *spiceboxv1alpha1.AgentSession,
	ev channelkinds.InboundEvent,
	nonHumanSubject bool,
) (channelkinds.InboundDecision, bool, error) {
	if nonHumanSubject || active.Spec.Parent == nil {
		// Not a person, or not a delegated child at all. Either way there is no
		// initiative to gate, and no read to pay for one.
		return channelkinds.InboundDecision{}, false, nil
	}

	sr, err := spiceboxv1alpha1.OwningSubagentRequest(ctx, p.K8s, active)
	if err != nil {
		log.FromContext(ctx).Info("pipeline: refusing inbound — the delegation terms of this child session could not be read, so whether a person may open a turn into it is unknown",
			"session", active.Namespace+"/"+active.Name,
			"channel", channelRefForLog(ev.Channel),
			"err", err.Error())
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, true,
			fmt.Errorf("initiative gate: %w", err)
	}
	if sr == nil || sr.PermitsHumanInitiative() {
		return channelkinds.InboundDecision{}, false, nil
	}

	log.FromContext(ctx).Info("pipeline: refusing inbound — a person cannot open a turn into a delegated child in this mode",
		"session", active.Namespace+"/"+active.Name,
		"request", sr.Namespace+"/"+sr.Name,
		"mode", sr.EffectiveMode(),
		"channel", channelRefForLog(ev.Channel))
	logAttachmentsDropped(ctx, active.Namespace, active.Name, channelRefForLog(ev.Channel),
		"a refused turn into a delegated agent", ev.Attachments)

	return channelkinds.InboundDecision{
		Outcome: channelkinds.OutcomeDeniedByPermission,
		Notice:  subagentNotAddressableNotice(active, sr),
	}, true, nil
}

// subagentNotAddressableNotice tells the person why their message went nowhere.
// Silence here would be the worst outcome available: they are looking at a live
// agent that is visibly working, and nothing else would ever explain why it
// ignored them.
//
// The copy names the delegating agent, because that is the whole remedy — the
// reader has somewhere to go, which is why this reads as a redirect rather than
// as a failure.
func subagentNotAddressableNotice(active *spiceboxv1alpha1.AgentSession, sr *spiceboxv1alpha1.SubagentRequest) *notice.Notice {
	return notice.New(categories.SubagentNotAddressable, notice.Args{
		Lead: "This agent takes direction only from the agent that delegated to it",
		Body: fmt.Sprintf("%s was handed a bounded piece of work by %s and answers to it alone, so your message was not delivered. It is still working; nothing has gone wrong.",
			active.Name, sr.Spec.Parent.Name),
		NextStep: fmt.Sprintf("Send this to %s instead, and let it decide what to pass on.", sr.Spec.Parent.Name),
		// Participants, not requester: this path has no approval flow and
		// nobody to address individually, and it is the same audience every
		// other in-thread refusal on this surface uses.
		Audience: participantsAudience(),
	})
}
