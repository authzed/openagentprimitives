package sessionevents

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// Consumer owns the meaning of a reviewed target and its live authority. The
// event framework dispatches registered consumers without knowing their domain.
type Consumer interface {
	TriggerAuthority
	LaunchConsumer
}

type consumerKind struct {
	name string
	Consumer
}

type Consumers struct {
	Legacy string
	kinds  *kindregistry.Registry[consumerKind]
}

func NewConsumers() *Consumers {
	return &Consumers{kinds: kindregistry.New[consumerKind]("event consumers", func(c consumerKind) string { return c.name })}
}

func (r *Consumers) Register(name string, c Consumer) { r.kinds.Register(consumerKind{name, c}) }
func (r *Consumers) consumer(s Subscription) (Consumer, error) {
	if r == nil || r.kinds == nil {
		return nil, ErrDenied
	}
	key := s.Consumer
	if key == "" {
		key = r.Legacy
	}
	c, ok := r.kinds.Get(key)
	if !ok || c.Consumer == nil {
		return nil, ErrDenied
	}
	return c.Consumer, nil
}

func (r *Consumers) CheckSubscription(ctx context.Context, s Subscription) error {
	c, err := r.consumer(s)
	if err != nil {
		return err
	}
	return c.CheckSubscription(ctx, s)
}

func (r *Consumers) CheckObservation(ctx context.Context, s Subscription, o Observation) error {
	c, err := r.consumer(s)
	if err != nil {
		return err
	}
	return c.CheckObservation(ctx, s, o)
}

func (r *Consumers) Materialize(ctx context.Context, l Launch) error {
	c, err := r.consumer(l.Subscription)
	if err != nil {
		return err
	}
	return c.Materialize(ctx, l)
}
