package goals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
)

func (d *Dispatcher) drainReportedObservations(ctx context.Context) error {
	outbox, ok := d.Store.(domain.ReportOutbox)
	if !ok {
		return nil
	}
	pending, err := outbox.PendingReportedObservations(ctx, 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, o := range pending {
		if err := d.publishReportedObservation(ctx, o); err != nil {
			failures = append(failures, fmt.Errorf("observation %s: %w", o.ID, err))
			reason := ""
			if errors.Is(err, domain.ErrDenied) || errors.Is(err, sessionevents.ErrDenied) {
				reason = "authority_denied"
			}
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, sessionevents.ErrNotFound) {
				reason = "source_missing"
			}
			if reason != "" {
				if discardErr := outbox.DiscardReportedObservation(ctx, o.ID, reason, d.now()); discardErr != nil {
					failures = append(failures, discardErr)
				}
			}
			continue
		}
		if err := outbox.AcknowledgeReportedObservation(ctx, o.ID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Publication retries are independent of the runner's dispatch lease and lifetime.
func (d *Dispatcher) publishReportedObservation(ctx context.Context, o domain.Occurrence) error {
	if o.Proposal == nil || o.Proposal.Observation == nil {
		return fmt.Errorf("%w: report outbox missing observation", domain.ErrInvalid)
	}
	if d.EventIngester == nil || d.EventIngester.Store == nil {
		return fmt.Errorf("goal observation ingress unavailable")
	}
	g, err := d.Service.Store.Get(ctx, o.Domain, o.GoalID)
	if err != nil {
		return err
	}
	source := domain.ReportStream(g)
	_, err = d.EventIngester.Store.Get(ctx, source, o.ID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sessionevents.ErrNotFound) {
		return err
	}
	raw, err := json.Marshal(domain.ReportReference{Source: source, OccurrenceID: o.ID})
	if err != nil {
		return err
	}
	_, err = d.EventIngester.Ingest(ctx, domain.ReportSourceKind, raw)
	return err
}
