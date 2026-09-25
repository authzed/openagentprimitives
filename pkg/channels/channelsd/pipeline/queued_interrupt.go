// pkg/channels/channelsd/pipeline/queued_interrupt.go
//
// The bound decision handler for the queued_messages interaction category — the
// mid-turn "Interrupt & Send Now" click on the enqueue-ack prompt. channelsd
// publishes interaction_request(queued_messages) when a message arrives mid-turn,
// the kind's generic decision-click handler publishes interaction_decision, and
// this applies it.
//
// This is the ASYNC leg. The other decision handlers resolve synchronously and
// return an Outcome the generic pipe publishes as interaction_applied, but an
// interrupt's true outcome is known only once the runner interrupts its
// in-flight turn. So this handler:
//
//  1. Republishes the click as a KindInterruptRequest on the runner's IN
//     subject, which the runner's .in.interrupt_request subscriber turns into
//     loop.Interrupt.
//  2. Returns Outcome{Suppressed:true} so the generic pipe SKIPS the synchronous
//     interaction_applied. The runner publishes KindInterruptApplied on .out
//     when the interrupt lands, and the bridge republishes THAT as
//     interaction_applied(queued_messages) to resolve the card.
//
// Standing is NOT re-checked here: HandleInteractionDecision already ran the
// category's DeciderPolicy check (queued_messages = DecideParticipant → any user
// with interact standing, fail-closed via CheckInteract) before invoking this.
package pipeline

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// BindQueuedInterruptHandler wires decideQueuedInterrupt into the interaction
// registry as the bound decision handler for categories.QueuedMessages. Called
// once at channelsd process start (internal/cmd/channelsd/main.go), after the pipeline
// pl is constructed — mirrors the provider_error_retry Bind site.
func BindQueuedInterruptHandler(p *Pipeline) {
	channelinteractions.Bind(categories.QueuedMessages, p.decideQueuedInterrupt)
}

// decideQueuedInterrupt republishes a resolved queued_messages decision as a
// KindInterruptRequest on the runner's IN subject and returns a suppressed
// Outcome. See the file doc comment for why this leg is async.
func (p *Pipeline) decideQueuedInterrupt(_ context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	ns, name := d.Session.Namespace, d.Session.Name
	pl := channelevents.InterruptRequestPayload{
		RequestID:   d.Payload.RequestRef, // correlation id minted at enqueue-ack (pipeline.go)
		SessionRef:  ns + "/" + name,
		Requester:   d.Payload.Decider,     // the clicker
		ResponseURL: d.Payload.ResponseRef, // response_url from the click, round-trips to the applied edit
	}
	if err := channelevents.PublishIn(p.NATS.Publish, ns, name, channelevents.KindInterruptRequest, pl); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("queued_messages decision: publish interrupt_request (session %s/%s): %w", ns, name, err)
	}
	return channelinteractions.Outcome{Suppressed: true}, nil
}
