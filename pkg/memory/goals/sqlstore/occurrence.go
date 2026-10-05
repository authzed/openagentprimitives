package sqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/google/uuid"
)

var _ goals.OccurrenceStore = (*Store)(nil)

const occurrenceColumns = `id,domain,goal_id,goal_revision,consent_digest,due_at,expires_at,state,worker,fence,lease_until,session_name,session_uid`

type rowScanner interface{ Scan(...any) error }

func scanOccurrence(row rowScanner) (goals.Occurrence, error) {
	var o goals.Occurrence
	var domain string
	var due, expires, lease int64
	if err := row.Scan(&o.ID, &domain, &o.GoalID, &o.GoalRevision, &o.ConsentDigest, &due, &expires, &o.State, &o.Worker, &o.Fence, &lease, &o.SessionName, &o.SessionUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return o, goals.ErrNotFound
		}
		return o, err
	}
	// Domain is resolved from the goal row, never reconstructed from key text.
	o.DueAt = time.Unix(0, due).UTC()
	o.ExpiresAt = time.Unix(0, expires).UTC()
	if lease != 0 {
		o.LeaseUntil = time.Unix(0, lease).UTC()
	}
	return o, nil
}

func (s *Store) occurrence(ctx context.Context, q querier, id string) (goals.Occurrence, error) {
	o, err := scanOccurrence(q.QueryRowContext(ctx, s.query(`SELECT `+occurrenceColumns+` FROM oap_goal_occurrences WHERE id=?`), id))
	if err != nil {
		return o, err
	}
	var raw string
	if err := q.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_goals WHERE domain=(SELECT domain FROM oap_goal_occurrences WHERE id=?) AND id=?`), id, o.GoalID).Scan(&raw); err != nil {
		return o, err
	}
	g, err := decode(raw)
	o.Domain = g.Domain
	return o, err
}

func (s *Store) Occurrence(ctx context.Context, id string) (goals.Occurrence, error) {
	return s.occurrence(ctx, s.db, id)
}

// dispatchTx takes the database write lock before reading. PostgreSQL locks the
// shared row and SQLite serializes writers. This makes capacity reservation and
// worker takeover one transaction across independent operator processes.
func (s *Store) dispatchTx(ctx context.Context, f func(*sql.Tx) (goals.Occurrence, error)) (goals.Occurrence, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return goals.Occurrence{}, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.Info("goal dispatch rollback failed", "error", err)
		}
	}()
	if _, err := tx.ExecContext(ctx, `UPDATE oap_goal_dispatch_lock SET generation=generation+1 WHERE id=1`); err != nil {
		return goals.Occurrence{}, err
	}
	o, err := f(tx)
	if err != nil {
		return goals.Occurrence{}, err
	}
	if err := tx.Commit(); err != nil {
		return goals.Occurrence{}, err
	}
	return o, nil
}

func executionMatches(g goals.Goal, revision int64, digest string) bool {
	c := g.Execution
	return g.State == goals.Active && g.Revision == revision && c != nil && c.Digest == digest && c.Decision != nil &&
		c.Decision.Approved && c.Decision.Owner == g.Domain.Owner && c.Decision.Digest == digest && c.Decision.Witness != "" && revision == c.RequestRevision+1
}

func (s *Store) lockedGoal(ctx context.Context, tx *sql.Tx, d goals.Domain, id string) (goals.Goal, error) {
	q := `SELECT payload FROM oap_goals WHERE domain=? AND id=?`
	if s.postgres {
		q += ` FOR UPDATE`
	}
	var raw string
	if err := tx.QueryRowContext(ctx, s.query(q), d.ID(), id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return goals.Goal{}, goals.ErrNotFound
		}
		return goals.Goal{}, err
	}
	return decode(raw)
}

// auditOccurrence commits the audit intent in the SAME transaction as every
// dispatch state change. The existing goal publisher signs and drains it.
func (s *Store) auditOccurrence(ctx context.Context, tx *sql.Tx, o goals.Occurrence, action string, now time.Time) error {
	var raw string
	if err := tx.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_goals WHERE domain=? AND id=?`), o.Domain.ID(), o.GoalID).Scan(&raw); err != nil {
		return err
	}
	g, err := decode(raw)
	if err != nil {
		return err
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM oap_goal_dispatch_lock WHERE id=1`).Scan(&sequence); err != nil {
		return err
	}
	e := goals.Event{ID: "goalev-occ-" + id.String(), Goal: g, Action: action, Session: o.SessionName, Proof: o.ConsentDigest, Occurrence: &o, OccurredAt: &now}
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_execution_events(id,domain,goal_id,revision,sequence,payload) VALUES(?,?,?,?,?,?)`), e.ID, o.Domain.ID(), o.GoalID, g.Revision, sequence, string(payload))
	return err
}

