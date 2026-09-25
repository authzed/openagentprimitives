// pkg/channels/channelkinds/agent/sender.go
//
// The agent kind's Sender. Every other kind's Sender calls a third-party API
// (Slack's chat.postMessage, Bento's pipeline); this one delivers by
// publishing a KindAgentMessageSend envelope onto the SENDING session's own
// inbound bus subject, naming the counterparty in the payload — a cluster-side
// NATS publish, not a network call out.
//
// # Why the sender's subject, when this Sender could publish on either
//
// This Sender runs inside channelsd, which holds a cluster-wide NATS
// credential, so it is the one publisher that COULD address the counterparty's
// subject directly. It publishes on the sender's instead, because that is the
// subject an authorization can be read off: channelsd's envelopeHandler
// cross-checks Envelope.Session against the subject the publisher was
// authorized on (internal/cmd/channelsd/main.go's inboundSubjectAuthorized),
// so a message published here is authenticated as coming FROM this session by
// the subject itself, and the destination is the claim — corroborated by
// pipeline.resolvePairChannel against a real Channel joining the two.
//
// Addressing the counterparty's subject would invert that: the destination
// would be authorized and the SENDER would be the claim, carried as a payload
// field with nothing but this Sender's own good behaviour behind it. Every
// other direction in the system authenticates its sender by the subject; this
// one now does too, and there is one arriving kind rather than two.
//
// The sending session's own subject is also the only one a RUNNER could
// publish on (its per-session NATS grant covers its own prefix and nothing
// else), which is why reply_to_subagent already publishes exactly this kind —
// so both producers of a session-to-session message now converge on one
// envelope, one subscription, and one handler.
//
// Delivery mechanism, and why: Deps offers two candidates for "publish onto
// the bus" — Deps.Inbound (the shared InboundPipeline a Listener drives) and
// Deps.NATSPublish (a one-off publish that bypasses the pipeline). This
// Sender uses NATSPublish. Two independent reasons converge on that choice:
//
//  1. In production, Deps.Inbound is unconditionally nil for every Sender.
//     internal/cmd/channelsd/sender_resolver.go's depsSnapshot sets
//     `Inbound: nil` with the comment "Senders do not call into the inbound
//     pipeline" — so calling deps.Inbound.Deliver from here would nil-panic
//     on the real binary regardless of any reasoning about the design. This
//     is the decisive fact, not a matter of taste.
//  2. Even setting that aside, using Inbound here would create outbound→
//     inbound re-entrancy: channelsd's outbound relay subscribes once to
//     the whole cluster's out.> tree (pkg/channels/channelsd/outbound.
//     Relay.Start) and calls Sender.Send from that single subscription's
//     callback goroutine. If Send called into the SAME Pipeline instance's
//     Deliver synchronously, that would be a nested call on the same
//     goroutine, not a deadlock by itself — Deliver holds no lock spanning
//     the call and never calls back into a Sender (verified: Pipeline has
//     no Senders field and no Sender.Send call in
//     pkg/channels/channelsd/pipeline) — but it also does real K8s/Memory
//     I/O, which would make the relay's one dispatch goroutine, cluster-
//     wide, block on it. NATSPublish is fire-and-forget — the actual
//     production wiring (internal/cmd/channelsd/sender_resolver.go's
//     depsSnapshot: `NATSPublish: func(subj string, p []byte) error {
//     return nc.Publish(subj, p) }`) is a plain nats.Conn.Publish, not a
//     request-reply round trip — so this Sender's own delivery step cannot
//     block on, or be blocked by, anything downstream of the publish.
//
// What NATSPublish does NOT do here: unlike the pipeline's own Deliver (used
// by every listener-driven kind), a raw NATSPublish does not write a durable
// memory turn or trigger session correlation, dedup or attribution — those
// stay owned by the inbound pipeline, and this Sender does not reimplement
// them. Its job ends at "the envelope is on the bus, published as this
// session, naming the counterparty"; turning that into a delivered,
// attributed turn happens centrally, not in this package. channelsd's
// subscription table (internal/cmd/channelsd/main.go's "agent_message_send"
// row) is the consumer of the subject this Sender publishes on, and calls
// pkg/channels/channelsd/pipeline.Pipeline.HandleAgentMessageSend, which
// resolves the Channel joining the (target, sender) pair — this same Channel,
// reached from whichever end is bound to it — to reach Deliver.
// agent.Kind.NewListener is a permanent no-op (see its own doc and
// the package README): channelkinds.Deps grants a kind only NATSPublish and
// NATSRequest, never a subscribe capability, so no Listener in this package
// will ever be the one that drains this subject.
package agent

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// agentSender implements channelkinds.Sender by publishing a
// KindAgentMessageSend envelope onto the SENDING session's inbound bus
// subject, naming the counterparty in the payload.
//
// The counterparty is resolved ONCE, at construction, from
// Deps.Channel.Spec.AuthzSubject: a Channel of this kind is dedicated to one
// counterparty for its whole lifetime (Kind.DefaultSessionScope is
// "singleton"), and Deps.Channel is already a construction-time snapshot for
// every field — the same trade-off channelsd's ScopeRefresher docs describe
// for Listeners. A malformed authzSubject is captured as resolveErr so every
// Send call refuses immediately, before touching the publish function —
// which is what makes "a malformed counterparty publishes nothing" true on
// every call, not just the first.
type agentSender struct {
	publish func(subject string, payload []byte) error

	channelRef string // "<ns>/<name>" of the Channel this Sender was built for; only for error text

	counterpartyNS, counterpartyName string
	resolveErr                       error
}

