package revocation

import (
	"context"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// EventPublisher is the slice of the bus client the publisher needs.
type EventPublisher interface {
	// Publish sends one envelope onto the bus. An error means no subscriber saw
	// the revocation, so the credential stays usable in every process holding it
	// — the caller must surface it, never treat the revoke as delivered.
	Publish(ctx context.Context, env channelevents.Envelope) error
}

// Publisher emits KindRevoked envelopes. A nil bus is tolerated (Emit no-ops)
// for local-dev / no-NATS operator runs.
type Publisher struct{ bus EventPublisher }

// NewPublisher constructs a Publisher. nil bus → Emit is a no-op.
func NewPublisher(bus EventPublisher) *Publisher { return &Publisher{bus: bus} }

// Emit publishes one revocation. kind matches a registered Invalidator; key is
// kind-specific; scope is the namespace ("" = cluster-wide).
func (p *Publisher) Emit(ctx context.Context, kind, key, scope string) error {
	if p == nil || p.bus == nil {
		return nil
	}
	payload := channelevents.RevokedPayload{Kind: kind, Key: key, Scope: scope}
	env, err := channelevents.BuildEnvelope(
		spiceboxv1alpha1.IdentitiesNamespace, key, channelevents.KindRevoked, payload)
	if err != nil {
		return err
	}
	return p.bus.Publish(ctx, env)
}
