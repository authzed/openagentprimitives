// internal/cmd/channelsd/webhook_inbound.go — the channelsd side of the
// webd -> channelsd verified-webhook-delivery handoff (see
// pkg/channels/channelevents/webhook.go).
package main

import (
	"context"
	"encoding/json"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// webhookInboundHandler resolves the Channel CR named by a verified webhook
// delivery and dispatches it through the shared inbound pipeline — the same
// entry point the bento sink (bentoInboundSink, above) uses for its
// cron-driven inbounds.
//
// There is no caller to return an error to (this runs off a NATS message,
// not a request/reply), so every failure path logs at INFO+ with the
// channel namespace/name/key and returns; nothing here is silent.
//
// Deliberately no requeue or retry: a verified delivery that cannot be
// dispatched is logged and dropped. GitHub's own webhook retry is the
// recovery mechanism for a transient failure, and webd already returns a
// 503 (which GitHub does retry) for the one case worth retrying — see
// Task 6/8. Retrying again here would double up on that.
func webhookInboundHandler(ctx context.Context, cli client.Client, pl channelkinds.InboundPipeline,
	logger logr.Logger, m *nats.Msg,
) {
	var p channelevents.WebhookInboundPayload
	if err := json.Unmarshal(m.Data, &p); err != nil {
		logger.Info("webhook inbound: undecodable payload", "err", err.Error())
		return
	}
	var ch spiceboxv1alpha1.Channel
	if err := cli.Get(ctx, client.ObjectKey{Namespace: p.ChannelNamespace, Name: p.ChannelName}, &ch); err != nil {
		logger.Info("webhook inbound: Channel CR lookup failed",
			"ns", p.ChannelNamespace, "name", p.ChannelName, "err", err.Error())
		return
	}
	// WHO this delivery acts as comes from the Channel CR, never from the
	// payload. The payload arrives over NATS, and holding a bus grant is not
	// the same as being webd — a publisher could otherwise spawn a session as
	// ANY subject, choosing its owner, its approver set, and who may read its
	// transcript.
	//
	// Nothing is lost by taking it from the CR: the subject was never
	// per-delivery data. github's receiver reads it straight off
	// ch.Spec.AuthzSubject, and this handler already fetches that CR to
	// dispatch at all. The payload field stays on the wire as corroboration,
	// and a disagreement is a forged publish or a version skew — neither of
	// which should start a session, so it refuses rather than silently
	// preferring the CR.
	//
	// A Channel declaring NO subject is refused too when a payload names one:
	// an unattributed Channel would otherwise be the easiest one to spawn
	// sessions through, since there is nothing for a claim to contradict.
	if p.AuthzSubject != ch.Spec.AuthzSubject {
		logger.Info("webhook inbound: delivery names a subject this Channel does not declare; refusing",
			"ns", p.ChannelNamespace, "name", p.ChannelName, "key", p.ChannelKey,
			"authzSubject", p.AuthzSubject, "channelAuthzSubject", ch.Spec.AuthzSubject)
		return
	}
	// The trigger's external owner — the PR author, for github — derived HERE,
	// at the writer, from the same verified RawDelivery bytes the pipeline
	// derives trigger facts from; no security-relevant derived value travels
	// between components as data. It rides PreTurnAnnotations so session
	// creation stamps it atomically, and the operator's owner resolver writes
	// the additional agentsession#owner tuple from the annotation. Best-effort:
	// the owner grant is an addition to the session, never a gate on it, so a
	// derivation failure logs and the delivery dispatches without one.
	var preTurn map[string]string
	if k, ok := chregistry.Get(ch.Spec.Kind); ok {
		if op, ok := k.WebhookReceiver(channelkinds.Deps{}).(channelkinds.TriggerOwnerProvider); ok {
			subject, has, oerr := op.TriggerOwnerSubject(&ch, p.DeliveryEvent, p.RawDelivery)
			switch {
			case oerr != nil:
				logger.Info("webhook inbound: trigger owner derivation failed; session starts without a trigger-owner grant",
					"ns", p.ChannelNamespace, "name", p.ChannelName, "key", p.ChannelKey, "err", oerr.Error())
			case has:
				preTurn = map[string]string{spiceboxv1alpha1.AnnotationTriggerOwnerSubject: subject}
			}
		}
	}
	dec, err := pl.Deliver(ctx, channelkinds.InboundEvent{
		Channel:            &ch,
		ChannelKey:         p.ChannelKey,
		MessageText:        p.MessageText,
		AuthzSubject:       ch.Spec.AuthzSubject,
		RawDelivery:        p.RawDelivery,
		DeliveryEvent:      p.DeliveryEvent,
		PreTurnAnnotations: preTurn,
	})
	if err != nil {
		logger.Info("webhook inbound: pipeline.Deliver errored",
			"ns", p.ChannelNamespace, "name", p.ChannelName, "key", p.ChannelKey, "err", err.Error())
		return
	}
	if dec.Outcome == channelkinds.OutcomeInternalError {
		logger.Info("webhook inbound: pipeline returned InternalError",
			"ns", p.ChannelNamespace, "name", p.ChannelName, "key", p.ChannelKey, "notice", dec.Notice.Category())
	}
}