// newAgentSender builds the Sender for one Channel. deps.Channel and
// deps.Channel.Spec.AuthzSubject are validated here, once, rather than on
// every Send.
func newAgentSender(deps channelkinds.Deps) channelkinds.Sender {
	s := &agentSender{publish: deps.NATSPublish}
	if deps.Channel == nil {
		s.resolveErr = fmt.Errorf("agent sender: no Channel bound (Deps.Channel is nil)")
		return s
	}
	s.channelRef = deps.Channel.Namespace + "/" + deps.Channel.Name
	// authz.ParseAgentSessionSubject, not a local parser: this value crosses
	// three packages and had three parsers, the laxest of which — this one —
	// checked only the prefix and the '/'. What kept an arbitrary id out of it
	// was the CRD Pattern in channel_types.go happening to agree with authz's
	// subjectRE, which is a correctness guarantee resting on two files staying
	// in step. Sharing the parser makes the charset check the parser's own.
	ref, err := authz.ParseAgentSessionSubject(deps.Channel.Spec.AuthzSubject)
	if err != nil {
		s.resolveErr = fmt.Errorf("agent sender: channel %s: authzSubject: %w", s.channelRef, err)
		return s
	}
	s.counterpartyNS, s.counterpartyName = ref.Namespace, ref.Name
	return s
}

// progressKindsWithNoAgentSurface is the set of envelope kinds the outbound
// relay routes to the MAIN sender (relay.go's `default:` dispatch arm — none
// of these five have their own sub-channel `case`) that render mid-turn
// status onto a human-facing surface: a Slack "is thinking…" indicator, a
// plan overlay, a tool-progress line. An agent counterparty has none of
// that — there is no status line, spinner, or plan overlay on the other end
// of a session-to-session channel — so Send drops them deliberately (nil
// error, nothing published) rather than falling through to "unsupported
// envelope kind". Every one of these fires at least once per turn on a
// channel-attached session; erroring on them would mean a fixed set of INFO
// lines on every single turn, which is exactly the "a log line that always
// fires and never means anything" failure mode the relay's own
// KindUIActionUpdate carve-out (relay.go) documents and avoids.
var progressKindsWithNoAgentSurface = map[channelevents.Kind]bool{
	channelevents.KindNotification:      true,
	channelevents.KindTurnProgress:      true,
	channelevents.KindToolProgress:      true,
	channelevents.KindOperationActivity: true,
	channelevents.KindPlanUpdate:        true,
}