func (s *Store) Schedule(ctx context.Context, g goals.Goal) (goals.Occurrence, error) {
	if g.Execution == nil || !executionMatches(g, g.Revision, g.Execution.Digest) {
		return goals.Occurrence{}, goals.ErrDenied
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d/%s", g.Domain.ID(), g.ID, g.Revision, g.Execution.Digest)))
	id := "occ-" + hex.EncodeToString(h[:])
	return s.dispatchTx(ctx, func(tx *sql.Tx) (goals.Occurrence, error) {
		current, err := s.lockedGoal(ctx, tx, g.Domain, g.ID)
		if err != nil {
			return goals.Occurrence{}, err
		}
		if !executionMatches(current, g.Revision, g.Execution.Digest) {
			return goals.Occurrence{}, goals.ErrConflict
		}
		t := current.Execution.Terms
		// Length-prefixed hashing via the domain's JSON representation preserves
		// namespace and class UID boundaries without separator ambiguities.
		owner := goals.Domain{Namespace: g.Domain.Namespace, Owner: g.Domain.Owner}.ID()
		class := goals.Domain{Namespace: g.Domain.Namespace, ClassUID: g.Domain.ClassUID}.ID()
		res, err := tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_occurrences(id,domain,owner_key,class_key,goal_id,goal_revision,consent_digest,due_at,expires_at,state,session_name) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`), id, g.Domain.ID(), owner, class, g.ID, g.Revision, current.Execution.Digest, t.DueAt.UnixNano(), t.ExpiresAt.UnixNano(), string(goals.OccurrenceQueued), "goal-"+hex.EncodeToString(h[:16]))
		if err != nil {
			return goals.Occurrence{}, err
		}
		o, err := s.occurrence(ctx, tx, id)
		if err != nil {
			return o, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return o, err
		}
		if n == 1 {
			err = s.auditOccurrence(ctx, tx, o, "execution_scheduled", current.UpdatedAt)
		}
		return o, err
	})
}

// Due selects one head per owner before the next head, so a prolific owner's
// backlog cannot consume an entire batch. Claimed work is included for recovery
// even after consent expiry; authority is rechecked before any new activation.
func (s *Store) Due(ctx context.Context, now time.Time, limit int) ([]goals.Occurrence, error) {
	if limit < 1 || limit > 100 {
		return nil, goals.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT id FROM (SELECT o.id,o.due_at,ROW_NUMBER() OVER(PARTITION BY o.owner_key ORDER BY o.due_at,o.id) AS owner_rank FROM oap_goal_occurrences o JOIN oap_goals g ON g.domain=o.domain AND g.id=o.goal_id WHERE (o.state='queued' AND o.due_at<=? AND o.expires_at>? AND g.state='active' AND g.revision=o.goal_revision) OR (o.state IN ('claimed','running','unknown') AND o.lease_until<=?)) AS due ORDER BY owner_rank,due_at,id LIMIT ?`), now.UnixNano(), now.UnixNano(), now.UnixNano(), limit)
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
	result := make([]goals.Occurrence, 0, len(ids))
	for _, id := range ids {
		o, err := s.Occurrence(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, o)
	}
	return result, nil
}

func validLease(worker string, lease time.Duration) bool {
	return worker != "" && len(worker) <= 128 && lease >= time.Second && lease <= 5*time.Minute
}

func (s *Store) Claim(ctx context.Context, r goals.ClaimRequest) (goals.Occurrence, error) {
	if !validLease(r.Worker, r.Lease) || r.OwnerLimit < 1 || r.ClassLimit < 1 {
		return goals.Occurrence{}, goals.ErrInvalid
	}
	return s.dispatchTx(ctx, func(tx *sql.Tx) (goals.Occurrence, error) {
		o, err := s.occurrence(ctx, tx, r.ID)
		if err != nil {
			return o, err
		}
		if o.State == goals.OccurrenceQueued {
			if r.Now.Before(o.DueAt) || !r.Now.Before(o.ExpiresAt) {
				return o, goals.ErrDenied
			}
			g, err := s.lockedGoal(ctx, tx, o.Domain, o.GoalID)
			if err != nil {
				return o, err
			}
			if !executionMatches(g, o.GoalRevision, o.ConsentDigest) {
				return o, goals.ErrConflict
			}
			for _, cap := range []struct {
				column string
				limit  int
			}{{"owner_key", r.OwnerLimit}, {"class_key", r.ClassLimit}} {
				var count int
				if err := tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM oap_goal_occurrences WHERE `+cap.column+`=(SELECT `+cap.column+` FROM oap_goal_occurrences WHERE id=?) AND state IN ('claimed','running','unknown')`), o.ID).Scan(&count); err != nil {
					return o, err
				}
				if count >= cap.limit {
					return o, goals.ErrConflict
				}
			}
		} else if (o.State != goals.OccurrenceClaimed && o.State != goals.OccurrenceRunning && o.State != goals.OccurrenceUnknown) || r.Now.Before(o.LeaseUntil) {
			return o, goals.ErrConflict
		}
		// Recovery preserves the reservation and session identity even if the
		// goal was paused or authority expired: cleanup still needs a worker.
		state := o.State
		if state == goals.OccurrenceQueued {
			state = goals.OccurrenceClaimed
		}
		_, err = tx.ExecContext(ctx, s.query(`UPDATE oap_goal_occurrences SET state=?,worker=?,fence=fence+1,lease_until=? WHERE id=?`), string(state), r.Worker, r.Now.Add(r.Lease).UnixNano(), o.ID)
		if err != nil {
			return o, err
		}
		claimed, err := s.occurrence(ctx, tx, o.ID)
		if err != nil {
			return claimed, err
		}
		return claimed, s.auditOccurrence(ctx, tx, claimed, "execution_claimed", r.Now)
	})
}

func (s *Store) fenced(ctx context.Context, o goals.Occurrence, now time.Time, action string, f func(*sql.Tx, goals.Occurrence) error) (goals.Occurrence, error) {
	return s.dispatchTx(ctx, func(tx *sql.Tx) (goals.Occurrence, error) {
		current, err := s.occurrence(ctx, tx, o.ID)
		if err != nil {
			return current, err
		}
		if current.Fence != o.Fence || current.Worker != o.Worker || !now.Before(current.LeaseUntil) || (current.State != goals.OccurrenceClaimed && current.State != goals.OccurrenceRunning && current.State != goals.OccurrenceUnknown) {
			return current, goals.ErrConflict
		}
		if err := f(tx, current); err != nil {
			return current, err
		}
		changed, err := s.occurrence(ctx, tx, o.ID)
		if err != nil {
			return changed, err
		}
		return changed, s.auditOccurrence(ctx, tx, changed, action, now)
	})
}

func (s *Store) Attach(ctx context.Context, o goals.Occurrence, uid string, now time.Time) (goals.Occurrence, error) {
	if uid == "" {
		return goals.Occurrence{}, goals.ErrInvalid
	}
	return s.fenced(ctx, o, now, "execution_attached", func(tx *sql.Tx, current goals.Occurrence) error {
		if current.SessionUID != "" && current.SessionUID != uid {
			return goals.ErrConflict
		}
		_, err := tx.ExecContext(ctx, s.query(`UPDATE oap_goal_occurrences SET session_uid=?,state='running' WHERE id=?`), uid, o.ID)
		return err
	})
}

func (s *Store) Renew(ctx context.Context, o goals.Occurrence, now time.Time, lease time.Duration) (goals.Occurrence, error) {
	if !validLease(o.Worker, lease) {
		return goals.Occurrence{}, goals.ErrInvalid
	}
	return s.fenced(ctx, o, now, "execution_renewed", func(tx *sql.Tx, _ goals.Occurrence) error {
		_, err := tx.ExecContext(ctx, s.query(`UPDATE oap_goal_occurrences SET lease_until=? WHERE id=?`), now.Add(lease).UnixNano(), o.ID)
		return err
	})
}

func (s *Store) Finish(ctx context.Context, o goals.Occurrence, state goals.OccurrenceState, now time.Time) (goals.Occurrence, error) {
	if state != goals.OccurrenceSucceeded && state != goals.OccurrenceFailed && state != goals.OccurrenceCancelled && state != goals.OccurrenceUnknown {
		return goals.Occurrence{}, goals.ErrInvalid
	}
	return s.fenced(ctx, o, now, "execution_"+string(state), func(tx *sql.Tx, current goals.Occurrence) error {
		if current.SessionUID != o.SessionUID {
			return goals.ErrConflict
		}
		if current.SessionUID == "" && state != goals.OccurrenceUnknown && state != goals.OccurrenceCancelled {
			return goals.ErrInvalid
		}
		_, err := tx.ExecContext(ctx, s.query(`UPDATE oap_goal_occurrences SET state=? WHERE id=?`), string(state), o.ID)
		return err
	})
}
