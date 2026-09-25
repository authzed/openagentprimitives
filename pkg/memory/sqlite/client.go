package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // registers the CGO-free "sqlite" driver
)

// Client wraps a *sql.DB opened against an on-disk SQLite file in WAL
// mode. WAL permits concurrent readers alongside a single writer;
// busy_timeout absorbs the operator's concurrent Puts from controllers
// and signal hooks.
type Client struct {
	db *sql.DB
}

// NewClient opens (creating if absent) the SQLite database at path.
// The path is used verbatim (no URL-escaping — that would mangle the
// '/' separators); query params carry the pragmas. modernc parses the
// substring before '?' as the filename.
func NewClient(path string) (*Client, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: ping %q: %w", path, err)
	}
	return &Client{db: db}, nil
}

// Migrate creates the entry/link tables and indexes, then applies the
// additive column migrations an already-created database may be missing.
// Safe to call repeatedly.
func (c *Client) Migrate(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, migrateSQL); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	for _, col := range addedColumns {
		if err := c.ensureColumn(ctx, col.table, col.column, col.decl); err != nil {
			return err
		}
	}
	return nil
}

// ensureColumn adds a column to an existing table when the database was
// created by an older schema that lacked it. SQLite has no
// ADD COLUMN IF NOT EXISTS, so the existing columns are probed first —
// rather than running the ALTER unconditionally and swallowing its
// "duplicate column name" error, which would also swallow every other
// reason the ALTER can fail.
func (c *Client) ensureColumn(ctx context.Context, table, column, decl string) error {
	var n int
	if err := c.db.QueryRowContext(ctx,
		"SELECT count(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&n); err != nil {
		return fmt.Errorf("sqlite: migrate: inspect %s.%s: %w", table, column, err)
	}
	if n > 0 {
		return nil
	}
	// table/column/decl are package-level constants, never caller input:
	// SQLite does not accept bound parameters for identifiers.
	if _, err := c.db.ExecContext(ctx,
		"ALTER TABLE "+table+" ADD COLUMN "+column+" "+decl); err != nil {
		return fmt.Errorf("sqlite: migrate: add column %s.%s: %w", table, column, err)
	}
	return nil
}

// DB exposes the underlying handle so the search provider can share it.
func (c *Client) DB() *sql.DB { return c.db }

func (c *Client) Close() error { return c.db.Close() }
