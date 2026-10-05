package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
)

var _ goals.CostStore = (*Store)(nil)

func (s *Store) RecordCost(ctx context.Context, o goals.Occurrence, c goals.RunCost, now time.Time) (goals.Occurrence, error) {
	if c.SessionUID == "" || c.SessionUID != o.SessionUID || (c.Estimate == nil && (c.Final || c.Reason == "")) || (c.Estimate != nil && (c.Estimate.AsOf.IsZero() || c.Estimate.AmountMicroUSD < 0)) {
		return goals.Occurrence{}, goals.ErrInvalid
	}
	c.ObservedAt = now.UTC()
	return s.dispatchTx(ctx, func(tx *sql.Tx) (goals.Occurrence, error) {
		current, err := s.occurrence(ctx, tx, o.ID)
		if err != nil {
			return current, err
		}
		if current.Fence != o.Fence || current.Worker != o.Worker || !now.Before(current.LeaseUntil) || current.SessionUID != c.SessionUID || current.GoalRevision != o.GoalRevision || (current.State != goals.OccurrenceClaimed && current.State != goals.OccurrenceRunning && current.State != goals.OccurrenceUnknown) {
			return current, goals.ErrConflict
		}
		if current.Cost != nil {
			old := *current.Cost
			old.ObservedAt = c.ObservedAt
			oldJSON, err := json.Marshal(old)
			if err != nil {
				return current, err
			}
			nextJSON, err := json.Marshal(c)
			if err != nil {
				return current, err
			}
			if bytes.Equal(oldJSON, nextJSON) {
				return current, nil
			}
			if current.Cost.Reason == "accounting_regressed" && (c.Final || c.Reason != "accounting_regressed") {
				return current, goals.ErrConflict
			}
			// Cleanup or a stale cache read cannot erase already observed spend.
			if current.Cost.Final || (current.Cost.Estimate != nil && (c.Estimate == nil || c.Estimate.AsOf.Before(&current.Cost.Estimate.AsOf) || c.Estimate.Currency != current.Cost.Estimate.Currency || c.Estimate.AmountMicroUSD < current.Cost.Estimate.AmountMicroUSD)) {
				return current, goals.ErrConflict
			}
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return current, err
		}
		if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_run_costs(occurrence_id,payload) VALUES(?,?) ON CONFLICT(occurrence_id) DO UPDATE SET payload=excluded.payload`), o.ID, string(raw)); err != nil {
			return current, err
		}
		current.Cost = &c
		return current, s.auditOccurrence(ctx, tx, current, "execution_cost_observed", now)
	})
}
