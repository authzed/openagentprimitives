// Package sqlstore persists normalized session observations and ingestion cursors
// on existing SQLite or PostgreSQL database/sql handles.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
)

type Store struct {
	db       *sql.DB
	postgres bool
}

func New(db *sql.DB, postgres bool) *Store { return &Store{db: db, postgres: postgres} }

var _ sessionevents.Store = (*Store)(nil)

func (s *Store) query(query string) string {
	if !s.postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func rollback(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Info("session event transaction rollback failed", "error", err)
	}
}
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS oap_session_observation_schema (version INTEGER PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS oap_session_observations (id TEXT PRIMARY KEY,source_key TEXT NOT NULL,event_id TEXT NOT NULL,digest TEXT NOT NULL,payload TEXT NOT NULL,UNIQUE(source_key,event_id))`,
		`CREATE TABLE IF NOT EXISTS oap_session_observation_cursors (source_key TEXT NOT NULL,publisher TEXT NOT NULL,sequence BIGINT NOT NULL,PRIMARY KEY(source_key,publisher))`,
		`CREATE TABLE IF NOT EXISTS oap_event_subscriptions (id TEXT PRIMARY KEY,digest TEXT NOT NULL,payload TEXT NOT NULL,used INTEGER NOT NULL,stopped INTEGER NOT NULL,stop_reason TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS oap_event_admissions (subscription_id TEXT NOT NULL,observation_id TEXT NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(subscription_id,observation_id))`,
		`CREATE TABLE IF NOT EXISTS oap_event_launches (id TEXT PRIMARY KEY,subscription_id TEXT NOT NULL,observation_id TEXT NOT NULL,due_at TEXT NOT NULL,expires_at TEXT NOT NULL,state TEXT NOT NULL,payload TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS oap_event_launch_pending ON oap_event_launches(state,due_at)`,
		`CREATE TABLE IF NOT EXISTS oap_session_observation_lock (id INTEGER PRIMARY KEY,generation BIGINT NOT NULL)`,
		`INSERT INTO oap_session_observation_lock(id,generation) VALUES(1,0) ON CONFLICT(id) DO NOTHING`,
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("session event migration: %w", err)
		}
	}
	var newer int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oap_session_observation_schema WHERE version>2`).Scan(&newer); err != nil {
		return err
	}
	if newer > 0 {
		return fmt.Errorf("session observation schema is newer than this operator")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO oap_session_observation_schema(version) VALUES(2) ON CONFLICT(version) DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Checkpoint(ctx context.Context, source sessionevents.Source, publisher string) (sessionevents.Checkpoint, error) {
	result := sessionevents.Checkpoint{Source: source, Publisher: publisher}
	if source.Validate() != nil || publisher == "" {
		return result, sessionevents.ErrInvalid
	}
	err := s.db.QueryRowContext(ctx, s.query(`SELECT sequence FROM oap_session_observation_cursors WHERE source_key=? AND publisher=?`), source.Key(), publisher).Scan(&result.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return result, err
}
func (s *Store) Get(ctx context.Context, source sessionevents.Source, eventID string) (sessionevents.Observation, error) {
	var result sessionevents.Observation
	if source.Validate() != nil || eventID == "" {
		return result, sessionevents.ErrInvalid
	}
	var payload string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_session_observations WHERE source_key=? AND event_id=?`), source.Key(), eventID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return result, sessionevents.ErrNotFound
	}
	if err != nil {
		return result, err
	}
	return result, json.Unmarshal([]byte(payload), &result)
}
func (s *Store) Ingest(ctx context.Context, input sessionevents.Input, expected int64) (sessionevents.Observation, error) {
	if err := input.Validate(); err != nil {
		return sessionevents.Observation{}, err
	}
	if expected < 0 {
		return sessionevents.Observation{}, sessionevents.ErrInvalid
	}
	observation := input.Observation
	observation.ObservedAt = observation.ObservedAt.UTC()
	digest, err := observation.Digest()
	if err != nil {
		return sessionevents.Observation{}, err
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return sessionevents.Observation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessionevents.Observation{}, err
	}
	defer rollback(tx)
	// Serialize writers before reading, including across separate operator
	// processes. SQLite takes a write lock; PostgreSQL locks the shared row.
	if _, err = tx.ExecContext(ctx, `UPDATE oap_session_observation_lock SET generation=generation+1 WHERE id=1`); err != nil {
		return sessionevents.Observation{}, err
	}
	sourceKey := observation.Source.Key()
	var current int64
	err = tx.QueryRowContext(ctx, s.query(`SELECT sequence FROM oap_session_observation_cursors WHERE source_key=? AND publisher=?`), sourceKey, input.Publisher).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return sessionevents.Observation{}, err
	}
	var oldDigest, oldPayload string
	err = tx.QueryRowContext(ctx, s.query(`SELECT digest,payload FROM oap_session_observations WHERE id=?`), observation.ID()).Scan(&oldDigest, &oldPayload)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return sessionevents.Observation{}, err
	}
	if exists && oldDigest != digest {
		return sessionevents.Observation{}, sessionevents.ErrConflict
	}
	if input.Sequence <= current {
		if !exists {
			return sessionevents.Observation{}, sessionevents.ErrConflict
		}
		var stored sessionevents.Observation
		if err = json.Unmarshal([]byte(oldPayload), &stored); err != nil {
			return sessionevents.Observation{}, err
		}
		if err = tx.Commit(); err != nil {
			return sessionevents.Observation{}, err
		}
		return stored, nil
	}
	if current != expected {
		return sessionevents.Observation{}, sessionevents.ErrConflict
	}
	if !exists {
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_session_observations(id,source_key,event_id,digest,payload) VALUES(?,?,?,?,?)`), observation.ID(), sourceKey, observation.EventID, digest, string(payload)); err != nil {
			return sessionevents.Observation{}, err
		}
		var stored sessionevents.Observation
		if err = json.Unmarshal(payload, &stored); err != nil {
			return sessionevents.Observation{}, err
		}
		observation = stored
	} else {
		var stored sessionevents.Observation
		if err = json.Unmarshal([]byte(oldPayload), &stored); err != nil {
			return sessionevents.Observation{}, err
		}
		observation = stored
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_session_observation_cursors(source_key,publisher,sequence) VALUES(?,?,?) ON CONFLICT(source_key,publisher) DO UPDATE SET sequence=excluded.sequence`), sourceKey, input.Publisher, input.Sequence); err != nil {
		return sessionevents.Observation{}, err
	}
	if err = tx.Commit(); err != nil {
		return sessionevents.Observation{}, err
	}
	return observation, nil
}
