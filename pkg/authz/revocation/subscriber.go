package revocation

import (
	"context"
	"encoding/json"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"
)

// Subject is the NATS subject every KindRevoked envelope is published on.
const Subject = subjects.Revocation

// NATSSubscriber is the slice of a NATS client the subscriber needs.
type NATSSubscriber interface {
	// Subscribe registers handler for every message on subject; handler is
	// invoked per message with the raw payload. An error means this process
	// will never learn of a revocation, so registration failure must be
	// surfaced at startup rather than logged and forgotten.
	Subscribe(subject string, handler func([]byte)) error
}

// RegisterSubscriber wires Subject to scope-filtered registry dispatch for a
// runner whose session is in myNamespace. Malformed envelopes and unknown
// kinds are logged and skipped (never crash).
//
// The optional onRevoked hook is called after each scope-matched invalidation
// that SUCCEEDED, so callers can emit audit events or update in-process state.
// The hook receives the revoked kind and key so callers can record them
// durably (e.g. in the lifecycle log for restart re-application). Pass nil or
// omit it when no post-invalidation action is needed.
//
// A failed Invalidate is logged at Error and the hook is NOT called. The hook is
// how a revocation becomes a durable claim (the runner appends a signed
// lifecycle record), and a revocation that reached only SOME holders of a
// credential is strictly worse than one that reports failure: the caller
// believes the credential is dead while a live copy keeps authenticating.
// Recording success after a partial failure writes exactly that into the
// tamper-evident log.
//
// There is deliberately NO retry here. Delivery is core NATS, which has no
// ack/nak to requeue with, and every Invalidator registered in this repo either
// cannot fail or fails permanently, so a retry would re-run a decided outcome.
// An Invalidator that can fail transiently must retry internally, where it knows
// the difference.
func RegisterSubscriber(ctx context.Context, bus NATSSubscriber, reg *Registry, myNamespace string, onRevoked ...func(kind, key string)) error {
	hook := func(_, _ string) {}
	if len(onRevoked) > 0 && onRevoked[0] != nil {
		hook = onRevoked[0]
	}
	logger := log.FromContext(ctx).WithName("revocation-subscriber")
	return bus.Subscribe(Subject, func(data []byte) {
		var e channelevents.Envelope
		if err := json.Unmarshal(data, &e); err != nil {
			logger.Info("malformed envelope", "err", err.Error())
			return
		}
		if e.Kind != channelevents.KindRevoked {
			return
		}
		var pl channelevents.RevokedPayload
		if err := json.Unmarshal(e.Payload, &pl); err != nil {
			logger.Info("malformed payload", "err", err.Error())
			return
		}
		if !Applies(pl.Scope, myNamespace) {
			return
		}
		inv, ok := reg.Lookup(pl.Kind)
		if !ok {
			logger.Info("no invalidator for kind; skipping", "kind", pl.Kind)
			return
		}
		if err := inv.Invalidate(pl.Key); err != nil {
			// Loud, and explicit about the consequence: the capability may
			// still be live in this process, and nothing downstream will
			// record it as revoked. An operator reading this line should not
			// have to know what the hook does to understand what is wrong.
			logger.Error(err, "revocation NOT applied; capability may still be live and no revocation record will be emitted",
				"kind", pl.Kind, "key", pl.Key, "namespace", myNamespace)
			return
		}
		hook(pl.Kind, pl.Key)
	})
}
