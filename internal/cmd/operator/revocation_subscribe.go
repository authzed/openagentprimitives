package main

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/credential"
)

// subscribeRevocation wires the oap.revocation subject to the operator's token
// broker so a revoked credential stops being resolved into new ToolCalls.
//
// The operator is BOTH the sole publisher of revocation envelopes and — through
// the broker the ToolCall reconciler calls on every sandbox exec — a consumer of
// them. Publishing without subscribing left its own broker cache authoritative
// and stale: because static credentials are cached with a zero (never) expiry,
// a revoked credential kept being injected into every NEWLY created ToolCall for
// the lifetime of the operator process. Unlike a runner, which holds only live
// in-flight state, the operator's exposure was on calls that had not yet started.
//
// Safe to call with a nil conn (NATS unreachable at startup): no-ops, matching
// every other operator NATS consumer. Returns an error so a failed subscribe is
// fail-loud at startup rather than a silently un-invalidated cache.
func subscribeRevocation(ctx context.Context, nc *nats.Conn, b credential.SecretInvalidator) error {
	if nc == nil {
		return nil
	}
	return subscribeRevocationOnBus(ctx, &operatorNATSAdapter{conn: nc}, b)
}

// operatorNATSAdapter adapts a *nats.Conn to revocation.NATSSubscriber so the
// subscriber core can run against a fake bus in tests.
type operatorNATSAdapter struct{ conn *nats.Conn }

func (a *operatorNATSAdapter) Subscribe(subject string, handler func([]byte)) error {
	_, err := a.conn.Subscribe(subject, func(msg *nats.Msg) { handler(msg.Data) })
	return err
}

// subscribeRevocationOnBus is the testable core of subscribeRevocation: it takes
// an arbitrary NATSSubscriber so tests can inject a fake bus.
//
// Only the credential Kind is registered. tool-origin revocations invalidate an
// in-process set that lives in each runner, not here; the subscriber skips kinds
// with no registered Invalidator.
//
// The operator subscribes as revocation.AllNamespaces because its broker caches
// credentials resolved on behalf of sessions in every namespace. Passing the
// operator's own namespace would work only by accident — credential revocations
// happen to be published cluster-wide today — and would silently stop working
// the day any revocation is namespace-scoped.
func subscribeRevocationOnBus(ctx context.Context, bus revocation.NATSSubscriber, b credential.SecretInvalidator) error {
	reg := revocation.NewRegistry()
	if err := reg.Register(credential.New(b)); err != nil {
		return fmt.Errorf("revocation: register credential invalidator: %w", err)
	}
	if err := revocation.RegisterSubscriber(ctx, bus, reg, revocation.AllNamespaces); err != nil {
		return fmt.Errorf("revocation: subscribe %s: %w", revocation.Subject, err)
	}
	return nil
}
