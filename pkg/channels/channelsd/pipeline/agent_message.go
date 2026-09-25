package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// HandleAgentMessageSend services ap.session.<ns>.<name>.in.agent_message_send
// — a message from one AgentSession to another, published by the SENDER on its
// OWN subject and naming its destination in the payload. It is what makes that
// publish arrive as work: without a subscriber, the envelope sits on the bus
// with nothing reading it (see channelevents.KindAgentMessageSend's doc).
//
// It is the ONLY arriving shape a session-to-session message has, and both of
// its producers publish it: a runner's reply_to_subagent (pkg/agent/tool/meta)
// and the `agent` channel kind's Sender (pkg/channels/channelkinds/agent),
// which runs inside channelsd. A runner has no other option — its per-session
// NATS grant enumerates publish rights on its own prefix only
// (runnerNATSUserGrant, pkg/controllers/agentsession/controller.go), and
// widening that to another session's inbound subtree would let any runner
// inject inbound traffic into any session in the cluster. The Sender does have
// another option, holding a cluster-wide credential, and deliberately declines
// it: addressing the destination's subject would leave the SENDER as an
// unauthenticated payload claim, which is the one direction of traffic in this
// system that would not be authenticated by the subject it arrived on.
//
// The `agent` kind has no Listener of its own — channelkinds.Deps grants a
// kind only NATSPublish (one-off) and NATSRequest (request-reply), never a
// subscribe capability or a raw *nats.Conn, so a channel-kind package cannot
// subscribe to anything. This subject is therefore serviced the same way
// every other wildcard inbound row in internal/cmd/channelsd/main.go is:
// centrally, by channelsd, the one component holding both a cluster-wide NATS
// credential and a write-capable memory token. agent.Kind.NewListener stays a
// documented no-op for the same reason.
//
// # The Channel it delivers through is the CONVERSATION, not an inbox
//
// A delegation edge gets exactly one `agent` Channel, created by the
// SubagentRequest controller (pkg/controllers/subagentrequest): it is bound to
// the CHILD as spec.inputChannel and names the PARENT on spec.authzSubject. So
// the one Channel has two ends — a bound end and a counterparty end — and both
// directions of the conversation ride it. resolvePairChannel finds it from the
// (target, sender) pair rather than from the target's own binding, which is
// what makes this work in BOTH directions:
//
//   - parent -> child: the target (the child) is the bound end, and the
//     Channel's authzSubject names the sender.
//   - child -> parent: the SENDER (the child) is the bound end, and the
//     Channel's authzSubject names the target. Reading the target's own
//     binding here would find a human surface — a root parent is bound to
//     slack or the local TUI — which is refused, correctly, by the
//     authzSubject type gate, and is why a conversational child could never
//     answer a root parent while this resolved an inbox instead of a pair.
//
// Delivery then reuses the same Deliver path a real channel listener's inbound
// takes, so the Interact hook and the signed memory append run exactly as they
// do for a human's message. It differs in one respect, and only because it
// must: InboundEvent.TargetSession names the session to deliver into, instead
// of leaving Deliver to find it from the Channel's correlation labels. Those
// labels live on the bound end — the child — so on a parent -> child message
// they resolve to the target and on a child -> parent one to the sender;
// naming the target explicitly is what makes both land in the right place.
//
// # What makes the resolution unforgeable
//
// pl.To is a CLAIM. It is never trusted on its own: resolvePairChannel
// requires a real, K8s-witnessed Channel whose TWO ENDS are exactly the
// claimed destination and the sender — one end named on spec.authzSubject, the
// other end witnessed by that session's own spec.inputChannel binding. A
// session with its own `agent` Channel to somebody else resolves nothing here,
// and a destination the resolved Channel does not name is refused. No Channel
// is ever synthesized, and the pair is never inferred from the payload alone.
//
// The SENDER is not a claim at all, which is the point of this shape: it is
// env.Session, and internal/cmd/channelsd/main.go's envelopeHandler has
// already cross-checked that against the NATS subject the publisher was
// authorized on and dropped any mismatch.
//
// The kind gate still applies to whatever Channel this resolves. Deliver's
// no-user-identity branch accepts an "agentsession:" AuthzSubject only when
// the CHANNEL's own kind (K8s-witnessed, never the payload's claim) allows a
// session counterparty (channelkinds.SessionCounterparty) — so a pair Channel
// of some other kind is refused there, not here, which keeps that gate the
// single choke point it was built to be.
//
// SpiceDB is the authorization, and it is separate from both: Deliver's
// InboundTurn hook checks agentsession#converse from the sender to the TARGET
// session (`converse = parent + child`, one hop each way), so a Channel that
// outlived the lineage it was created for still gets a fresh, fully-consistent
// refusal.
//
// # Attribution
//
// The sending session, never a human: the acting subject is the
// "agentsession:<ns>/<name>" form of the SUBJECT-authorized sender, built
// here, and it is passed as InboundEvent.AuthzSubject with NO ExternalIDs set,
// so Deliver's no-user-identity branch uses it VERBATIM. This is the
// monotonic-identity property the whole delegation design rests on: a child
// must never be able to act as its parent's human, so this handler must never
// populate ExternalIDs from anything derived from a human identity — the
// payload carries none to derive from, and must never grow a field that does.
func (p *Pipeline) HandleAgentMessageSend(ctx context.Context, env channelevents.Envelope) error {
	if env.Kind != channelevents.KindAgentMessageSend {
		return fmt.Errorf("HandleAgentMessageSend: unexpected kind %q", env.Kind)
	}
	sender := client.ObjectKey{Namespace: env.Session.Namespace, Name: env.Session.Name}

	// Verification + anti-replay: a valid session signature over the
	// subject-bound digest, session-window binding, freshness, and a
	// monotonic sigSeq. This is the ONE choke point every agent_message_send
	// passes through — envelopeHandler's wildcard subscription means the
	// bus-level subject check alone would let any session forge another's
	// claimed sender in the payload's `Publisher` field; this closes that.
	if err := p.envVerify.verifyAgentMessage(ctx, env); err != nil {
		if p.NATS != nil {
			mev := channelevents.MonitoringEvent{
				Level:      channelevents.MonitoringLevelWarning,
				Category:   "session",
				Transition: channelevents.MonitoringTransitionFailed,
				Source: channelevents.MonitoringSourceRef{
					Kind: "AgentSession", Namespace: env.Session.Namespace, Name: env.Session.Name,
				},
				Condition: "AgentMessageIntegrity",
				Reason:    "EnvelopeVerificationFailed",
				Summary:   err.Error(),
				Timestamp: p.Now(),
			}
			if perr := channelevents.PublishMonitoring(p.NATS.Publish, mev); perr != nil {
				log.FromContext(ctx).Info("agent_message_send: publish integrity monitoring event failed",
					"session", sessionRefText(sender), "err", perr.Error())
			}
		}
		return fmt.Errorf("agent_message_send: envelope refused: %w", err)
	}

	var pl channelevents.AgentMessageSendPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("agent_message_send: decode payload (sender %s): %w", sessionRefText(sender), err)
	}
	if pl.Text == "" {
		return fmt.Errorf("agent_message_send: empty text (sender %s)", sessionRefText(sender))
	}
	if pl.To.Namespace == "" || pl.To.Name == "" {
		return fmt.Errorf("agent_message_send: no destination (sender %s)", sessionRefText(sender))
	}
	target := client.ObjectKey{Namespace: pl.To.Namespace, Name: pl.To.Name}

	return p.deliverAgentMessage(ctx, target, sender, pl.Text)
}

