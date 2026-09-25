// Package postgres is the durable postgres skillbundle.Store backend.
// It stores gzipped-tar skill bundle bytes in a single `skill_bundle` table,
// keyed by content digest. Takes a *pgxpool.Pool directly (low coupling — the
// caller is free to share the pool from pkg/memory/postgres.Client.Pool() or
// construct one independently).
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
)

const migrateSQL = `
CREATE TABLE IF NOT EXISTS skill_bundle (
    digest     TEXT        PRIMARY KEY,
    data       BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// Store is the postgres-backed skillbundle.Store.
type Store struct {
	pool *pgxpool.Pool
}

var _ skillbundle.Store = (*Store)(nil)

// New returns a Store backed by the given pool.
// The pool may be shared with other components (e.g. pkg/memory/postgres).
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Migrate creates the skill_bundle table if it does not already exist.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, migrateSQL); err != nil {
		return fmt.Errorf("skillbundle postgres: migrate: %w", err)
	}
	return nil
}

// Put stores data under digest. Content-addressed → idempotent; a second Put
// for the same digest is a no-op (ON CONFLICT DO NOTHING).
func (s *Store) Put(ctx context.Context, digest string, data []byte) error {
	const q = `INSERT INTO skill_bundle (digest, data) VALUES ($1, $2) ON CONFLICT (digest) DO NOTHING`
	if _, err := s.pool.Exec(ctx, q, digest, data); err != nil {
		return fmt.Errorf("skillbundle postgres: put %s: %w", digest, err)
	}
	return nil
}

// Get returns the bundle bytes for the given digest, or skillbundle.ErrNotFound
// when the digest is absent.
func (s *Store) Get(ctx context.Context, digest string) ([]byte, error) {
	const q = `SELECT data FROM skill_bundle WHERE digest = $1`
	row := s.pool.QueryRow(ctx, q, digest)
	var data []byte
	if err := row.Scan(&data); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, skillbundle.ErrNotFound
		}
		return nil, fmt.Errorf("skillbundle postgres: get %s: %w", digest, err)
	}
	return data, nil
}

// Has reports whether the given digest is present in the store.
func (s *Store) Has(ctx context.Context, digest string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM skill_bundle WHERE digest = $1)`
	row := s.pool.QueryRow(ctx, q, digest)
	var exists bool
	if err := row.Scan(&exists); err != nil {
		return false, fmt.Errorf("skillbundle postgres: has %s: %w", digest, err)
	}
	return exists, nil
}
