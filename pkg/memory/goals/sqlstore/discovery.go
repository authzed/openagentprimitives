package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
)

func discoveryDecode[T any](raw string, err error) (T, error) {
	var value T
	if errors.Is(err, sql.ErrNoRows) {
		return value, goals.ErrNotFound
	}
	if err != nil {
		return value, err
	}
	err = json.Unmarshal([]byte(raw), &value)
	return value, err
}

func (s *Store) DiscoveryPolicy(ctx context.Context, d goals.Domain, id string) (goals.DiscoveryPolicy, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_discovery_policies WHERE domain=? AND id=?`), d.ID(), id).Scan(&raw)
	return discoveryDecode[goals.DiscoveryPolicy](raw, err)
}

func (s *Store) DiscoveryProposal(ctx context.Context, d goals.Domain, id string) (goals.DiscoveryProposal, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_discovery_proposals WHERE domain=? AND id=?`), d.ID(), id).Scan(&raw)
	return discoveryDecode[goals.DiscoveryProposal](raw, err)
}

func discoveryRollback(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Info("discovery transaction rollback failed", "error", err)
	}
}

func (s *Store) discoveryTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE oap_goal_dispatch_lock SET generation=generation+1 WHERE id=1`); err != nil {
		discoveryRollback(tx)
		return nil, err
	}
	return tx, nil
}

func (s *Store) CreateDiscoveryPolicy(ctx context.Context, p goals.DiscoveryPolicy) (goals.DiscoveryPolicy, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	_, err = s.db.ExecContext(ctx, s.query(`INSERT INTO oap_discovery_policies(id,domain,payload,used,stopped) VALUES(?,?,?,0,0) ON CONFLICT(id) DO NOTHING`), p.ID, p.Template.Domain.ID(), string(raw))
	if err != nil {
		return p, err
	}
	prior, err := s.DiscoveryPolicy(ctx, p.Template.Domain, p.ID)
	if err != nil {
		return p, err
	}
	if !reflect.DeepEqual(prior.Request, p.Request) || prior.Actor.Domain != p.Actor.Domain || prior.Actor.SessionUID != p.Actor.SessionUID {
		return p, goals.ErrConflict
	}
	return prior, nil
}

func (s *Store) DecideDiscoveryPolicy(ctx context.Context, p goals.DiscoveryPolicy, d goals.ExecutionDecision) (goals.DiscoveryPolicy, error) {
	tx, err := s.discoveryTx(ctx)
	if err != nil {
		return p, err
	}
	defer discoveryRollback(tx)
	var raw string
	err = tx.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_discovery_policies WHERE domain=? AND id=?`), p.Template.Domain.ID(), p.ID).Scan(&raw)
	prior, err := discoveryDecode[goals.DiscoveryPolicy](raw, err)
	if err != nil {
		return p, err
	}
	if prior.Decision != nil {
		if *prior.Decision != d {
			return p, goals.ErrConflict
		}
		return prior, nil
	}
	if !reflect.DeepEqual(prior, p) || prior.Stopped {
		return p, goals.ErrConflict
	}
	p.Decision = &d
	p.Stopped = !d.Approved
	rawBytes, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_discovery_policies SET payload=?,stopped=? WHERE id=?`), string(rawBytes), boolInt(p.Stopped), p.ID); err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) StopDiscoveryPolicy(ctx context.Context, d goals.Domain, id string) error {
	tx, err := s.discoveryTx(ctx)
	if err != nil {
		return err
	}
	defer discoveryRollback(tx)
	var raw string
	err = tx.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_discovery_policies WHERE domain=? AND id=?`), d.ID(), id).Scan(&raw)
	p, err := discoveryDecode[goals.DiscoveryPolicy](raw, err)
	if err != nil {
		return err
	}
	p.Stopped = true
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_discovery_policies SET payload=?,stopped=1 WHERE id=?`), string(b), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateDiscoveryProposal(ctx context.Context, p goals.DiscoveryPolicy, q goals.DiscoveryProposal) (goals.DiscoveryProposal, error) {
	tx, err := s.discoveryTx(ctx)
	if err != nil {
		return q, err
	}
	defer discoveryRollback(tx)
	var raw string
	var used, stopped int
	err = tx.QueryRowContext(ctx, s.query(`SELECT payload,used,stopped FROM oap_discovery_policies WHERE domain=? AND id=?`), p.Template.Domain.ID(), p.ID).Scan(&raw, &used, &stopped)
	stored, err := discoveryDecode[goals.DiscoveryPolicy](raw, err)
	if err != nil {
		return q, err
	}
	if !reflect.DeepEqual(stored, p) || stopped != 0 || p.Decision == nil || !p.Decision.Approved {
		return q, goals.ErrDenied
	}
	subject := q.Goal.Execution.Terms.Event.Predicate.Subject
	err = tx.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_discovery_proposals WHERE policy_id=? AND subject=?`), p.ID, subject).Scan(&raw)
	if err == nil {
		return discoveryDecode[goals.DiscoveryProposal](raw, nil)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return q, err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM oap_discovery_proposals WHERE policy_id=? AND state='pending' AND expires_at>?`), p.ID, q.CreatedAt.UnixMicro()).Scan(&pending); err != nil {
		return q, err
	}
	if used >= p.Request.MaxProposals || pending >= p.Request.MaxPending {
		q.State = "suppressed"
	}
	b, err := json.Marshal(q)
	if err != nil {
		return q, err
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_discovery_proposals(id,domain,policy_id,subject,state,expires_at,payload) VALUES(?,?,?,?,?,?,?)`), q.ID, q.Goal.Domain.ID(), p.ID, subject, q.State, q.ExpiresAt.UnixMicro(), string(b)); err != nil {
		return q, err
	}
	if q.State == "pending" {
		if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_discovery_policies SET used=used+1 WHERE id=?`), p.ID); err != nil {
			return q, err
		}
	}
	return q, tx.Commit()
}

func (s *Store) DecideDiscoveryProposal(ctx context.Context, q goals.DiscoveryProposal, d goals.ExecutionDecision, now time.Time) (goals.DiscoveryProposal, error) {
	tx, err := s.discoveryTx(ctx)
	if err != nil {
		return q, err
	}
	defer discoveryRollback(tx)
	var raw string
	err = tx.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_discovery_proposals WHERE domain=? AND id=?`), q.Goal.Domain.ID(), q.ID).Scan(&raw)
	prior, err := discoveryDecode[goals.DiscoveryProposal](raw, err)
	if err != nil {
		return q, err
	}
	if prior.Decision != nil {
		if *prior.Decision != d {
			return q, goals.ErrConflict
		}
		return prior, nil
	}
	if !reflect.DeepEqual(prior, q) || q.State != "pending" || !now.Before(q.ExpiresAt) {
		return q, goals.ErrDenied
	}
	err = tx.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_discovery_policies WHERE domain=? AND id=?`), q.Goal.Domain.ID(), q.PolicyID).Scan(&raw)
	p, err := discoveryDecode[goals.DiscoveryPolicy](raw, err)
	if err != nil {
		return q, err
	}
	if p.Stopped || p.Decision == nil || !p.Decision.Approved || !now.Before(p.Template.Execution.Terms.ExpiresAt) {
		return q, goals.ErrDenied
	}
	q.Decision = &d
	q.State = "declined"
	if d.Approved {
		q.State = "accepted"
		g := q.Goal
		consent := *g.Execution
		g.Execution = &consent
		g.Revision++
		g.UpdatedAt = now
		g.Execution.Decision = &d
		gb, err := json.Marshal(g)
		if err != nil {
			return q, err
		}
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_goals(domain,id,revision,state,payload) VALUES(?,?,?,?,?)`), g.Domain.ID(), g.ID, g.Revision, string(g.State), string(gb)); err != nil {
			return q, err
		}
		event := goals.Event{ID: "goalev-" + q.ID, Goal: g, Action: "execution_decision", Session: g.OriginSession, Proof: d.Witness}
		eb, err := json.Marshal(event)
		if err != nil {
			return q, err
		}
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_events(id,domain,goal_id,revision,payload) VALUES(?,?,?,?,?)`), event.ID, g.Domain.ID(), g.ID, g.Revision, string(eb)); err != nil {
			return q, err
		}
	}
	b, err := json.Marshal(q)
	if err != nil {
		return q, err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_discovery_proposals SET state=?,payload=? WHERE id=?`), q.State, string(b), q.ID); err != nil {
		return q, err
	}
	return q, tx.Commit()
}