// deliverAgentMessage carries one message from sender to target: the pair
// Channel that witnesses their conversation, then the ordinary Deliver path.
//
// Split out from the handler so the decode-and-validate step and the delivery
// step read separately; the sender is always the session the arriving
// subject authorized, so the acting subject is BUILT here rather than passed
// in — there is no wire field a publisher could put it in.
func (p *Pipeline) deliverAgentMessage(
	ctx context.Context, target, sender client.ObjectKey, text string,
) error {
	if sender == target {
		// A session messaging itself is a self-feeding loop, not a
		// conversation. The agent kind's Sender refuses the mirror image of
		// this at publish time (a Channel naming its own bound session); this
		// is the arrival-side half, so a publish that bypassed the Sender
		// cannot reintroduce it.
		return fmt.Errorf("agent_message_send: session %s addressed itself", sessionRefText(target))
	}
	from := "agentsession:" + sender.Namespace + "/" + sender.Name

	ch, channelKey, err := p.resolvePairChannel(ctx, target, sender)
	if err != nil {
		return err
	}

	dec, err := p.Deliver(ctx, channelkinds.InboundEvent{
		Channel:       ch,
		ChannelKey:    channelKey,
		TargetSession: &target,
		MessageText:   text,
		AuthzSubject:  from,
		// ExternalIDs deliberately left at its zero value: see the monotonic-
		// identity note above. Do not populate this from the acting subject or
		// from anything else — that would re-introduce a per-user identity for
		// a message that has none.
	})
	if err != nil {
		return fmt.Errorf("agent_message_send: deliver (session %s, from %s): %w",
			sessionRefText(target), from, err)
	}
	if dec.Outcome == channelkinds.OutcomeInternalError {
		return fmt.Errorf("agent_message_send: pipeline returned InternalError (session %s, from %s)",
			sessionRefText(target), from)
	}
	return nil
}

