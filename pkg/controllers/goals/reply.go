package goals

import (
	"context"
	"errors"
	"fmt"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/memory"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (d *Dispatcher) PrepareGoalReply(ctx context.Context, sess *v1.AgentSession, payload channelevents.OutboundUserMessagePayload, sources []domain.Source) (domain.Occurrence, error) {
	if err := d.ValidateGoalSession(ctx, sess); err != nil {
		return domain.Occurrence{}, err
	}
	store, ok := d.Store.(domain.ReplyStore)
	if !ok || d.DeliveryMemory == nil {
		return domain.Occurrence{}, domain.ErrDenied
	}
	o, err := d.Store.Occurrence(ctx, sess.Spec.GoalExecution.OccurrenceID)
	if err != nil {
		return o, err
	}
	g, err := d.occurrenceGoal(ctx, o)
	if err != nil {
		return o, err
	}
	permitted := false
	for _, op := range g.Execution.Terms.AllowedOperations {
		if op == "respond_to_user" {
			permitted = true
		}
	}
	if !permitted {
		return o, domain.ErrDenied
	}
	binding := v1.OutboundBinding(sess)
	kind, ok := registry.Get(binding.Kind)
	if !ok {
		return o, domain.ErrDenied
	}
	_, confirmable := kind.(channelkinds.DeliveryReceiverProvider)
	if !confirmable && !g.Domain.GroupOwned() {
		return o, fmt.Errorf("%w: transport has no durable delivery receiver", domain.ErrDenied)
	}
	var dest delivery.Destination
	if g.Domain.GroupOwned() {
		dest, err = d.groupReplyDestination(ctx, g, binding)
		if err != nil {
			return o, err
		}
	} else {
		pin := g.Execution.Terms.Destination
		dest = delivery.Destination{Kind: binding.Kind, ChannelUID: pin.ChannelUID, BindingDigest: pin.BindingDigest, Recipient: o.Domain.Owner}
	}
	intent := delivery.Intent{Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name}, Destination: dest, Payload: payload, CreatedAt: d.now()}
	reply := domain.RunReply{Intent: intent, Sources: sources}
	if !confirmable {
		// A group run on a kind without a receiver (until that kind's receiver
		// lands) replies through the ordinary outbound relay, which posts the
		// runner's one envelope. The record goes straight to its terminal
		// unconfirmed state: authorized and handed off, never a receipt, and
		// never reconciled or retried. The run's information-leak gate has
		// already checked the reply against the conversation's audience.
		return store.HandOffReply(ctx, o, reply, d.now())
	}
	return store.PrepareReply(ctx, o, reply, d.now())
}

// RecordGoalReplyPublishFailed records that the runner could not publish a
// handed-off group reply: the relay never received it, so nothing was posted.
// The reply moves from unconfirmed to publish_failed under the same run
// fences as the hand-off. The failure is logged here, where the goal and
// occurrence are known, as well as by the runner.
func (d *Dispatcher) RecordGoalReplyPublishFailed(ctx context.Context, sess *v1.AgentSession, payload channelevents.OutboundUserMessagePayload) (domain.Occurrence, error) {
	if err := d.ValidateGoalSession(ctx, sess); err != nil {
		return domain.Occurrence{}, err
	}
	store, ok := d.Store.(domain.ReplyStore)
	if !ok || payload.Delivery == nil {
		return domain.Occurrence{}, domain.ErrDenied
	}
	o, err := d.Store.Occurrence(ctx, sess.Spec.GoalExecution.OccurrenceID)
	if err != nil {
		return o, err
	}
	log.FromContext(ctx).Info("goal reply handed off but not published; nothing was posted",
		"session", sess.Namespace+"/"+sess.Name, "operation", payload.Delivery.ID, "goal", o.GoalID, "occurrence", o.ID)
	return store.FailReplyPublish(ctx, o, payload.Delivery.ID, d.now())
}

// groupReplyDestination is a group reply's audience: the whole group, on the
// Channel the terms pinned. The run's binding must still be on that Channel
// (same name and live UID) and address exactly the goal's member group, so a
// reply can only ever reach the group that owns the goal.
func (d *Dispatcher) groupReplyDestination(ctx context.Context, g domain.Goal, binding *v1.ChannelBinding) (delivery.Destination, error) {
	pin := g.Execution.Terms.Group
	if pin == nil || pin.MemberGroup != g.Domain.Owner || binding.Name != pin.Channel {
		return delivery.Destination{}, fmt.Errorf("%w: group reply is not addressed to the goal's group", domain.ErrDenied)
	}
	ch, host, err := d.groupChannel(ctx, g.Domain.Namespace, pin)
	if err != nil {
		return delivery.Destination{}, err
	}
	if binding.Kind != ch.Spec.Kind {
		return delivery.Destination{}, fmt.Errorf("%w: group reply is not on the pinned Channel's kind", domain.ErrDenied)
	}
	if member, ok := host.ConversationMemberRef(binding); !ok || member != pin.MemberGroup {
		return delivery.Destination{}, fmt.Errorf("%w: group reply is not addressed to the goal's group", domain.ErrDenied)
	}
	route, err := pin.RouteDigest()
	if err != nil {
		return delivery.Destination{}, err
	}
	return delivery.Destination{Kind: binding.Kind, ChannelUID: pin.ChannelUID, BindingDigest: route, Recipient: pin.MemberGroup}, nil
}

