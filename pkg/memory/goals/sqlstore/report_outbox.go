package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
)

func (s *Store) PendingReportedObservations(ctx context.Context, limit int) ([]goals.Occurrence, error) {
	if limit < 1 || limit > 100 {
		return nil, goals.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT occurrence_id FROM oap_goal_report_outbox WHERE disposition='' ORDER BY occurrence_id LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(err, rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var result []goals.Occurrence
	for _, id := range ids {
		o, err := s.Occurrence(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, o)
	}
	return result, nil
}

func (s *Store) AcknowledgeReportedObservation(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, s.query(`UPDATE oap_goal_report_outbox SET disposition='published' WHERE occurrence_id=? AND disposition IN ('','published')`), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return goals.ErrNotFound
	}
	return nil
}

var _ goals.ReportOutbox = (*Store)(nil)

func (s *Store) DiscardReportedObservation(ctx context.Context, id, reason string, now time.Time) error {
	if reason != "authority_denied" && reason != "source_missing" {
		return goals.ErrInvalid
	}
	_, err := s.dispatchTx(ctx, func(tx *sql.Tx) (goals.Occurrence, error) {
		o, err := s.occurrence(ctx, tx, id)
		if err != nil {
			return o, err
		}
		result, err := tx.ExecContext(ctx, s.query(`UPDATE oap_goal_report_outbox SET disposition=? WHERE occurrence_id=? AND disposition=''`), reason, id)
		if err != nil {
			return o, err
		}
		n, err := result.RowsAffected()
		if err != nil || n == 0 {
			return o, err
		}
		return o, s.auditOccurrence(ctx, tx, o, "execution_report_discarded_"+reason, now)
	})
	return err
}
