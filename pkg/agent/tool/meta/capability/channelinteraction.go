package capability

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func init() { Register(&channelInteractionCapability{}) }

// channelInteractionCapability is default-on but only active when the
// session is channel-attached (o.Binding != nil). It contributes the four
// meta tools every channel-attached session needs to talk to the user:
// respond_to_user, await_user_message, update_status, and set_thread_title.
// Not channel-attached is a normal inactive state (kubectl-driven sessions),
// not a granted-but-unavailable skip, so Offer returns (nil, nil) rather
// than a SkipReason when Binding is nil.
//
// respond_to_user is WITHHELD from a session EITHER of whose bindings reaches
// another session rather than a person — see respondToUserSkip.
type channelInteractionCapability struct{}

func (channelInteractionCapability) Name() string                                { return "channel_interaction" }
func (channelInteractionCapability) DefaultOn() bool                             { return true }
func (channelInteractionCapability) Infrastructural() bool                       { return false }
func (channelInteractionCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (channelInteractionCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Binding == nil {
		return nil, nil // not channel-attached → no channel tools, not a skip
	}
	e := o.Env
	tools := []tool.Tool{
		meta.NewAwait(meta.AwaitConfig{
			IdleTTL:    e.IdleTTL,
			InboundCh:  e.InboundCh,
			PresenceCh: e.PresenceCh,
			Clock:      e.Clock,
			OnYield:    e.OnAwaitYield,
			OnResume:   e.OnAwaitResume,
		}),
		meta.NewUpdateStatus(meta.UpdateStatusConfig{
			NATSPublish:       e.NATSPublish,
			NATSSubjectPrefix: e.SubjectPrefix,
			EnvelopeSigner:    e.EnvelopeSigner,
		}),
		meta.NewSetThreadTitle(meta.SetThreadTitleConfig{
			NATSPublish:       e.NATSPublish,
			NATSSubjectPrefix: e.SubjectPrefix,
			EnvelopeSigner:    e.EnvelopeSigner,
		}),
	}
	if skip := respondToUserSkip(o.Binding.Kind, outboundKindOf(o), terminalToolNameFor(o.Session)); skip != nil {
		return tools, skip
	}
	// respond_to_user first, as it always has been: Assemble preserves the
	// order a capability returns, and the tool list order is what the model
	// reads.
	return append([]tool.Tool{
		meta.New(meta.RespondConfig{
			// Both from the OUTBOUND binding: what a reply may carry, and which
			// dialect it is written in, are facts about the transport that will
			// RENDER it. On a split-channel session (a webhook in, a chat
			// transport out) the input binding answers for a channel no reply is
			// ever shown on — github advertises nothing, so asking it strips
			// `attached` out of the schema and tells the agent "plain text only"
			// while its reader is on Slack.
			Capabilities:      o.OutboundBinding().Capabilities,
			ChannelKind:       o.OutboundBinding().Kind,
			NATSPublish:       e.NATSPublish,
			NATSSubjectPrefix: e.SubjectPrefix,
			EnvelopeSigner:    e.EnvelopeSigner,
			AppendSystemNote:  e.AppendSystemNote,
			Client:            e.Client,
			Artifacts:         e.Artifacts,
			// LeakageGate is wrapped so the meta tool always has a non-nil
			// func value (matching internal/cmd/runner's late-bound closure pattern);
			// a nil Env.LeakageGate is treated as "no gate" rather than
			// leaving RespondConfig.LeakageGate nil.
			LeakageGate: func(ctx context.Context, sess *tool.SessionContext, text string, attachments []channelevents.AttachmentRef) error {
				if e.LeakageGate == nil {
					return nil
				}
				return e.LeakageGate(ctx, sess, text, attachments)
			},
		}),
	}, tools...), nil
}

// outboundKindOf is the channel kind respond_to_user's envelope is actually
// ROUTED by: the outbound relay addresses every non-human-directed envelope
// through v1alpha1.OutboundBinding (pkg/channels/channelsd/outbound's relay),
// which prefers spec.OutputChannel and falls back to spec.InputChannel.
//
// o.Binding is the INPUT binding (internal/cmd/runner passes
// sess.Spec.InputChannel), so on a split-channel session the tool is built from
// one binding and delivered through another. Asking the input kind alone
// coincides with the routed kind for every session that has no OutputChannel —
// which is every session in tree today, and exactly why asking only it read as
// correct.
//
// Falls back to the input kind when the session is unavailable: OutboundBinding
// returns nil only for a session with NO channel attachment, and Offer has
// already established there is one. That is not a fail-open — the input kind is
// still asked below, and where there is no OutputChannel it IS the routed kind.
func outboundKindOf(o OfferContext) string {
	if ob := spiceboxv1alpha1.OutboundBinding(o.Session); ob != nil {
		return ob.Kind
	}
	return o.Binding.Kind
}

// respondToUserSkip decides whether respond_to_user is withheld from a session
// whose input binding is of kind inputKind and whose outbound binding is of
// kind outboundKind, and returns the reason when it is.
//
// terminalTool is the name coreCapability wired for THIS session
// (terminalToolNameFor) — return_result for a delegated child, which is every
// session that reaches this skip in tree today. It is passed in rather than
// spelled here because the reason text tells an operator what the session still
// has, and naming a door it was never given is worse than naming none.
//
// SECURITY. A session whose binding reaches another SESSION has two ways to put
// text into that session's context, and only one of them is inspected:
//
//   - ask_parent carries the child's words to the delegating agent as an
//     untrusted TOOL RESULT, which runs through Loop.inspectMetaToolCall
//     (pkg/agent/runner/loop_metainspect.go). Routing the question through the
//     delegation is what buys that inspection.
//   - respond_to_user goes out through the relay, through the `agent` kind's
//     Sender, onto the counterparty's inbound subject, and lands in its
//     transcript as an inbound message. Content inspection runs at
//     PreToolCall/PostToolCall only (contentguard.InspectionPoints), and an
//     inbound message is neither — so it is inspected NOWHERE.
//
// Leaving both wired lets the same child launder arbitrary text past the
// inspection by choosing the other tool, which defeats the reason its question
// goes through the delegation at all.
//
// BOTH bindings are asked — by chregistry.FirstSessionCounterparty, which owns
// the walk — and either one reaching a session withholds the tool. The one that
// decides delivery is the OUTBOUND binding — that is what
// the relay routes by — so asking only the input binding would offer
// respond_to_user to a session with inputChannel.kind=slack and
// outputChannel.kind=agent, and send its reply straight down the uninspected
// path. The input binding is still asked because it is the binding the tool is
// CONSTRUCTED from, and because fail-closed is the only direction that cannot
// re-open the laundering path: the one shape it withholds for no gain is the
// mirror image (input reaches a session, output reaches a person), which
// nothing in tree produces either — no code path gives a delegated child an
// OutputChannel.
//
// Withholding it costs the session NO reach. respond_to_user emits
// KindUserMessage, which is not in the relay's human-directed set
// (pkg/channels/channelsd/outbound/humandirected.go), so for such a session it
// was never routed to a person: it went through the default arm to the
// session's own outbound binding — the counterparty agent — which is precisely
// the uninspected path. What still reaches a human is a human-directed prompt,
// and the relay resolves those up the lineage regardless of this. ask_parent and
// the session's own terminal tool remain, so it can still ask and still answer.
//
// The question is asked of the KIND, never of a kind NAME: a second
// session-to-session transport must inherit this with no edit here (AGENTS.md:
// registry, not branching).
//
// Fail-CLOSED on an unregistered kind. Every kind is blank-imported by the
// runner (guarded by internal/cmd/runner's channelkinds_registered_test), so an
// unresolvable kind is a wiring bug, and the direction that treats it as "not a
// session counterparty" is the direction that re-opens the laundering path.
func respondToUserSkip(inputKind, outboundKind, terminalTool string) *SkipReason {
	// The derivation itself lives in the registry
	// (chregistry.FirstSessionCounterparty) rather than here, because the
	// `artifact-delivered` completion requirement keys on the same fact: it
	// demands a respond_to_user call, so it must be answerable only by a
	// session that was offered one. Two readings of "does this session talk to
	// a person" would drift, and the drift would show up as a session refused
	// completion for not making a call it was never given.
	b, reaches, err := chregistry.FirstSessionCounterparty(inputKind, outboundKind)
	if err != nil {
		return &SkipReason{
			Capability: "channel_interaction",
			Reason: fmt.Sprintf("respond_to_user withheld: this session's %s channel kind %q is not registered in this binary, "+
				"so whether its counterparty is another agent could not be established (%v); ask_parent and %s remain",
				b.Role, b.Kind, err, terminalTool),
		}
	}
	if reaches {
		return &SkipReason{
			Capability: "channel_interaction",
			Reason: fmt.Sprintf("respond_to_user withheld: this session's %s channel kind %q reaches another agent, not a person, "+
				"and a message arriving that way lands in the other agent's transcript with no content inspection anywhere. "+
				"It cost this session no reach — respond_to_user was never routed to a human here — and ask_parent (inspected as "+
				"an untrusted tool result) and %s remain", b.Role, b.Kind, terminalTool),
		}
	}
	return nil
}