func (d *Dispatcher) processReply(ctx context.Context, o domain.Occurrence) (domain.Occurrence, error) {
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:operator"), "system:operator")
	if o.Reply == nil || o.Reply.Concluded() {
		return o, nil
	}
	store, ok := d.Store.(domain.ReplyStore)
	if !ok || d.DeliveryMemory == nil {
		return o, fmt.Errorf("durable delivery unavailable")
	}
	kind, ok := registry.Get(o.Reply.Intent.Destination.Kind)
	if !ok {
		return o, domain.ErrDenied
	}
	provider, ok := kind.(channelkinds.DeliveryReceiverProvider)
	if !ok {
		return o, domain.ErrDenied
	}
	receiver := provider.NewDeliveryReceiver(d.DeliveryMemory)
	if receiver == nil {
		return o, fmt.Errorf("durable delivery receiver unavailable")
	}
	// Reconciliation only reads the existing operation, so it is valid after
	// cancellation or expiration. A new acceptance always rechecks authority.
	receipt, err := receiver.Lookup(ctx, o.Reply.Intent)
	if errors.Is(err, delivery.ErrClosed) {
		return d.concludeReply(ctx, store, o, nil)
	}
	if err != nil {
		return o, err
	}
	if receipt != nil {
		return d.concludeReply(ctx, store, o, receipt)
	}
	var sess v1.AgentSession
	err = d.Reader.Get(ctx, client.ObjectKey{Namespace: o.Domain.Namespace, Name: o.SessionName}, &sess)
	if err == nil {
		err = d.ValidateGoalSession(ctx, &sess)
	}
	if err != nil {
		if errors.Is(err, domain.ErrDenied) || apierrors.IsNotFound(err) {
			if o.Reply.State == domain.ReplyAttempted {
				closer, ok := receiver.(delivery.Closer)
				if !ok {
					return o, fmt.Errorf("delivery outcome unknown; transport cannot close in-flight attempts")
				}
				receipt, closeErr := closer.Close(ctx, o.Reply.Intent)
				if closeErr != nil {
					return o, closeErr
				}
				return d.concludeReply(ctx, store, o, receipt)
			}
			return o, nil // Prepared only: no send has ever been attempted.
		}
		return o, err
	}
	// The reply carries what its author read. The owner must still be able to
	// read every one of those sources at send time, not only at prepare time.
	// Checked before AttemptReply, so a revoked source leaves the reply
	// prepared (never attempted) rather than an in-flight send to reconcile.
	if err := d.checkReplySources(ctx, o); err != nil {
		return o, err
	}
	if o.Reply.State == domain.ReplyPrepared {
		attempted, attemptErr := store.AttemptReply(ctx, o, d.now())
		err = attemptErr
		if err != nil {
			return o, err
		}
		o = attempted
	} else if !receiver.RetryAbsent() {
		return o, fmt.Errorf("delivery outcome unknown; transport cannot safely retry")
	}
	if err := d.ValidateGoalSession(ctx, &sess); err != nil {
		return o, err
	}
	if err := d.checkReplySources(ctx, o); err != nil {
		return o, err
	}
	accepted, err := receiver.Accept(ctx, o.Reply.Intent)
	if err != nil {
		return o, err
	} // Keep attempted/unknown; never fabricate a receipt.
	return d.concludeReply(ctx, store, o, &accepted)
}

func (d *Dispatcher) checkReplySources(ctx context.Context, o domain.Occurrence) error {
	// A group goal has no owning user to read its sources as. Its run posts
	// only through the information-leakage gate, which checks what the run
	// read (seeded with the goal's own sources at activation) against the
	// conversation's whole audience before every post.
	if o.Domain.GroupOwned() {
		return nil
	}
	if d.Service.Auth == nil {
		return domain.ErrDenied
	}
	return d.Service.Auth.ReadGoal(ctx, domain.Actor{Domain: o.Domain}, domain.Goal{Sources: o.Reply.Sources})
}

func (d *Dispatcher) concludeReply(ctx context.Context, store domain.ReplyStore, o domain.Occurrence, receipt *delivery.Receipt) (domain.Occurrence, error) {
	updated, err := store.ConcludeReply(ctx, o, receipt, d.now())
	if err != nil {
		return o, err
	}
	return updated, nil
}
