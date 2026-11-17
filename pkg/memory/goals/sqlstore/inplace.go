package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
)

var _ goals.InPlaceStore = (*Store)(nil)

// CommitInPlace implements goals.InPlaceStore: it rewrites the goal's payload
// at its current revision, guarded by the full stored content (m.Prior)
// rather than the revision alone, and appends the event to the sequenced
// execution audit, whose rows are not keyed by revision.
func (s *Store) CommitInPlace(ctx context.Context, m goals.Mutation) (goals.Goal, error) {
	g, err := s.commitInPlace(ctx, m)
	if err != nil {
		// A concurrent identical request may have committed first.
		if replay, ok, rerr := s.Receipt(ctx, m.Goal.Domain, m.RequestID, m.Hash); rerr != nil {
			return goals.Goal{}, errors.Join(err, rerr)
		} else if ok {
			return replay, nil
		}
	}
	return g, err
}

func (s *Store) commitInPlace(ctx context.Context, m goals.Mutation) (goals.Goal, error) {
	if m.Expected == 0 || m.Expected != m.Goal.Revision || len(m.Prior) == 0 {
		return goals.Goal{}, goals.ErrConflict
	}
	gb, err := json.Marshal(m.Goal)
	if err != nil {
		return goals.Goal{}, err
	}
	eb, err := json.Marshal(m.Event)
	if err != nil {
		return goals.Goal{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return goals.Goal{}, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.Info("goal in-place commit rollback failed", "goal", m.Goal.ID, "error", err)
		}
	}()
	// The receipt insert takes the write lock before anything is read.
	res, err := tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_receipts(domain,request_id,hash,payload) VALUES(?,?,?,?) ON CONFLICT(domain,request_id) DO NOTHING`), m.Goal.Domain.ID(), m.RequestID, m.Hash, string(gb))
	if err != nil {
		return goals.Goal{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return goals.Goal{}, err
	}
	if n == 0 {
		g, _, err := s.receipt(ctx, tx, m.Goal.Domain, m.RequestID, m.Hash)
		return g, err
	}
	if err := s.checkPrior(ctx, tx, m); err != nil {
		return goals.Goal{}, err
	}
	res, err = tx.ExecContext(ctx, s.query(`UPDATE oap_goals SET state=?,payload=? WHERE domain=? AND id=? AND revision=?`), string(m.Goal.State), string(gb), m.Goal.Domain.ID(), m.Goal.ID, m.Expected)
	if err != nil {
		return goals.Goal{}, err
	}
	if err := affected(res, nil); err != nil {
		return goals.Goal{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oap_goal_dispatch_lock SET generation=generation+1 WHERE id=1`); err != nil {
		return goals.Goal{}, err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM oap_goal_dispatch_lock WHERE id=1`).Scan(&sequence); err != nil {
		return goals.Goal{}, err
	}
	if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_execution_events(id,domain,goal_id,revision,sequence,payload) VALUES(?,?,?,?,?,?)`), m.Event.ID, m.Goal.Domain.ID(), m.Goal.ID, m.Goal.Revision, sequence, string(eb)); err != nil {
		return goals.Goal{}, err
	}
	if err := tx.Commit(); err != nil {
		return goals.Goal{}, err
	}
	return decode(string(gb))
}

// checkPrior refuses a mutation whose goal changed since m.Prior was read.
// It must run inside the write transaction, after the write lock is taken.
func (s *Store) checkPrior(ctx context.Context, tx *sql.Tx, m goals.Mutation) error {
	if len(m.Prior) == 0 {
		return nil
	}
	current, err := s.lockedGoal(ctx, tx, m.Goal.Domain, m.Goal.ID)
	if err != nil {
		return err
	}
	have, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if !bytes.Equal(have, m.Prior) {
		return goals.ErrConflict
	}
	return nil
}