// Send refuses every envelope kind. progressKindsWithNoAgentSurface are
// dropped silently (see its doc); everything else is an error.
//
// THERE IS NO FREE-FORM SESSION-TO-SESSION MESSAGE. This Sender used to accept
// channelevents.KindUserMessage — respond_to_user's envelope — and publish it
// onward as a KindAgentMessageSend. Nothing produced one: a session bound to a
// kind that allows a session counterparty is not offered respond_to_user at
// all, because such a message lands in the other agent's transcript with no
// content inspection anywhere, so the runner withholds it
// (pkg/agent/tool/meta/capability's respondToUserSkip, fail-closed — an
// unregistered kind withholds too).
//
// A conversational delegation's three live directions each go somewhere else,
// and each is TYPED and separately authorized: the child's question through
// ask_parent, its answer through return_result, the parent's reply through
// reply_to_subagent. All three reach the other side as inspected tool results.
// A free-form message is precisely the untyped channel that design replaced.
//
// The arm was removed rather than left dormant because unreachable code that
// would REACTIVATE if a withholding rule changed is a latent re-opening: relax
// respondToUserSkip later and this would go live carrying a path nobody
// re-reviewed, with nothing in that diff to say so. If the need returns it
// should arrive as a deliberately designed direction, reviewed on its own
// terms. Git history keeps the code.
//
// CONSEQUENCE WORTH KNOWING WHEN READING LOGS. The outbound relay's
// delivery-failure fallback builds a KindUserMessage notice and sends it
// through this same Sender (surfaceDeliveryFailure, relay.go). So a
// KindUserMessage that somehow reaches an agent-bound session produces TWO
// errors: the refusal here, then "delivery-failure notice also failed to send;
// channel unreachable". That is correct, not a bug — there is no person on this
// channel for a failure notice to reach, the counterparty is another agent —
// and it does not recurse. The relay's gate itself is deliberately left alone:
// it is generic, and slack/browser depend on it.
//
// This is the default sub-channel's Sender only — the
// interaction family (KindInteractionRequest/Applied/DecisionRejected) is
// dispatched by the relay to the "interaction" SUB-CHANNEL sender instead
// (relay.go), never reaching Send here at all; Kind.SubChannelSender returns
// nil for every name today, so the relay's own nil-sender degrade drops those,
// not this method.
func (s *agentSender) Send(
	_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope,
) (channelkinds.SubChannelSendResult, error) {
	if s.resolveErr != nil {
		return channelkinds.SubChannelSendResult{}, s.resolveErr
	}
	if s.counterpartyNS == sess.Namespace && s.counterpartyName == sess.Name {
		// A Channel naming its own sending session as counterparty is
		// nonsensical for every envelope, not just a real message: refuse
		// before even looking at env.Kind. This is a real self-feeding loop
		// today, not a hypothetical one: pipeline.HandleAgentMessageSend
		// already delivers whatever this Sender publishes, so an unguarded
		// publish here would feed the sending session's own inbound right
		// back to itself the moment a Channel is bound naming its own
		// session. The arrival side refuses the same shape independently, so
		// a publish that never went through this Sender cannot reintroduce it.
		return channelkinds.SubChannelSendResult{}, fmt.Errorf(
			"agent sender: channel %s: counterparty agentsession:%s/%s names the sending session itself",
			s.channelRef, s.counterpartyNS, s.counterpartyName)
	}

	if progressKindsWithNoAgentSurface[env.Kind] {
		return channelkinds.SubChannelSendResult{}, nil
	}
	return channelkinds.SubChannelSendResult{}, fmt.Errorf(
		"agent sender: channel %s: unsupported envelope kind %q; a session-to-session "+
			"conversation carries only its typed directions (ask_parent, return_result, "+
			"reply_to_subagent), never a free-form message", s.channelRef, env.Kind)
}
