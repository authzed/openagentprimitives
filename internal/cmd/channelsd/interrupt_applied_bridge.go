// interruptAppliedBridge subscribes to ap.session.*.*.out.interrupt_applied and,
// for SLACK-input sessions ONLY, republishes the runner's KindInterruptApplied
// as an interaction_applied(queued_messages) on the OUT subject so the generic
// interaction sender resolves the queued_messages card in place.
//
// This is the async other half of the queued_messages "Interrupt & Send Now"
// flow (pkg/channels/channelsd/pipeline/queued_interrupt.go): the click's bound handler
// returns Suppressed:true (so the generic decision pipe does NOT publish a
// synchronous interaction_applied), fires a KindInterruptRequest at the runner,
// and the runner answers on .out.interrupt_applied once the interrupt lands.
// This bridge turns that answer back into the generic Applied envelope.
//
// THE CRITICAL INVARIANT — the double-fire guard: only Slack-input sessions
// resolve the card through this bridge. builtin / local / webchat sessions
// consume KindInterruptApplied DIRECTLY on their own queued_messages sub-channel
// sender (they never migrated onto the interaction model — see
// pkg/channels/channelkinds/{builtin,local}/sender_queued_messages.go), so re-publishing
// an interaction_applied for them would double-render. The guard mirrors the
// EXACT gate the producer uses (pipeline.go: InputChannel.Kind == "slack").
//
// Mirrors monitoringRelay / outbound.Relay: a fixed subject family, a plain
// Subscribe, one handler per message.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// interruptAppliedBridgeSubject is the OUT subject family the bridge watches —
// ONLY interrupt_applied (the outbound relay uses out.> for everything; this
// wants just this one kind). Cluster-wide over the two session wildcards, which
// is why handle re-derives (ns, name) from each message's own subject rather
// than trusting the envelope.
var interruptAppliedBridgeSubject = channelevents.SubjectOut(
	channelevents.AnySessionPrefix(), channelevents.KindInterruptApplied)

// interruptOutcomeInterrupted is the InterruptAppliedPayload.Outcome value the
// runner stamps when the interrupt actually landed (as opposed to "rejected").
// Kept local — the runner writes it as a bare literal (internal/cmd/runner/nats.go); the
// bridge only needs to distinguish "interrupted" from everything-else.
const interruptOutcomeInterrupted = "interrupted"

// interruptAppliedBridge republishes runner interrupt outcomes as generic
// interaction_applied(queued_messages) for Slack-input sessions.
type interruptAppliedBridge struct {
	cli client.Client
	nc  *nats.Conn
	// publish is the NATS publish closure. Wired by the constructor to
	// nc.Publish; tests inject a recorder. (Same injectable-publish shape as
	// sessionWatcher.publish.)
	publish channelevents.PublishFunc

	mu  sync.Mutex
	sub *nats.Subscription
}

func newInterruptAppliedBridge(cli client.Client, nc *nats.Conn) *interruptAppliedBridge {
	b := &interruptAppliedBridge{cli: cli, nc: nc}
	if nc != nil {
		b.publish = nc.Publish
	}
	return b
}

// Start subscribes to the interrupt_applied subject family. Safe to call once.
func (b *interruptAppliedBridge) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sub != nil {
		return nil
	}
	sub, err := b.nc.Subscribe(interruptAppliedBridgeSubject, func(m *nats.Msg) {
		b.handle(ctx, m)
	})
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", interruptAppliedBridgeSubject, err)
	}
	b.sub = sub
	return nil
}

// Stop drains the subscription and clears b.sub so a subsequent Start
// re-subscribes. Safe to call multiple times.
func (b *interruptAppliedBridge) Stop(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sub == nil {
		return nil
	}
	err := b.sub.Drain()
	b.sub = nil
	return err
}