func (s *Store) PendingDiscovery(ctx context.Context, now time.Time, limit int) ([]goals.DiscoveryProposal, error) {
	if limit < 1 || limit > 100 {
		return nil, goals.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT payload FROM oap_discovery_proposals WHERE notified=0 AND state='pending' AND expires_at>? ORDER BY id LIMIT ?`), now.UnixMicro(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []goals.DiscoveryProposal
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		q, err := discoveryDecode[goals.DiscoveryProposal](raw, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) DiscoveryNotified(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.query(`UPDATE oap_discovery_proposals SET notified=1 WHERE id=?`), id)
	return err
}

func (s *Store) ActiveDiscoveryPolicies(ctx context.Context, after string, limit int) ([]goals.DiscoveryPolicy, error) {
	if limit < 1 || limit > 100 {
		return nil, goals.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT payload FROM oap_discovery_policies WHERE stopped=0 AND id>? ORDER BY id LIMIT ?`), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []goals.DiscoveryPolicy
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		p, err := discoveryDecode[goals.DiscoveryPolicy](raw, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DiscoveryPolicyNotification(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.query(`SELECT notified FROM oap_discovery_policies WHERE id=?`), id).Scan(&n)
	return n == 1, err
}

func (s *Store) DiscoveryPolicyNotified(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.query(`UPDATE oap_discovery_policies SET notified=1 WHERE id=?`), id)
	return err
}
