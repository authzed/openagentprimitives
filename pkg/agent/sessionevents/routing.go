package sessionevents

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RoutingStore scans durable evidence that has no admission for a watch yet.
// The admission itself is the checkpoint, including rejected events. A crash
// anywhere between ingest and route is therefore replayable without data loss.
type RoutingStore interface {
	ActiveSubscriptions(context.Context, string, int) ([]SubscriptionState, error)
	Unadmitted(context.Context, string, int) ([]Observation, error)
}
type Router struct {
	Triggers *Triggers
	Store    RoutingStore
	After    string
}

func (r *Router) Tick(ctx context.Context, now time.Time) error {
	if r == nil || r.Triggers == nil || r.Store == nil || r.Triggers.Store == nil || r.Triggers.Authority == nil {
		return ErrDenied
	}
	watches, err := r.Store.ActiveSubscriptions(ctx, r.After, 100)
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		r.After = ""
		return nil
	}
	var failures []error
	for _, state := range watches {
		sub := state.Subscription
		r.After = sub.ID
		if !now.Before(sub.EndsAt) {
			if err := r.Triggers.Store.StopSubscription(ctx, sub.ID, "expired"); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if err := r.Triggers.Authority.CheckSubscription(ctx, sub); err != nil {
			if errors.Is(err, ErrDenied) {
				if stopErr := r.Triggers.Store.StopSubscription(ctx, sub.ID, "authority_denied"); stopErr != nil {
					failures = append(failures, stopErr)
				}
			} else {
				failures = append(failures, fmt.Errorf("check watch %s: %w", sub.ID, err))
			}
			continue
		}
		if now.Before(sub.StartsAt) {
			continue
		}
		observations, err := r.Store.Unadmitted(ctx, sub.ID, 100)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, observation := range observations {
			if _, err := r.Triggers.Accept(ctx, sub.ID, observation.Source, observation.EventID, now); err != nil {
				if errors.Is(err, ErrDenied) {
					if stopErr := r.Triggers.Store.StopSubscription(ctx, sub.ID, "evidence_authority_denied"); stopErr != nil {
						failures = append(failures, stopErr)
					}
				}
				failures = append(failures, fmt.Errorf("route watch %s observation %s: %w", sub.ID, observation.ID(), err))
				break
			}
		}
	}
	return errors.Join(failures...)
}