// handle decodes one interrupt_applied envelope and, for a Slack-input session,
// republishes it as interaction_applied(queued_messages) on the OUT subject.
func (b *interruptAppliedBridge) handle(ctx context.Context, m *nats.Msg) {
	logger := log.FromContext(ctx).WithName("interrupt-applied-bridge")

	var env channelevents.Envelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		logger.Info("drop, malformed envelope", "err", err.Error())
		return
	}

	// The SUBJECT is the routing authority, not the envelope. This bridge holds
	// its OWN cluster-wide subscription, separate from the outbound relay's, so
	// it needs the relay's rule (see pkg/channels/channelsd/outbound/relay.go's handle)
	// for itself: a runner's per-session JWT permits publishing under exactly
	// one "ap.session.<ns>.<own-name>.>" tree, so the subject is the only
	// session identity NATS authorized and env.Session is publisher-controlled
	// JSON. Routing off the envelope would let a publisher authorized on
	// session A's out subject name session B and have this bridge render an
	// interruption card — carrying its own Reason text — into B's Slack thread.
	ns, name, subjectOK := channelevents.ParseOutSubject(m.Subject)
	if !subjectOK {
		logger.Info("drop, unparseable out subject", "subject", m.Subject,
			"claimedSession", env.Session.Namespace+"/"+env.Session.Name)
		return
	}
	if ns != env.Session.Namespace || name != env.Session.Name {
		// Logged with the publisher-locating trio (subject, claimed session,
		// kind) per AGENTS.md's no-silent-errors rule.
		logger.Info("drop, envelope session does not match the authorized subject",
			"subject", m.Subject, "subjectSession", ns+"/"+name,
			"claimedSession", env.Session.Namespace+"/"+env.Session.Name, "kind", string(env.Kind))
		return
	}

	var ip channelevents.InterruptAppliedPayload
	if err := json.Unmarshal(env.Payload, &ip); err != nil {
		logger.Info("drop, malformed interrupt_applied payload", "session", ns+"/"+name, "err", err.Error())
		return
	}

	var sess spiceboxv1alpha1.AgentSession
	if err := b.cli.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		logger.Info("drop, AgentSession lookup failed", "session", ns+"/"+name, "err", err.Error())
		return
	}

	// THE double-fire guard (see file doc comment): non-Slack sessions consume
	// KindInterruptApplied directly — republishing would double-render.
	if sess.Spec.InputChannel == nil || sess.Spec.InputChannel.Kind != "slack" {
		return
	}

	outcome, text := channelevents.OutcomeDenied, couldntInterruptText(ip.Reason)
	if ip.Outcome == interruptOutcomeInterrupted {
		// An interrupt drains the whole held queue, not just the one message this
		// ack was minted for — hence "message(s)". Reproduces the deleted Slack
		// sender's interruptAppliedText.
		outcome, text = channelevents.OutcomeResolved, "Interrupting — sending your queued message(s) now."
	}

	applied := channelevents.InteractionAppliedPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        categories.QueuedMessages,
		RequestRef:      ip.RequestID,
		Outcome:         outcome,
		OutcomeText:     text,
		Reason:          ip.Reason,
		ResponseRef:     ip.ResponseURL,
	}
	// OUT only: the runner already applied the interrupt via loop.Interrupt (this
	// message IS its answer), so there is no runner-side resume to unblock — do
	// NOT PublishIn.
	if err := channelevents.PublishOut(b.publish, ns, name, channelevents.KindInteractionApplied, applied); err != nil {
		logger.Info("publish interaction_applied failed",
			"session", ns+"/"+name, "requestID", ip.RequestID, "err", err.Error())
	}
}

// couldntInterruptText renders the rejected-outcome text, reproducing the
// deleted Slack queued_messages sender's interruptAppliedText: a bare reason,
// falling back to a sensible default when the runner supplied none.
func couldntInterruptText(reason string) string {
	if reason == "" {
		reason = "the runner declined."
	}
	return "Couldn't interrupt — " + reason
}
