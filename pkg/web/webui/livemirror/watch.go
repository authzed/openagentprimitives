package livemirror

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// NATSSubscriber is the minimal *nats.Conn surface WatchOutbound needs — just
// enough to register a callback subscription. Declared as an interface (using
// nats.MsgHandler, the exact named type *nats.Conn.Subscribe already uses, so
// *nats.Conn satisfies it with no adapter) so callers can pass a fake in
// tests without a live NATS connection.
type NATSSubscriber interface {
	Subscribe(subj string, cb nats.MsgHandler) (*nats.Subscription, error)
}

// WatchOutbound subscribes to a session's out.<kind> NATS subjects — one per
// entry in kinds — decodes each message into a channelevents.Envelope, and
// emits it on the returned channel. This is the subscribe-and-decode core
// every live mirror of a session's outbound events needs identically: the
// built-in web chat's message mirror and the session-view page's live stream
// each watch a different kind set off the same (ns, name) prefix. A caller
// needing session-specific enrichment layers it on the decoded Envelope —
// nothing session- or view-specific belongs here.
//
// The channel is buffered (capacity 8) so a slow consumer does not stall NATS
// delivery. Subscriptions are torn down when ctx is done; the channel itself is
// deliberately NOT closed, because a NATS callback goroutine may still be
// mid-send when ctx is cancelled. Senders guard on ctx.Done() and the consumer
// must stop reading on the same ctx.Done() rather than rely on closure.
//
// A subscribe error for ANY requested kind is fail-closed: every subscription
// already registered is unsubscribed, no channel is returned, and the caller
// gets the joined error.
//
// Every emitted Envelope has been cross-checked against the subject it arrived
// on (channelevents.AuthorizeOutSubject). The subscriptions here are concrete
// per-session subjects, so ROUTING is sound without the check — but the decoded
// Envelope a consumer forwards carries the PUBLISHER's own Session claim, and a
// runner holds publish on its whole "ap.session.<ns>.<own-name>.>" tree.
// pkg/web/webui/sessionview writes the envelope as-is to the authorized
// viewer's websocket, so without this a session could label its live-view
// events with another session's namespace/name. Dropping rather than
// relabelling matches every other consumer of a session subject: publisher and
// subject disagreeing is a bug or an attack, never normal traffic.
func WatchOutbound(ctx context.Context, nc NATSSubscriber, ns, name string, kinds ...channelevents.Kind) (<-chan channelevents.Envelope, error) {
	out := make(chan channelevents.Envelope, 8)
	// log.FromContext, not a passed-in logger: webd's BaseContext carries the
	// process logger into every request context, and these watchers start per
	// request, so a dropped envelope leaves a trace instead of vanishing.
	logger := log.FromContext(ctx).WithValues("session", ns+"/"+name)

	onMsg := func(msg *nats.Msg) {
		var env channelevents.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			logger.Info("livemirror: undecodable envelope dropped",
				"subject", msg.Subject, "err", err.Error())
			return
		}
		if _, _, err := channelevents.AuthorizeOutSubject(msg.Subject, env); err != nil {
			logger.Info("livemirror: envelope dropped, session claim does not match its subject",
				"subject", msg.Subject, "kind", string(env.Kind), "err", err.Error())
			return
		}
		select {
		case out <- env:
		case <-ctx.Done():
		}
	}

	prefix := channelevents.SubjectPrefix(ns, name)
	subs := make([]*nats.Subscription, 0, len(kinds))
	var errs []error
	for _, k := range kinds {
		sub, err := nc.Subscribe(channelevents.SubjectOut(prefix, k), onMsg)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		subs = append(subs, sub)
	}
	if len(errs) > 0 {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
		close(out)
		return nil, errors.Join(errs...)
	}

	go func() {
		<-ctx.Done()
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
		// out is deliberately NOT closed: NATS callback goroutines may still be
		// mid-emit. Senders guard on ctx.Done; the consumer exits on ctx.Done.
	}()

	return out, nil
}
