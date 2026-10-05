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
	g, err := d.Service.Store.Get(ctx, o.Domain, o.GoalID)
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
	if _, ok := kind.(channelkinds.DeliveryReceiverProvider); !ok {
		return o, fmt.Errorf("%w: transport has no durable delivery receiver", domain.ErrDenied)
	}
	pin := g.Execution.Terms.Destination
	intent := delivery.Intent{Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name}, Destination: delivery.Destination{Kind: binding.Kind, ChannelUID: pin.ChannelUID, BindingDigest: pin.BindingDigest, Recipient: o.Domain.Owner}, Payload: payload, CreatedAt: d.now()}
	return store.PrepareReply(ctx, o, domain.RunReply{Intent: intent, Sources: sources}, d.now())
}

func (d *Dispatcher) processReply(ctx context.Context, o domain.Occurrence) (domain.Occurrence, error) {
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:operator"), "system:operator")
	if o.Reply == nil || o.Reply.State == "accepted" || o.Reply.State == "absent" {
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
			if o.Reply.State == "attempted" {
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
	if o.Reply.State == "prepared" {
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
	accepted, err := receiver.Accept(ctx, o.Reply.Intent)
	if err != nil {
		return o, err
	} // Keep attempted/unknown; never fabricate a receipt.
	return d.concludeReply(ctx, store, o, &accepted)
}

func (d *Dispatcher) concludeReply(ctx context.Context, store domain.ReplyStore, o domain.Occurrence, receipt *delivery.Receipt) (domain.Occurrence, error) {
	updated, err := store.ConcludeReply(ctx, o, receipt, d.now())
	if err != nil {
		return o, err
	}
	return updated, nil
}