// resolvePairChannel finds the Channel that IS the conversation between target
// and sender, and the channelKey that Channel's bound end is correlated on.
//
// A conversational delegation has exactly one Channel, so there are exactly
// two places it can be witnessed from — whichever end is the bound end — and
// both are checked against the same rule: the Channel's spec.authzSubject must
// name the OTHER end of the pair. Nothing here reads a channel KIND: which
// kinds may carry an "agentsession:" subject at all is decided once, by
// Deliver's authzSubject type gate, against the Channel this returns.
//
// The target's own binding is tried FIRST, and the first match wins — this
// does not keep looking for a "better" Channel. A binding that names the
// sender as its counterparty IS a Channel joining the pair; whether it may
// carry the traffic is the gate's question, not this one, and asking it here
// too would give a mis-bound target a second, silent route instead of one
// loud refusal.
//
// Fail-closed, and loud. When NEITHER end witnesses the pair the error names
// both of them, so a delegation whose Channel was rolled back — or a publish
// claiming a sender it is not — is locatable from the channelsd log rather
// than disappearing. The caller returns the error to envelopeHandler, which
// logs it and moves on; core NATS does not redeliver, so a refusal here costs
// one log line, not a retry loop.
//
// What it deliberately does NOT decide is whether the target session exists.
// A missing target is a benign miss to correlateSessions, which every channel
// kind reaches, and having a second opinion here is what turned a child
// replying to a garbage-collected parent into an ERROR per message. So a
// target that is gone is simply an end that cannot witness the pair: the
// search continues from the sender, and correlateSessions makes the one call.
func (p *Pipeline) resolvePairChannel(
	ctx context.Context, target, sender client.ObjectKey,
) (*spiceboxv1alpha1.Channel, string, error) {
	// The target is the bound end: parent -> child, the direction the
	// SubagentRequest controller provisions directly.
	if ch, key, err := p.pairChannelFrom(ctx, target, sender); err != nil || ch != nil {
		return ch, key, err
	}

	// The SENDER is the bound end: child -> parent. The target may hold no
	// `agent` binding of its own at all here — a root parent's binding is its
	// human surface, and a kubectl-driven parent has none — which is exactly
	// why this arm cannot be expressed as a variation of the one above.
	if ch, key, err := p.pairChannelFrom(ctx, sender, target); err != nil || ch != nil {
		return ch, key, err
	}

	return nil, "", fmt.Errorf(
		"agent_message_send: no channel joins session %s and claimed sender %s: "+
			"neither end is bound to a channel whose authzSubject names the other",
		sessionRefText(target), sessionRefText(sender))
}

