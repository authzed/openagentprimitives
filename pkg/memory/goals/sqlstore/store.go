// Package sqlstore implements transactional goals on database/sql connections.
// SQLite and PostgreSQL adapters supply the existing operator database handles.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
)

type Store struct {
	db       *sql.DB
	postgres bool
}

func New(db *sql.DB, postgres bool) *Store { return &Store{db: db, postgres: postgres} }

var _ goals.Store = (*Store)(nil)

func (s *Store) query(q string) string {
	if !s.postgres {
		return q
	}
	n := 0
	return replaceParams(q, &n)
}
func replaceParams(q string, n *int) string {
	var b strings.Builder
	for _, r := range q {
		if r == '?' {
			*n++
			fmt.Fprintf(&b, "$%d", *n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Store) Migrate(ctx context.Context) error {
	// Additive migrations are transactional and repeatable after a crash.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.Info("goals migration rollback failed", "error", err)
		}
	}()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS oap_goal_schema (version INTEGER PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS oap_goals (domain TEXT NOT NULL, id TEXT NOT NULL, revision BIGINT NOT NULL, state TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(domain,id))`,
		`CREATE TABLE IF NOT EXISTS oap_goal_receipts (domain TEXT NOT NULL, request_id TEXT NOT NULL, hash TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(domain,request_id))`,
		`CREATE TABLE IF NOT EXISTS oap_goal_events (id TEXT PRIMARY KEY, domain TEXT NOT NULL, goal_id TEXT NOT NULL, revision BIGINT NOT NULL, payload TEXT NOT NULL, envelope TEXT NOT NULL DEFAULT '', published INTEGER NOT NULL DEFAULT 0, UNIQUE(domain,goal_id,revision))`,
		`CREATE INDEX IF NOT EXISTS oap_goals_list ON oap_goals(domain,state,id)`,
		`CREATE INDEX IF NOT EXISTS oap_goal_events_pending ON oap_goal_events(published,domain,goal_id,revision)`,
		`CREATE TABLE IF NOT EXISTS oap_goal_dispatch_lock (id INTEGER PRIMARY KEY, generation BIGINT NOT NULL)`,
		`INSERT INTO oap_goal_dispatch_lock(id,generation) VALUES(1,0) ON CONFLICT(id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS oap_goal_occurrences (id TEXT PRIMARY KEY, domain TEXT NOT NULL, owner_key TEXT NOT NULL, class_key TEXT NOT NULL, goal_id TEXT NOT NULL, goal_revision BIGINT NOT NULL, consent_digest TEXT NOT NULL, due_at BIGINT NOT NULL, expires_at BIGINT NOT NULL, state TEXT NOT NULL, worker TEXT NOT NULL DEFAULT '', fence BIGINT NOT NULL DEFAULT 0, lease_until BIGINT NOT NULL DEFAULT 0, session_name TEXT NOT NULL, session_uid TEXT NOT NULL DEFAULT '', UNIQUE(domain,goal_id,goal_revision))`,
		`CREATE INDEX IF NOT EXISTS oap_goal_occurrences_due ON oap_goal_occurrences(state,due_at,lease_until)`,
		`CREATE INDEX IF NOT EXISTS oap_goal_occurrences_owner ON oap_goal_occurrences(owner_key,state)`,
		`CREATE INDEX IF NOT EXISTS oap_goal_occurrences_class ON oap_goal_occurrences(class_key,state)`,
		`CREATE TABLE IF NOT EXISTS oap_goal_execution_events (id TEXT PRIMARY KEY, domain TEXT NOT NULL, goal_id TEXT NOT NULL, revision BIGINT NOT NULL, sequence BIGINT NOT NULL UNIQUE, payload TEXT NOT NULL, envelope TEXT NOT NULL DEFAULT '', published INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS oap_goal_execution_events_pending ON oap_goal_execution_events(published,sequence)`,
		`CREATE TABLE IF NOT EXISTS oap_goal_run_outcomes (occurrence_id TEXT PRIMARY KEY, payload TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS oap_goal_run_proposals (occurrence_id TEXT PRIMARY KEY, payload TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS oap_goal_run_replies (occurrence_id TEXT PRIMARY KEY, operation_id TEXT NOT NULL UNIQUE, payload TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("goals migration: %w", err)
		}
	}
	var newer int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oap_goal_schema WHERE version>4`).Scan(&newer); err != nil {
		return err
	}
	if newer > 0 {
		return fmt.Errorf("goals schema is newer than this operator")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO oap_goal_schema(version) VALUES(4) ON CONFLICT(version) DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}
func decode(raw string) (goals.Goal, error) {
	var g goals.Goal
	err := json.Unmarshal([]byte(raw), &g)
	return g, err
}
func (s *Store) Get(ctx context.Context, d goals.Domain, id string) (goals.Goal, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_goals WHERE domain=? AND id=?`), d.ID(), id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return goals.Goal{}, goals.ErrNotFound
	}
	if err != nil {
		return goals.Goal{}, err
	}
	return decode(raw)
}
func (s *Store) List(ctx context.Context, d goals.Domain, r goals.ListRequest) (goals.Page, error) {
	q := `SELECT payload FROM oap_goals WHERE domain=? AND id>?`
	args := []any{d.ID(), r.After}
	if r.State != "" {
		q += ` AND state=?`
		args = append(args, string(r.State))
	}
	q += ` ORDER BY id LIMIT ?`
	args = append(args, r.Limit+1)
	rows, err := s.db.QueryContext(ctx, s.query(q), args...)
	if err != nil {
		return goals.Page{}, err
	}
	defer rows.Close()
	p := goals.Page{Goals: []goals.Goal{}}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return goals.Page{}, err
		}
		g, err := decode(raw)
		if err != nil {
			return goals.Page{}, err
		}
		p.Goals = append(p.Goals, g)
	}
	if err := rows.Err(); err != nil {
		return goals.Page{}, err
	}
	if len(p.Goals) > r.Limit {
		p.Goals = p.Goals[:r.Limit]
		p.Next = p.Goals[len(p.Goals)-1].ID
	}
	return p, nil
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) receipt(ctx context.Context, q querier, d goals.Domain, key, hash string) (goals.Goal, bool, error) {
	var h, raw string
	err := q.QueryRowContext(ctx, s.query(`SELECT hash,payload FROM oap_goal_receipts WHERE domain=? AND request_id=?`), d.ID(), key).Scan(&h, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return goals.Goal{}, false, nil
	}
	if err != nil {
		return goals.Goal{}, false, err
	}
	if h != hash {
		return goals.Goal{}, false, goals.ErrConflict
	}
	g, err := decode(raw)
	return g, true, err
}
func (s *Store) Receipt(ctx context.Context, d goals.Domain, key, hash string) (goals.Goal, bool, error) {
	return s.receipt(ctx, s.db, d, key, hash)
}
func (s *Store) Commit(ctx context.Context, m goals.Mutation) (goals.Goal, error) {
	g, err := s.commit(ctx, m)
	if err != nil {
		// A concurrent identical request may have committed while our CAS lost.
		if prior, ok, rerr := s.Receipt(ctx, m.Goal.Domain, m.RequestID, m.Hash); rerr != nil {
			return goals.Goal{}, errors.Join(err, rerr)
		} else if ok {
			return prior, nil
		}
	}
	return g, err
}
func (s *Store) commit(ctx context.Context, m goals.Mutation) (goals.Goal, error) {
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
			slog.Info("goals transaction rollback failed", "goal", m.Goal.ID, "error", err)
		}
	}()
	// Take the write lock before reading: a deferred SQLite read transaction
	// cannot safely upgrade its snapshot after another writer commits.
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
	if m.Expected == 0 {
		res, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_goals(domain,id,revision,state,payload) VALUES(?,?,?,?,?) ON CONFLICT(domain,id) DO NOTHING`), m.Goal.Domain.ID(), m.Goal.ID, m.Goal.Revision, string(m.Goal.State), string(gb))
	} else {
		res, err = tx.ExecContext(ctx, s.query(`UPDATE oap_goals SET revision=?,state=?,payload=? WHERE domain=? AND id=? AND revision=?`), m.Goal.Revision, string(m.Goal.State), string(gb), m.Goal.Domain.ID(), m.Goal.ID, m.Expected)
	}
	if err != nil {
		return goals.Goal{}, err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return goals.Goal{}, err
	}
	if n != 1 {
		return goals.Goal{}, goals.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_events(id,domain,goal_id,revision,payload) VALUES(?,?,?,?,?)`), m.Event.ID, m.Goal.Domain.ID(), m.Goal.ID, m.Goal.Revision, string(eb)); err != nil {
		return goals.Goal{}, err
	}
	if err := tx.Commit(); err != nil {
		return goals.Goal{}, err
	}
	return decode(string(gb))
}
func (s *Store) Pending(ctx context.Context, limit int) ([]goals.Event, error) {
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT payload FROM (SELECT domain,goal_id,revision,payload,envelope,published,0 AS sequence FROM oap_goal_events UNION ALL SELECT domain,goal_id,revision,payload,envelope,published,sequence FROM oap_goal_execution_events) AS events WHERE published=0 ORDER BY CASE WHEN envelope='' THEN 1 ELSE 0 END,domain,goal_id,revision,sequence LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []goals.Event{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var e goals.Event
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) SaveEnvelope(ctx context.Context, id string, b json.RawMessage) error {
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE `+eventTable(id)+` SET envelope=? WHERE id=? AND (envelope='' OR envelope=?)`), string(b), id, string(b))
	return affected(res, err)
}
func (s *Store) Envelope(ctx context.Context, id string) (json.RawMessage, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT envelope FROM `+eventTable(id)+` WHERE id=?`), id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, goals.ErrNotFound
	}
	return json.RawMessage(raw), err
}
func (s *Store) Published(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE `+eventTable(id)+` SET published=1 WHERE id=? AND envelope<>''`), id)
	return affected(res, err)
}

func eventTable(id string) string {
	if strings.HasPrefix(id, "goalev-occ-") {
		return "oap_goal_execution_events"
	}
	return "oap_goal_events"
}
func affected(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return goals.ErrConflict
	}
	return nil
}
