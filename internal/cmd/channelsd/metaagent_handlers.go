// channelsd subscriptions for metaagent_scope_approval and metaagent_notice.
// These are published by authzd (internal/cmd/authzd) and the operator as raw JSON
// payloads rather than channelevents Envelopes — see the wire-format note in
// pkg/channels/channelevents/metaagent.go — so they are handled by direct subscriptions
// rather than through the outbound relay, which skips them
// (channelevents.Kind.RelayHandles).
//
// Subjects consumed:
//   - ap.session.<ns>.<name>.out.metaagent_scope_approval  (authzd → channelsd)
//   - ap.session.<ns>.<name>.out.metaagent_notice          (authzd, operator → channelsd)
//
// Both handlers are transport-agnostic: they locate the session, resolve its
// Channel's kind through the registry, and hand the payload to that kind's
// sub-channel Sender. The rendering — Block Kit, mrkdwn, which External keys
// carry the routing, how a canonical subject becomes an addressable user —
// belongs to the kind (pkg/channels/channelkinds/slack/metaagent_sender.go for Slack).
package main

import (
	"context"
	"time"

	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// metaagentHandlers provides direct NATS subscriptions for metaagent flow
// envelopes that bypass the channelevents Envelope wrapper.
type metaagentHandlers struct {
	cli client.Client
	nc  *nats.Conn
	// slackKind is never read: the metaagent scope-approval Sender records its
	// own MetaagentApprovalRef beside the post it made, so nothing here needs a
	// concrete kind. It exists only so main.go's wiring block compiles — delete
	// the two together.
	slackKind *slack.Kind
}

// handleScopeApproval processes one ap.session.*.*.out.metaagent_scope_approval
// message.
func (h *metaagentHandlers) handleScopeApproval(ctx context.Context, msg *nats.Msg) {
	h.dispatch(ctx, msg, channelevents.KindMetaagentScopeApproval,
		"a human approval prompt; the runner is blocked on the decision and will halt when the approval window expires")
}

// handleNotice processes one ap.session.*.*.out.metaagent_notice message.
func (h *metaagentHandlers) handleNotice(ctx context.Context, msg *nats.Msg) {
	h.dispatch(ctx, msg, channelevents.KindMetaagentNotice,
		"an acknowledgment the user is waiting on")
}

// dispatch is the shared path for both metaagent subjects: decode the message
// against the kind this handler owns, locate the session the SUBJECT
// authorized, resolve its kind, hand the payload to that kind's sub-channel
// Sender.
//
// The sub-channel name is the kind's own string — SubChannelMetaagentNotice IS
// "metaagent_notice" — so the subject decides which surface renders, and a
// notice arriving on the scope-approval handler is refused rather than rendered
// as the wrong thing. metaagent_subchannel_test.go pins the two names equal.
//
// waitingOn describes, in user terms, what is lost when nothing renders. It is
// logged on every drop: these two surfaces are the ones a human is actively
// waiting for, and the original failure mode here — a non-Slack session's
// approval silently discarded while the runner blocked the whole approval
// window — was invisible precisely because the log said nothing about cost.
func (h *metaagentHandlers) dispatch(ctx context.Context, msg *nats.Msg, kind channelevents.Kind, waitingOn string) {
	subChannel := string(kind)
	logger := ctrllog.FromContext(ctx).WithValues("subject", msg.Subject, "subChannel", subChannel)

	// One decode covers four checks: the subject grammar (an "out" segment and a
	// non-empty ns/name), the leaf matching THIS handler's kind, payload
	// well-formedness, and — for an envelope-shaped body — that its session
	// claim agrees with the subject NATS authorized. Publishers send a bare
	// payload today; DecodeMetaagentOut accepts both shapes so the cross-check
	// is already in place should any of them start sending an envelope.
	dec, err := channelevents.DecodeMetaagentOut(msg.Subject, msg.Data, kind)
	if err != nil {
		logger.Info("metaagent: message refused before dispatch; dropping",
			"err", err.Error(), "waitingOn", waitingOn)
		return
	}
	ns, name := dec.Namespace, dec.Name
	logger = logger.WithValues("session", dec.SessionRef())

	var sess spiceboxv1alpha1.AgentSession
	if err := h.cli.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		logger.Info("metaagent: session lookup failed; dropping",
			"err", err.Error(), "waitingOn", waitingOn)
		return
	}
	if sess.Spec.InputChannel == nil {
		logger.Info("metaagent: session is not channel-attached; dropping", "waitingOn", waitingOn)
		return
	}

	ch, sec, k, err := resolve.ForSession(ctx, h.cli, &sess)
	if err != nil {
		logger.Info("metaagent: resolve channel/secret failed; dropping",
			"err", err.Error(), "waitingOn", waitingOn)
		return
	}

	// Deps are built here rather than borrowed from the senderResolver because
	// metaagentHandlers is constructed before that resolver exists, and because
	// these two surfaces must NOT be cached per Channel: they are posted rarely
	// and a rotated bot token should take effect on the next one.
	nc := h.nc
	sender := k.SubChannelSender(subChannel, channelkinds.Deps{
		Channel:     ch,
		Secret:      sec,
		K8sClient:   h.cli,
		NATSPublish: func(subj string, p []byte) error { return nc.Publish(subj, p) },
	})
	if sender == nil {
		// The kind's documented "I do not implement this sub-channel" answer.
		// It is a real gap, not a formality: nobody will see this prompt, so
		// say so loudly enough that an operator can find it without reading
		// source. The runner-side fix — gating cold-start scope approval on
		// whether the bound kind can render it — is not channelsd's to make.
		logger.Info("metaagent: bound channel kind has no sender for this sub-channel; nothing will be shown to the user",
			"kind", k.Name(), "waitingOn", waitingOn)
		return
	}

	// Prefer OutputChannel for routing (cron-spawned sessions post into the
	// output thread); fall back to the input binding. Which binding is the
	// right one is generic; which KEYS inside it locate a thread is the kind's.
	binding := sess.Spec.OutputChannel
	if binding == nil {
		binding = sess.Spec.InputChannel
	}
	info := channelkinds.SessionInfo{
		Namespace:   ns,
		Name:        name,
		Channel:     binding,
		Annotations: sess.Annotations,
	}
	env := channelevents.Envelope{
		Version:     1,
		Kind:        kind,
		Session:     channelevents.SessionRef{Namespace: ns, Name: name},
		PublishedAt: time.Now().UTC(),
		Payload:     dec.Payload,
	}
	if _, err := sender.Send(ctx, info, env); err != nil {
		logger.Info("metaagent: send failed; nothing was shown to the user",
			"kind", k.Name(), "err", err.Error(), "waitingOn", waitingOn)
		return
	}
}