// pairChannelFrom looks for the pair Channel from ONE end of the conversation:
// `end` must exist, hold a spec.inputChannel binding, and that binding's
// Channel must name `other` on spec.authzSubject. It returns that Channel and
// the channelKey `end` is correlated on.
//
// A clean MISS returns (nil, "", nil) so the caller can try the other end.
// Four states are misses, and all four are states the OTHER end may still
// witness the pair from:
//
//   - `end` does not exist. Whether that matters is correlateSessions's
//     question, not this one — see resolvePairChannel's doc.
//   - `end` holds no spec.inputChannel at all (a kubectl-driven parent).
//   - its binding names a Channel that no longer exists. A broken binding, and
//     the one that has to keep going: a human Slack Channel deleted while
//     sessions still reference it would otherwise make the target unreachable
//     by its own child even though the pair Channel — bound to the child, and
//     perfectly healthy — is right there. Logged at INFO, because nothing
//     else reports a dangling binding.
//   - its Channel names somebody else. These two are simply not a pair here.
//
// A lookup that FAILED is returned as an error instead, and the distinction is
// the point: a transient apiserver fault reported as "no channel joins these
// two" would give a retryable failure the shape of a definitive refusal.
func (p *Pipeline) pairChannelFrom(
	ctx context.Context, end, other client.ObjectKey,
) (*spiceboxv1alpha1.Channel, string, error) {
	logger := log.FromContext(ctx)

	var sess spiceboxv1alpha1.AgentSession
	switch err := p.K8s.Get(ctx, end, &sess); {
	case err == nil:
	case apierrors.IsNotFound(err):
		logger.V(1).Info("agent_message_send: conversation end does not exist; it cannot witness the pair",
			"end", sessionRefText(end), "otherEnd", sessionRefText(other))
		return nil, "", nil
	default:
		return nil, "", fmt.Errorf(
			"agent_message_send: get session %s while resolving its conversation with %s: %w",
			sessionRefText(end), sessionRefText(other), err)
	}

	b := sess.Spec.InputChannel
	if b == nil {
		return nil, "", nil
	}

	var ch spiceboxv1alpha1.Channel
	switch err := p.K8s.Get(ctx, client.ObjectKey{Namespace: end.Namespace, Name: b.Name}, &ch); {
	case err == nil:
	case apierrors.IsNotFound(err):
		logger.Info("agent_message_send: a conversation end's spec.inputChannel names a channel that does not exist; trying the other end",
			"end", sessionRefText(end), "otherEnd", sessionRefText(other),
			"channel", end.Namespace+"/"+b.Name)
		return nil, "", nil
	default:
		return nil, "", fmt.Errorf(
			"agent_message_send: get channel %s/%s bound to %s while resolving its conversation with %s: %w",
			end.Namespace, b.Name, sessionRefText(end), sessionRefText(other), err)
	}

	if !channelNames(&ch, other) {
		return nil, "", nil
	}
	return &ch, b.Key, nil
}

// channelNames reports whether ch's counterparty — spec.authzSubject — is the
// given session. Compared as PARSED references rather than as raw strings, so
// a subject of some other type (a "service:" identity on a channel that also
// happens to sit on this edge) is a clean non-match instead of a string
// comparison that only accidentally fails.
func channelNames(ch *spiceboxv1alpha1.Channel, sess client.ObjectKey) bool {
	ref, err := agentSessionRef(ch.Spec.AuthzSubject)
	return err == nil && ref == sess
}

// agentSessionRef parses an "agentsession:<namespace>/<name>" SpiceDB subject
// — the value Channel.spec.authzSubject carries to name the counterparty end
// of a conversation — into the client.ObjectKey this package addresses
// sessions by.
//
// The parsing itself is authz.ParseAgentSessionSubject's, shared with the
// authz inbound gate and the agent kind's Sender; this is only the shape
// conversion from authz.SessionRef to the key the K8s client wants.
func agentSessionRef(subject string) (client.ObjectKey, error) {
	ref, err := authz.ParseAgentSessionSubject(subject)
	if err != nil {
		return client.ObjectKey{}, err
	}
	return client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, nil
}

// sessionRefText renders a session key as "<namespace>/<name>" for error text.
func sessionRefText(k client.ObjectKey) string { return k.Namespace + "/" + k.Name }
